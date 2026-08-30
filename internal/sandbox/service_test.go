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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type staticTokens struct {
	value string
	err   error
	calls []bool
}

func (s *staticTokens) AccessToken(_ context.Context, force bool) (string, error) {
	s.calls = append(s.calls, force)
	return s.value, s.err
}

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
	streamErrors             []error
	cursors                  []string
	current                  client.Sandbox
	operation                client.CreateSandboxOperationRequest
	listErrors               []error
	listTokens               []string
	execCalls                int
	grantErrors              []error
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
func (f *fakeAPI) ListSandboxes(_ context.Context, token, _, _ string) (client.SandboxList, error) {
	f.listTokens = append(f.listTokens, token)
	if len(f.listErrors) > 0 {
		err := f.listErrors[0]
		f.listErrors = f.listErrors[1:]
		return client.SandboxList{}, err
	}
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
	if len(f.grantErrors) > 0 {
		err := f.grantErrors[0]
		f.grantErrors = f.grantErrors[1:]
		if err != nil {
			return client.SandboxAccessGrantCreated{}, err
		}
	}
	return f.grant, nil
}
func (f *fakeAPI) StreamSandboxEvents(_ context.Context, _ string, _ string, cursor string) (EventStream, error) {
	f.cursors = append(f.cursors, cursor)
	if len(f.streamErrors) > 0 {
		err := f.streamErrors[0]
		f.streamErrors = f.streamErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.streams) == 0 {
		return nil, errors.New("offline")
	}
	s := f.streams[0]
	f.streams = f.streams[1:]
	return s, nil
}
func (f *fakeAPI) ExecuteSandboxGrant(context.Context, string, string, client.SandboxExecRequest) (client.SandboxExecResult, error) {
	f.execCalls++
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
	return NewService(api, &staticTokens{value: "session-secret"})
}
func grant(kind client.SandboxGrantKind, sandboxID string) client.SandboxAccessGrantCreated {
	now := time.Now().UTC()
	scope := map[client.SandboxGrantKind]string{client.SandboxGrantExec: "sandbox.exec", client.SandboxGrantUpload: "sandbox.upload", client.SandboxGrantDownload: "sandbox.download"}[kind]
	return client.SandboxAccessGrantCreated{Grant: client.SandboxAccessGrant{ID: "22222222-2222-4222-8222-222222222222", SandboxID: sandboxID, WorkspaceID: "33333333-3333-4333-8333-333333333333", Scope: scope, Kind: kind, State: client.SandboxGrantActive, CreatedAt: now.Add(-time.Second).Format(time.RFC3339Nano), ExpiresAt: now.Add(30 * time.Second).Format(time.RFC3339Nano)}, AccessToken: strings.Repeat("g", 43), Endpoint: "https://blazn.benpelo.com/v1/sandbox-access-grants/22222222-2222-4222-8222-222222222222"}
}

func TestExecKeepsGrantInMemoryAndTruncationIsPartial(t *testing.T) {
	api := &fakeAPI{grant: grant(client.SandboxGrantExec, "11111111-1111-4111-8111-111111111111"), exec: client.SandboxExecResult{RemoteExitCode: 23, StdoutBase64: "b2s=", Truncated: true}}
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
	api := &fakeAPI{grant: grant(client.SandboxGrantUpload, "11111111-1111-4111-8111-111111111111")}
	service := newFakeService(api)
	if _, err := service.Upload(context.Background(), "11111111-1111-4111-8111-111111111111", link, "/workspace/tmp/file"); err == nil || api.grantCalls != 0 {
		t.Fatalf("err=%v grants=%d", err, api.grantCalls)
	}
	result, err := service.Upload(context.Background(), "11111111-1111-4111-8111-111111111111", source, "/workspace/tmp/file")
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
	api := &fakeAPI{grant: grant(client.SandboxGrantUpload, "11111111-1111-4111-8111-111111111111")}
	if _, err := newFakeService(api).Upload(context.Background(), "11111111-1111-4111-8111-111111111111", path, "/workspace/tmp/file"); err == nil || api.grantCalls != 0 {
		t.Fatalf("err=%v grants=%d", err, api.grantCalls)
	}
}

