package microk8sissuer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeRunner struct {
	output        []byte
	observeOutput []byte
	args          []string
	calls         int
	tokenFile     string
	failStatus    bool
}

func (f *fakeRunner) Run(_ context.Context, path string, args []string) ([]byte, error) {
	f.calls++
	f.args = append([]string(nil), args...)
	if f.failStatus && path == "/snap/bin/microk8s.status" {
		return nil, errors.New("unready details")
	}
	if strings.Contains(path, "kubectl") {
		return f.observeOutput, nil
	}
	if len(args) >= 2 && args[0] == "--token" && f.tokenFile != "" {
		file, _ := os.OpenFile(f.tokenFile, os.O_APPEND|os.O_WRONLY, 0)
		if file != nil {
			_, _ = file.WriteString(args[1] + "|0000001060\n")
			_ = file.Close()
		}
	}
	return f.output, nil
}

func TestBackendHealthUsesOnlyTheFixedReadinessProbe(t *testing.T) {
	backend, runner, _ := backendFixture(t, "")
	if err := backend.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.args) != 3 || runner.args[0] != "--wait-ready" || runner.args[1] != "--timeout" || runner.args[2] != "5" {
		t.Fatalf("readiness args %#v", runner.args)
	}
	runner.failStatus = true
	if err := backend.Healthy(context.Background()); err == nil || err.Error() == "unready details" {
		t.Fatal("readiness failed open or leaked output")
	}
}
func backendFixture(t *testing.T, content string) (*MicroK8sBackend, *fakeRunner, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster-tokens.txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	runner := &fakeRunner{}
	runner.tokenFile = path
	return &MicroK8sBackend{AddNodePath: "/snap/bin/microk8s.add-node", StatusPath: "/snap/bin/microk8s.status", KubectlPath: "/snap/bin/microk8s.kubectl", TokenFile: path, ExpectedUID: uint32(uid), ExpectedGID: uint32(os.Getgid()), ExpectedMode: 0600, Runner: runner, Now: func() time.Time { return time.Unix(1000, 0).UTC() }, allowTestPaths: true}, runner, path
}

func TestObserveRequiresExactBootstrapTaintAndWorkerRole(t *testing.T) {
	backend, runner, _ := backendFixture(t, "")
	runner.observeOutput = []byte(`{"metadata":{"name":"worker-a","uid":"uid-a","resourceVersion":"17","labels":{"kubernetes.io/arch":"amd64"}},"spec":{"taints":[{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]},"status":{"ignored":true}}`)
	observed, err := backend.Observe(context.Background(), "worker-a")
	if err != nil || !observed.BootstrapTainted || !observed.WorkerOnly || observed.UID != "uid-a" {
		t.Fatalf("observation=%#v err=%v", observed, err)
	}
	if len(runner.args) != 5 || runner.args[0] != "get" || runner.args[1] != "node" || runner.args[2] != "worker-a" || runner.args[3] != "-o" || runner.args[4] != "json" {
		t.Fatalf("args=%#v", runner.args)
	}
	runner.observeOutput = []byte(`{"metadata":{"name":"worker-a","uid":"uid-a","resourceVersion":"18","labels":{"node-role.kubernetes.io/control-plane":""}},"spec":{"taints":[{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]}}`)
	observed, err = backend.Observe(context.Background(), "worker-a")
	if err != nil || observed.WorkerOnly {
		t.Fatalf("control-plane observation=%#v err=%v", observed, err)
	}
}
func TestBackendUsesOnlyFixedCommandAndParsesExactJSON(t *testing.T) {
	backend, runner, _ := backendFixture(t, "")
	token := "0123456789abcdef0123456789abcdef"
	runner.output = []byte(fmt.Sprintf(`{"token":%q,"urls":[%q]}`, token+"/check-value-123456", "10.0.0.1:25000/"+token+"/check-value-123456"))
	issued, err := backend.Issue(context.Background(), token, 60)
	if err != nil {
		t.Fatal(err)
	}
	if issued.ExpiresAt.Unix() != 1060 || len(runner.args) != 6 || runner.args[0] != "--token" || runner.args[1] != token || runner.args[4] != "--format" || runner.args[5] != "json" {
		t.Fatalf("unexpected fixed invocation %#v", runner.args)
	}
}
func TestBackendRejectsUnexpectedJSONAndURL(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	cases := []string{fmt.Sprintf(`{"token":%q,"urls":[%q],"extra":true}`, token+"/check-value-123456", "10.0.0.1:25000/"+token+"/check-value-123456"), fmt.Sprintf(`{"token":%q,"urls":[%q]}`, token+"/check-value-123456", "evil.example:25000/"+token+"/check-value-123456")}
	for _, output := range cases {
		backend, runner, _ := backendFixture(t, "")
		runner.output = []byte(output)
		if _, err := backend.Issue(context.Background(), token, 60); err == nil {
			t.Fatal("invalid upstream accepted")
		}
	}
}

