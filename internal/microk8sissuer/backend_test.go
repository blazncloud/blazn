package microk8sissuer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type fakeRunner struct {
	output []byte
	args   []string
	calls  int
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string) ([]byte, error) {
	f.calls++
	f.args = append([]string(nil), args...)
	return f.output, nil
}
func backendFixture(t *testing.T, content string) (*MicroK8sBackend, *fakeRunner, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster-tokens.txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	runner := &fakeRunner{}
	return &MicroK8sBackend{AddNodePath: "/snap/bin/microk8s.add-node", TokenFile: path, ExpectedUID: uint32(uid), Runner: runner, Now: func() time.Time { return time.Unix(1000, 0).UTC() }, allowTestPaths: true}, runner, path
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
func TestRevokeRemovesOnlyExactTokenAndIsIdempotent(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	backend, _, path := backendFixture(t, token+"|123\nother|456\n")
	if err := backend.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := backend.Revoke(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "other|456\n" {
		t.Fatalf("unexpected token file %q", data)
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