func TestDownloadVerifiesBeforeCreatingDestination(t *testing.T) {
	dir := t.TempDir()
	destination := dir + "/out"
	contents := []byte("download")
	sum := sha256.Sum256(contents)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	api := &fakeAPI{grant: grant(client.SandboxGrantDownload, "11111111-1111-4111-8111-111111111111"), downloadBody: contents, downloadSize: int64(len(contents)), downloadDigest: digest}
	result, err := newFakeService(api).Download(context.Background(), "11111111-1111-4111-8111-111111111111", "/workspace/artifacts/out", destination)
	got, _ := os.ReadFile(destination)
	if err != nil || result.SHA256 != digest || !bytes.Equal(got, contents) {
		t.Fatalf("result=%+v bytes=%q err=%v", result, got, err)
	}
	grants := api.grantCalls
	if _, err := newFakeService(api).Download(context.Background(), "11111111-1111-4111-8111-111111111111", "/workspace/artifacts/out", destination); err == nil || !strings.Contains(err.Error(), "already exists") || api.grantCalls != grants {
		t.Fatalf("existing destination err=%v grants=%d", err, api.grantCalls)
	}
	link := dir + "/link"
	if err := os.Symlink(destination, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newFakeService(api).Download(context.Background(), "11111111-1111-4111-8111-111111111111", "/workspace/artifacts/out", link); err == nil || !strings.Contains(err.Error(), "already exists") || api.grantCalls != grants {
		t.Fatalf("symlink destination err=%v grants=%d", err, api.grantCalls)
	}
}

func TestDownloadMismatchLeavesDestinationAbsent(t *testing.T) {
	destination := t.TempDir() + "/out"
	contents := []byte("tampered")
	sum := sha256.Sum256([]byte("expected"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	api := &fakeAPI{grant: grant(client.SandboxGrantDownload, "11111111-1111-4111-8111-111111111111"), downloadBody: contents, downloadSize: int64(len(contents)), downloadDigest: digest}
	if _, err := newFakeService(api).Download(context.Background(), "11111111-1111-4111-8111-111111111111", "/workspace/artifacts/out", destination); err == nil {
		t.Fatal("expected mismatch")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination exists after mismatch: %v", err)
	}
	matches, err := filepath.Glob(filepath.Dir(destination) + "/.blazn-download-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary downloads=%v err=%v", matches, err)
	}
}

func TestWriteVerifiedFileConcurrentDownloadsDoNotClobber(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "out")
	contents := [][]byte{[]byte("first download"), []byte("second download")}
	errorsByDownload := make(chan error, len(contents))
	start := make(chan struct{})
	for _, body := range contents {
		body := body
		go func() {
			<-start
			sum := sha256.Sum256(body)
			errorsByDownload <- writeVerifiedFile(destination, bytes.NewReader(body), int64(len(body)), "sha256:"+hex.EncodeToString(sum[:]))
		}()
	}
	close(start)
	succeeded, existed := 0, 0
	for range contents {
		err := <-errorsByDownload
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "already exists"):
			existed++
		default:
			t.Fatalf("unexpected concurrent download error: %v", err)
		}
	}
	if succeeded != 1 || existed != 1 {
		t.Fatalf("succeeded=%d existed=%d", succeeded, existed)
	}
	got, err := os.ReadFile(destination)
	if err != nil || (!bytes.Equal(got, contents[0]) && !bytes.Equal(got, contents[1])) {
		t.Fatalf("destination=%q err=%v", got, err)
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".blazn-download-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary downloads=%v err=%v", matches, err)
	}
}

