package microk8sissuer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeBackend struct {
	issues, revokes       int
	failIssue, failRevoke bool
	now                   time.Time
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
func (f *fakeBackend) Healthy(context.Context) error { return nil }
func requestFixture() Request {
	return Request{SchemaVersion: SchemaVersion, Operation: "issue", IssuanceID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ClusterID: "cluster-a", ExpectedNodeName: "worker-a", BootstrapTaint: BootstrapTaint, TTLSeconds: 60, WorkerOnly: true}
}
func TestIssueIsDeterministicAndBindsCredential(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{now: now}
	service, _ := NewService(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"), backend)
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
func TestConflictingIssueFailsAndDoesNotCallBackend(t *testing.T) {
	backend := &fakeBackend{now: time.Now()}
	service, _ := NewService(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"), backend)
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
	service, _ := NewService(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	if _, err := service.Handle(context.Background(), requestFixture()); err == nil {
		t.Fatal("failure accepted")
	}
	backend.failIssue = false
	if _, err := service.Handle(context.Background(), requestFixture()); err != nil {
		t.Fatal(err)
	}
	if backend.revokes != 1 {
		t.Fatal("retry did not revoke prior token")
	}
	req := Request{SchemaVersion: SchemaVersion, Operation: "revoke", ProviderHandle: requestFixture().IssuanceID}
	if _, err := service.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Handle(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if backend.revokes != 2 {
		t.Fatalf("revoke count %d", backend.revokes)
	}
}
func TestBackendFailureDoesNotLeakSecret(t *testing.T) {
	now := time.Now()
	backend := &fakeBackend{now: now, failIssue: true}
	service, _ := NewService(t.TempDir(), []byte("0123456789abcdef0123456789abcdef"), backend)
	service.now = func() time.Time { return now }
	_, err := service.Handle(context.Background(), requestFixture())
	if err == nil || err.Error() == "secret output" {
		t.Fatal("secret backend error leaked")
	}
}
