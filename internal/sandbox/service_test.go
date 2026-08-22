package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

type staticTokens struct {
	value string
	err   error
}

func (s staticTokens) AccessToken(context.Context) (string, error) { return s.value, s.err }

type fakeAPI struct {
	grant                    client.SandboxAccessGrantCreated
	exec                     client.SandboxExecResult
	execErr                  error
	grantCalls               int
	uploadPath, uploadDigest string
	uploadSize               int64
	uploadBody               []byte
	downloadBody             []byte
	downloadSize             int64
	downloadDigest           string
	streams                  []EventStream
	cursors                  []string
	current                  client.Sandbox
	operation                client.CreateSandboxOperationRequest
}

type sliceStream struct {
	events []client.SandboxEvent
	err    error
	closed bool
}

func (s *sliceStream) Next() (client.SandboxEvent, error) {
	if len(s.events) == 0 {
		if s.err != nil {
			err := s.err
			s.err = nil
			return client.SandboxEvent{}, err
		}
		return client.SandboxEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *sliceStream) Close() error { s.closed = true; return nil }

func (f *fakeAPI) CreateSandboxTemplate(context.Context, string, string, string, client.SandboxManifest) (client.SandboxTemplateEnvelope, error) {
	return client.SandboxTemplateEnvelope{}, nil
}
func (f *fakeAPI) ListSandboxTemplates(context.Context, string, string, string) (client.SandboxTemplateList, error) {
	return client.SandboxTemplateList{}, nil
}
func (f *fakeAPI) ReplaceSandboxTemplateDraft(context.Context, string, string, string, client.ReplaceSandboxTemplateDraftRequest) (client.SandboxTemplateEnvelope, error) {
	return client.SandboxTemplateEnvelope{}, nil
}
func (f *fakeAPI) PublishSandboxTemplateVersion(context.Context, string, string, string, client.PublishSandboxTemplateVersionRequest) (client.SandboxTemplateVersionEnvelope, error) {
	return client.SandboxTemplateVersionEnvelope{}, nil
}
func (f *fakeAPI) CreateSandbox(context.Context, string, string, string, client.CreateSandboxRequest) (client.SandboxMutation, error) {
	return client.SandboxMutation{}, nil
}
func (f *fakeAPI) ListSandboxes(context.Context, string, string, string) (client.SandboxList, error) {
	return client.SandboxList{}, nil
}
func (f *fakeAPI) GetSandbox(context.Context, string, string) (client.Sandbox, error) {
	return f.current, nil
}
func (f *fakeAPI) CreateSandboxOperation(_ context.Context, _ string, _ string, _ string, r client.CreateSandboxOperationRequest) (client.SandboxMutation, error) {
	f.operation = r
	return client.SandboxMutation{}, nil
}
func (f *fakeAPI) CreateSandboxAccessGrant(context.Context, string, string, client.CreateSandboxAccessGrantRequest) (client.SandboxAccessGrantCreated, error) {
	f.grantCalls++
	return f.grant, nil
}
func (f *fakeAPI) StreamSandboxEvents(_ context.Context, _ string, _ string, cursor string) (EventStream, error) {
	f.cursors = append(f.cursors, cursor)
	if len(f.streams) == 0 {
		return nil, errors.New("offline")
	}
	s := f.streams[0]
	f.streams = f.streams[1:]
	return s, nil
}
func (f *fakeAPI) ExecuteSandboxGrant(context.Context, string, string, client.SandboxExecRequest) (client.SandboxExecResult, error) {
	return f.exec, f.execErr
}
func (f *fakeAPI) UploadSandboxGrantFile(_ context.Context, _, _, path, digest string, r io.Reader, size int64) (client.SandboxFileTransferResult, error) {
	f.uploadPath, f.uploadDigest, f.uploadSize = path, digest, size
	f.uploadBody, _ = io.ReadAll(r)
	return client.SandboxFileTransferResult{Path: path, Size: size, SHA256: digest}, nil
}
func (f *fakeAPI) DownloadSandboxGrantFile(context.Context, string, string, string) (io.ReadCloser, int64, string, error) {
	return io.NopCloser(bytes.NewReader(f.downloadBody)), f.downloadSize, f.downloadDigest, nil
}

func newFakeService(api *fakeAPI) *Service {
	return NewService(api, staticTokens{value: "session-secret"})
}
func grant(kind client.SandboxGrantKind) client.SandboxAccessGrantCreated {
	return client.SandboxAccessGrantCreated{Grant: client.SandboxAccessGrant{ID: "22222222-2222-4222-8222-222222222222", Kind: kind}, AccessToken: "grant-secret", Endpoint: "https://example.test/grant"}
}

func TestExecKeepsGrantInMemoryAndTruncationIsPartial(t *testing.T) {
	api := &fakeAPI{grant: grant(client.SandboxGrantExec), exec: client.SandboxExecResult{RemoteExitCode: 23, StdoutBase64: "b2s=", Truncated: true}}
	result, err := newFakeService(api).Exec(context.Background(), "11111111-1111-4111-8111-111111111111", []string{"false"})
	if !IsPartial(err) || result.RemoteExitCode != 23 || !result.Truncated || strings.Contains(string(mustJSON(t, result)), "secret") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestUploadRejectsSymlinkAndTransfersExactDigest(t *testing.T) {
	dir := t.TempDir()
	source := dir + "/source"
	link := dir + "/link"
	contents := []byte("exact bytes")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{grant: grant(client.SandboxGrantUpload)}
	service := newFakeService(api)
	if _, err := service.Upload(context.Background(), "id", link, "/workspace/tmp/file"); err == nil || api.grantCalls != 0 {
		t.Fatalf("err=%v grants=%d", err, api.grantCalls)
	}
	result, err := service.Upload(context.Background(), "id", source, "/workspace/tmp/file")
	sum := sha256.Sum256(contents)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if err != nil || result.SHA256 != want || api.uploadDigest != want || api.uploadSize != int64(len(contents)) || !bytes.Equal(api.uploadBody, contents) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestUploadRejectsOverEightMiBBeforeGrant(t *testing.T) {
	path := t.TempDir() + "/large"
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(client.SandboxMaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	api := &fakeAPI{grant: grant(client.SandboxGrantUpload)}
	if _, err := newFakeService(api).Upload(context.Background(), "id", path, "/workspace/tmp/file"); err == nil || api.grantCalls != 0 {
		t.Fatalf("err=%v grants=%d", err, api.grantCalls)
	}
}

func TestDownloadVerifiesBeforeReplacingDestination(t *testing.T) {
	dir := t.TempDir()
	destination := dir + "/out"
	contents := []byte("download")
	sum := sha256.Sum256(contents)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	api := &fakeAPI{grant: grant(client.SandboxGrantDownload), downloadBody: contents, downloadSize: int64(len(contents)), downloadDigest: digest}
	result, err := newFakeService(api).Download(context.Background(), "id", "/workspace/artifacts/out", destination)
	got, _ := os.ReadFile(destination)
	if err != nil || result.SHA256 != digest || !bytes.Equal(got, contents) {
		t.Fatalf("result=%+v bytes=%q err=%v", result, got, err)
	}
	link := dir + "/link"
	if err := os.Symlink(destination, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newFakeService(api).Download(context.Background(), "id", "/workspace/artifacts/out", link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestStopBindsCurrentVersion(t *testing.T) {
	api := &fakeAPI{current: client.Sandbox{Version: 19}}
	if _, err := newFakeService(api).Stop(context.Background(), "id", "request-123"); err != nil {
		t.Fatal(err)
	}
	if api.operation.Type != client.SandboxOperationStop || api.operation.ExpectedVersion != 19 {
		t.Fatalf("operation=%+v", api.operation)
	}
}

func TestWatchReconnectsWithCursorAndEmitsOnce(t *testing.T) {
	first := &sliceStream{events: []client.SandboxEvent{{EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 4, Type: "sandbox.provisioning", Payload: map[string]any{"state": "provisioning"}}}}
	second := &sliceStream{events: []client.SandboxEvent{{EventID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 5, Type: "sandbox.ready", Payload: map[string]any{"state": "ready"}}}}
	api := &fakeAPI{streams: []EventStream{first, second}}
	service := newFakeService(api)
	service.reconnect = 0
	var ids []string
	terminal, err := service.Watch(context.Background(), "11111111-1111-4111-8111-111111111111", "", func(event client.SandboxEvent) error { ids = append(ids, event.EventID); return nil })
	if err != nil || terminal != WatchReady || len(ids) != 2 || len(api.cursors) != 2 || api.cursors[1] != ids[0] || !first.closed || !second.closed {
		t.Fatalf("terminal=%q ids=%v cursors=%v err=%v", terminal, ids, api.cursors, err)
	}
}

func TestWatchRejectsNonMonotonicSequence(t *testing.T) {
	stream := &sliceStream{events: []client.SandboxEvent{
		{EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 7, Type: "sandbox.running", Payload: map[string]any{}},
		{EventID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 6, Type: "sandbox.ready", Payload: map[string]any{}},
	}}
	api := &fakeAPI{streams: []EventStream{stream}}
	service := newFakeService(api)
	service.reconnect = 0
	if _, err := service.Watch(context.Background(), "11111111-1111-4111-8111-111111111111", "", func(client.SandboxEvent) error { return nil }); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("err=%v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
