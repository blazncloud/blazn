package microk8sissuer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeBackend struct {
	issues, revokes       int
	failIssue, failRevoke bool
	now                   time.Time
	failHealthy           bool
	observation           NodeObservation
	failObserve           bool
}

func (f *fakeBackend) Observe(_ context.Context, name string) (NodeObservation, error) {
	if f.failObserve {
		return NodeObservation{}, errors.New("private observation detail")
	}
	if f.observation.Name == "" {
		return NodeObservation{Name: name, UID: "uid-a", ResourceVersion: "17", BootstrapTainted: true, WorkerOnly: true}, nil
	}
	return f.observation, nil
}

func (f *fakeBackend) Issue(_ context.Context, token string, ttl int) (BackendIssue, error) {
	f.issues++
	if f.failIssue {
		return BackendIssue{}, errors.New("secret output")
	}
	return BackendIssue{TokenCheck: token + "/check-value-123456", URLs: []string{"10.0.0.2:25000/" + token + "/check-value-123456"}, ExpiresAt: f.now.Add(time.Duration(ttl) * time.Second)}, nil
}
func (f *fakeBackend) Revoke(context.Context, string) error {
	f.revokes++
	if f.failRevoke {
		return errors.New("secret")
	}
	return nil
}
func (f *fakeBackend) Healthy(context.Context) error {
	if f.failHealthy {
		return errors.New("unhealthy detail")
	}
	return nil
}
func secureTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}
func requestFixture() Request {
	return Request{SchemaVersion: SchemaVersion, Operation: "issue", IssuanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ClusterID: "cluster-a", ExpectedNodeName: "worker-a", BootstrapTaint: BootstrapTaint, TTLSeconds: 60, WorkerOnly: true}
}
func observeFixture() Request {
	return Request{SchemaVersion: SchemaVersion, Operation: "observe", IssuanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ClusterID: "cluster-a", ExpectedNodeName: "worker-a", BootstrapTaint: BootstrapTaint}
}
func TestIssueIsDeterministicAndBindsCredential(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{now: now}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	first, err := service.Handle(context.Background(), requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Handle(context.Background(), requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	if backend.issues != 1 || first.(IssueResponse).Credential != second.(IssueResponse).Credential {
		t.Fatal("issue was not idempotent")
	}
	raw, err := base64.RawURLEncoding.DecodeString(first.(IssueResponse).Credential)
	if err != nil {
		t.Fatal(err)
	}
	var payload credentialPayload
	if json.Unmarshal(raw, &payload) != nil || payload.ExpectedNodeName != "worker-a" || payload.BootstrapTaint != BootstrapTaint || !payload.WorkerOnly {
		t.Fatal("credential omitted binding")
	}
}
func TestObserveRequiresIssuedBindingAndEnforcesWorkerQuarantine(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{now: now}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	if _, err := service.Handle(context.Background(), observeFixture()); err == nil {
		t.Fatal("observation without issued binding passed")
	}
	if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
		t.Fatal(err)
	}
	result, err := service.Handle(context.Background(), observeFixture())
	if err != nil {
		t.Fatal(err)
	}
	observed := result.(ObserveResponse)
	if observed.NodeUID != "uid-a" || observed.ResourceVersion != "17" || !observed.BootstrapTainted || !observed.WorkerOnly {
		t.Fatalf("observation=%#v", observed)
	}
	backend.observation = NodeObservation{Name: "worker-a", UID: "uid-a", ResourceVersion: "18", WorkerOnly: true}
	if _, err := service.Handle(context.Background(), observeFixture()); err == nil {
		t.Fatal("untainted worker observation passed")
	}
	backend.observation = NodeObservation{Name: "worker-a", UID: "uid-a", ResourceVersion: "19", BootstrapTainted: true}
	if _, err := service.Handle(context.Background(), observeFixture()); err == nil {
		t.Fatal("control-plane observation passed")
	}
}
func TestConflictingIssueFailsAndDoesNotCallBackend(t *testing.T) {
	backend := &fakeBackend{now: time.Now()}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return backend.now }
	if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
		t.Fatal(err)
	}
	changed := requestFixture()
	changed.ExpectedNodeName = "worker-b"
	if _, err := service.Handle(context.Background(), changed); err == nil {
		t.Fatal("conflict accepted")
	}
	if backend.issues != 1 {
		t.Fatal("backend called for conflict")
	}
}
func TestCrashStateRevokesBeforeRetryAndRevokeIsIdempotent(t *testing.T) {
	now := time.Now()
	backend := &fakeBackend{now: now, failIssue: true}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	if _, err := service.Handle(context.Background(), requestFixture()); err == nil {
		t.Fatal("failure accepted")
	}
	backend.failIssue = false
	if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
		t.Fatal(err)
	}
	if backend.revokes != 2 {
		t.Fatal("retry did not revoke prior token")
	}
	req := Request{SchemaVersion: SchemaVersion, Operation: "revoke", ProviderHandle: requestFixture().IssuanceID}
	if _, err := service.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if backend.revokes != 3 {
		t.Fatalf("revoke count %d", backend.revokes)
	}
}
func TestBackendFailureDoesNotLeakSecret(t *testing.T) {
	now := time.Now()
	backend := &fakeBackend{now: now, failIssue: true}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	_, err := service.Handle(context.Background(), requestFixture())
	if err == nil || err.Error() == "secret output" {
		t.Fatal("secret backend error leaked")
	}
}