func TestBackendRejectsDuplicateURLs(t *testing.T) {
	backend, runner, _ := backendFixture(t, "")
	token := "0123456789abcdef0123456789abcdef"
	join := "10.0.0.1:25000/" + token + "/check-value-123456"
	runner.output = []byte(fmt.Sprintf(`{"token":%q,"urls":[%q,%q]}`, token+"/check-value-123456", join, join))
	if _, err := backend.Issue(context.Background(), token, 60); err == nil {
		t.Fatal("duplicate URL accepted")
	}
}
func TestIssueSyncFailureDoesNotReturnCredential(t *testing.T) {
	backend, runner, _ := backendFixture(t, "")
	token := "0123456789abcdef0123456789abcdef"
	runner.output = []byte(fmt.Sprintf(`{"token":%q,"urls":[%q]}`, token+"/check-value-123456", "10.0.0.1:25000/"+token+"/check-value-123456"))
	backend.syncTokenFile = func(*os.File) error { return errors.New("injected sync failure") }
	if _, err := backend.Issue(context.Background(), token, 60); err == nil {
		t.Fatal("unsynced token was returned")
	}
}
func TestRevokeExpiresOnlyExactTokenAndPreservesPermanentAdminToken(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	other := "fedcba9876543210fedcba9876543210"
	backend, _, path := backendFixture(t, token+"|0000001060\n"+other+"\n")
	if err := backend.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := backend.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != token+"|0000000001\n"+other+"\n" {
		t.Fatalf("unexpected token file %q", data)
	}
}

func TestRevokeMergesAConcurrentExternalAppend(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	other := "fedcba9876543210fedcba9876543210"
	backend, _, path := backendFixture(t, token+"|0000001060\n")
	backend.beforeTokenExpiryWrite = func() {
		file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if file != nil {
			_, _ = file.WriteString(other + "|0000001061\n")
			_ = file.Close()
		}
	}
	if err := backend.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != token+"|0000000001\n"+other+"|0000001061\n" {
		t.Fatalf("concurrent token was lost: %q", data)
	}
}

func TestRevokeRequiresCompleteWriteAndDurableSync(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	backend, _, path := backendFixture(t, token+"|0000001060\n")
	backend.writeTokenExpiry = func(*os.File, []byte, int64) (int, error) { return 5, nil }
	if err := backend.Revoke(context.Background(), token); err == nil {
		t.Fatal("partial expiry write reported success")
	}
	data, _ := os.ReadFile(path)
	if string(data) != token+"|0000001060\n" {
		t.Fatal("partial write fixture changed unexpectedly")
	}
	backend.writeTokenExpiry = nil
	backend.syncTokenFile = func(*os.File) error { return errors.New("injected sync failure") }
	if err := backend.Revoke(context.Background(), token); err == nil {
		t.Fatal("sync failure reported success")
	}
	backend.syncTokenFile = nil
	if err := backend.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerTokenCannotBePermanent(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	backend, _, _ := backendFixture(t, token+"\n")
	if err := backend.Revoke(context.Background(), token); err == nil {
		t.Fatal("permanent broker token accepted")
	}
}

func TestTokenFileRejectsMalformedSuffixAndEpochRange(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	for _, content := range []string{token + "|anything\n", token + "|0000000000\n", token + "|9999999999\n", token + "|123\n"} {
		backend, _, _ := backendFixture(t, content)
		if err := backend.Revoke(context.Background(), token); err == nil {
			t.Fatalf("malformed token accepted: %q", content)
		}
	}
}
func TestUnsafeAndAmbiguousTokenFilesFailClosed(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	backend, runner, path := backendFixture(t, token+"evil|1\n")
	runner.output = []byte(`{}`)
	if _, err := backend.Issue(context.Background(), token, 60); err == nil {
		t.Fatal("ambiguous token accepted")
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if err := backend.Revoke(context.Background(), token); err == nil {
		t.Fatal("unsafe token file accepted")
	}
	_ = syscall.Unlink(path)
}