func TestInvalidRemotePathCreatesNoGrant(t *testing.T) {
	api := &fakeAPI{grant: grant(client.SandboxGrantDownload, "11111111-1111-4111-8111-111111111111")}
	service := newFakeService(api)
	for _, path := range []string{"/workspace/tmp/../secret", "/workspace/tmp/%2e%2e", "/workspace/tmp/é", "/workspace/tmp/a\x00b", strings.Repeat("a", 513)} {
		if _, err := service.Download(context.Background(), "11111111-1111-4111-8111-111111111111", path, t.TempDir()+"/out"); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if _, err := service.Upload(context.Background(), "11111111-1111-4111-8111-111111111111", "does-not-exist", "/workspace/tmp/%2e%2e"); err == nil {
		t.Fatal("invalid upload path accepted")
	}
	if api.grantCalls != 0 {
		t.Fatalf("grant calls=%d", api.grantCalls)
	}
}

func TestAccessExpiredRefreshesOnceAndGrantIsNeverReused(t *testing.T) {
	expired := &client.APIError{StatusCode: 401, Body: client.ErrorBody{Code: "access_expired", Message: "expired", RequestID: "r"}}
	tokens := &staticTokens{value: "token"}
	api := &fakeAPI{listErrors: []error{expired, nil}}
	service := NewService(api, tokens)
	if _, err := service.List(context.Background(), "workspace", ""); err != nil {
		t.Fatal(err)
	}
	if len(tokens.calls) != 2 || tokens.calls[0] || !tokens.calls[1] || len(api.listTokens) != 2 {
		t.Fatalf("refresh calls=%v api=%v", tokens.calls, api.listTokens)
	}
	tokens = &staticTokens{value: "token"}
	api = &fakeAPI{grant: grant(client.SandboxGrantExec, "11111111-1111-4111-8111-111111111111"), grantErrors: []error{expired, nil}, exec: client.SandboxExecResult{RemoteExitCode: 0, StdoutBase64: "", StderrBase64: ""}}
	service = NewService(api, tokens)
	if _, err := service.Exec(context.Background(), "11111111-1111-4111-8111-111111111111", []string{"true"}); err != nil {
		t.Fatal(err)
	}
	if api.grantCalls != 2 || api.execCalls != 1 || len(tokens.calls) != 2 || tokens.calls[0] || !tokens.calls[1] {
		t.Fatalf("grant=%d exec=%d refresh=%v", api.grantCalls, api.execCalls, tokens.calls)
	}
	api = &fakeAPI{grant: grant(client.SandboxGrantExec, "11111111-1111-4111-8111-111111111111"), execErr: expired}
	service = NewService(api, &staticTokens{value: "token"})
	if _, err := service.Exec(context.Background(), "11111111-1111-4111-8111-111111111111", []string{"true"}); !IsPartial(err) {
		t.Fatalf("err=%v", err)
	}
	if api.grantCalls != 1 || api.execCalls != 1 {
		t.Fatalf("grant=%d exec=%d", api.grantCalls, api.execCalls)
	}
}

func TestMalformedGrantAndExecResponsesFailClosed(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	badGrant := grant(client.SandboxGrantExec, id)
	badGrant.Grant.Kind = client.SandboxGrantUpload
	api := &fakeAPI{grant: badGrant}
	if _, err := newFakeService(api).Exec(context.Background(), id, []string{"true"}); err == nil || api.execCalls != 0 {
		t.Fatalf("err=%v exec=%d", err, api.execCalls)
	}
	api = &fakeAPI{grant: grant(client.SandboxGrantExec, id), exec: client.SandboxExecResult{RemoteExitCode: 999, StdoutBase64: "%%%"}}
	result, err := newFakeService(api).Exec(context.Background(), id, []string{"true"})
	if !IsPartial(err) || !result.Truncated || result.StdoutBase64 != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	api = &fakeAPI{grant: grant(client.SandboxGrantExec, id), execErr: errors.New("transport failed")}
	result, err = newFakeService(api).Exec(context.Background(), id, []string{"true"})
	if !IsPartial(err) || !result.Truncated || result.GrantID == "" {
		t.Fatalf("transport partial result=%#v err=%v", result, err)
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
	first := &sliceStream{events: []client.SandboxEvent{{EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 4, Type: "sandbox.provisioning", Payload: map[string]any{"state": "provisioning"}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}}}
	second := &sliceStream{events: []client.SandboxEvent{{EventID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 5, Type: "sandbox.ready", Payload: map[string]any{"state": "ready"}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}}}
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
		{EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 7, Type: "sandbox.running", Payload: map[string]any{}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		{EventID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", SandboxID: "11111111-1111-4111-8111-111111111111", Sequence: 6, Type: "sandbox.ready", Payload: map[string]any{}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
	}}
	api := &fakeAPI{streams: []EventStream{stream}}
	service := newFakeService(api)
	service.reconnect = 0
	if _, err := service.Watch(context.Background(), "11111111-1111-4111-8111-111111111111", "", func(client.SandboxEvent) error { return nil }); err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("err=%v", err)
	}
}

func TestWatchBoundsCleanEOFAndDuplicateOnlyReconnects(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	event := client.SandboxEvent{EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SandboxID: id, Sequence: 1, Type: "sandbox.running", Payload: map[string]any{}, CreatedAt: now}
	api := &fakeAPI{streams: []EventStream{&sliceStream{events: []client.SandboxEvent{event}}, &sliceStream{events: []client.SandboxEvent{event}}, &sliceStream{events: []client.SandboxEvent{event}}}}
	service := newFakeService(api)
	service.reconnect = 0
	service.maxErrors = 3
	emitted := 0
	_, err := service.Watch(context.Background(), id, "", func(client.SandboxEvent) error { emitted++; return nil })
	if !IsUnavailable(err) || emitted != 1 || len(api.cursors) != 3 {
		t.Fatalf("err=%v emitted=%d cursors=%v", err, emitted, api.cursors)
	}
}

func TestWatchRefreshesExpiredAccessOnReopen(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	expired := &client.APIError{StatusCode: 401, Body: client.ErrorBody{Code: "access_expired"}}
	event := client.SandboxEvent{EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", SandboxID: id, Sequence: 1, Type: "sandbox.ready", Payload: map[string]any{"state": "ready"}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	api := &fakeAPI{streamErrors: []error{expired, nil}, streams: []EventStream{&sliceStream{events: []client.SandboxEvent{event}}}}
	tokens := &staticTokens{value: "token"}
	service := NewService(api, tokens)
	terminal, err := service.Watch(context.Background(), id, "", func(client.SandboxEvent) error { return nil })
	if err != nil || terminal != WatchReady || len(tokens.calls) != 2 || tokens.calls[0] || !tokens.calls[1] {
		t.Fatalf("terminal=%q err=%v refresh=%v", terminal, err, tokens.calls)
	}
}

func TestWatchRejectsMalformedEventBeforeEmitAndCancellationIsNotUnavailable(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	api := &fakeAPI{streams: []EventStream{&sliceStream{events: []client.SandboxEvent{{EventID: "bad", SandboxID: id, Sequence: -1, Type: "BAD", Payload: nil, CreatedAt: "bad"}}}}}
	service := newFakeService(api)
	called := false
	if _, err := service.Watch(context.Background(), id, "", func(client.SandboxEvent) error { called = true; return nil }); err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Watch(ctx, id, "", func(client.SandboxEvent) error { return nil }); !errors.Is(err, context.Canceled) || IsUnavailable(err) {
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