func TestDurableTokenCollisionFailsBeforeBackend(t *testing.T) {
	root := secureTempDir(t)
	backend := &fakeBackend{now: time.Now()}
	service, _ := NewService(root, []byte("0123456789abcdef0123456789abcdef"), backend)
	req := requestFixture()
	collision := durableState{SchemaVersion: SchemaVersion, Status: "revoked", Request: req, RequestDigest: "other", TokenHash: hash(service.token(req))}
	data, _ := json.Marshal(collision)
	if err := os.WriteFile(filepath.Join(root, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Handle(context.Background(), req); err == nil {
		t.Fatal("durable collision accepted")
	}
	if backend.issues != 0 {
		t.Fatal("backend called for collision")
	}
}

func TestLockWaitHonorsDeadline(t *testing.T) {
	root := secureTempDir(t)
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	service, _ := NewService(root, []byte("0123456789abcdef0123456789abcdef"), &fakeBackend{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := service.Handle(ctx, requestFixture()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestIssuedReplayRejectsEveryDurableBindingCorruption(t *testing.T) {
	mutations := []func(*durableState){
		func(state *durableState) { state.Status = "unknown" },
		func(state *durableState) { state.RequestDigest = strings.Repeat("0", 64) },
		func(state *durableState) { state.TokenHash = strings.Repeat("0", 64) },
		func(state *durableState) { state.ExpiresAt = state.ExpiresAt.Add(time.Second) },
		func(state *durableState) {
			data, _ := base64.RawURLEncoding.DecodeString(state.Credential)
			var payload credentialPayload
			_ = json.Unmarshal(data, &payload)
			payload.ClusterID = "other"
			changed, _ := json.Marshal(payload)
			state.Credential = base64.RawURLEncoding.EncodeToString(changed)
		},
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprintf("corruption-%d", index), func(t *testing.T) {
			now := time.Now().UTC()
			backend := &fakeBackend{now: now}
			root := secureTempDir(t)
			service, _ := NewService(root, []byte("0123456789abcdef0123456789abcdef"), backend)
			service.now = func() time.Time { return now }
			if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
				t.Fatal(err)
			}
			path := service.statePath(requestFixture().IssuanceID)
			data, _ := os.ReadFile(path)
			var state durableState
			_ = json.Unmarshal(data, &state)
			mutate(&state)
			changed, _ := json.Marshal(state)
			if err := os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Handle(context.Background(), requestFixture()); err == nil {
				t.Fatal("corrupt issued replay accepted")
			}
			if backend.issues != 1 {
				t.Fatal("corruption triggered new issuance")
			}
		})
	}
}

func TestExpiredIssuedStateIsRevokedBeforeReissue(t *testing.T) {
	now := time.Now().UTC()
	backend := &fakeBackend{now: now}
	service, _ := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	backend.now = now
	if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
		t.Fatal(err)
	}
	if backend.issues != 2 || backend.revokes != 1 {
		t.Fatalf("issues=%d revokes=%d", backend.issues, backend.revokes)
	}
}
