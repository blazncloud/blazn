package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type EventStream interface {
	Next() (client.SandboxEvent, error)
	Close() error
}

// API is the frozen generated sandbox client surface consumed by the CLI.
// ClientAPI adapts *client.Client to this interface without modifying generated code.
type API interface {
	CreateSandboxTemplate(context.Context, string, string, string, client.SandboxManifest) (client.SandboxTemplateEnvelope, error)
	ListSandboxTemplates(context.Context, string, string, string) (client.SandboxTemplateList, error)
	ReplaceSandboxTemplateDraft(context.Context, string, string, string, client.ReplaceSandboxTemplateDraftRequest) (client.SandboxTemplateEnvelope, error)
	PublishSandboxTemplateVersion(context.Context, string, string, string, client.PublishSandboxTemplateVersionRequest) (client.SandboxTemplateVersionEnvelope, error)
	CreateSandbox(context.Context, string, string, string, client.CreateSandboxRequest) (client.SandboxMutation, error)
	ListSandboxes(context.Context, string, string, string) (client.SandboxList, error)
	GetSandbox(context.Context, string, string) (client.Sandbox, error)
	CreateSandboxOperation(context.Context, string, string, string, client.CreateSandboxOperationRequest) (client.SandboxMutation, error)
	CreateSandboxAccessGrant(context.Context, string, string, client.CreateSandboxAccessGrantRequest) (client.SandboxAccessGrantCreated, error)
	StreamSandboxEvents(context.Context, string, string, string) (EventStream, error)
	ExecuteSandboxGrant(context.Context, string, string, client.SandboxExecRequest) (client.SandboxExecResult, error)
	UploadSandboxGrantFile(context.Context, string, string, string, string, io.Reader, int64) (client.SandboxFileTransferResult, error)
	DownloadSandboxGrantFile(context.Context, string, string, string) (io.ReadCloser, int64, string, error)
}

type ClientAPI struct {
	Client *client.Client
	wire   *strictTransport
}

func (a ClientAPI) CreateSandboxTemplate(ctx context.Context, token, workspace, key string, manifest client.SandboxManifest) (client.SandboxTemplateEnvelope, error) {
	return a.Client.CreateSandboxTemplate(ctx, token, workspace, key, manifest)
}
func (a ClientAPI) ListSandboxTemplates(ctx context.Context, token, workspace, cursor string) (client.SandboxTemplateList, error) {
	return a.Client.ListSandboxTemplates(ctx, token, workspace, cursor)
}
func (a ClientAPI) ReplaceSandboxTemplateDraft(ctx context.Context, token, template, key string, request client.ReplaceSandboxTemplateDraftRequest) (client.SandboxTemplateEnvelope, error) {
	return a.Client.ReplaceSandboxTemplateDraft(ctx, token, template, key, request)
}
func (a ClientAPI) PublishSandboxTemplateVersion(ctx context.Context, token, template, key string, request client.PublishSandboxTemplateVersionRequest) (client.SandboxTemplateVersionEnvelope, error) {
	return a.Client.PublishSandboxTemplateVersion(ctx, token, template, key, request)
}
func (a ClientAPI) CreateSandbox(ctx context.Context, token, workspace, key string, request client.CreateSandboxRequest) (client.SandboxMutation, error) {
	return a.Client.CreateSandbox(ctx, token, workspace, key, request)
}
func (a ClientAPI) ListSandboxes(ctx context.Context, token, workspace, cursor string) (client.SandboxList, error) {
	return a.Client.ListSandboxes(ctx, token, workspace, cursor)
}
func (a ClientAPI) GetSandbox(ctx context.Context, token, id string) (client.Sandbox, error) {
	return a.Client.GetSandbox(ctx, token, id)
}
func (a ClientAPI) CreateSandboxOperation(ctx context.Context, token, id, key string, request client.CreateSandboxOperationRequest) (client.SandboxMutation, error) {
	return a.Client.CreateSandboxOperation(ctx, token, id, key, request)
}
func (a ClientAPI) CreateSandboxAccessGrant(ctx context.Context, token, id string, request client.CreateSandboxAccessGrantRequest) (client.SandboxAccessGrantCreated, error) {
	return a.Client.CreateSandboxAccessGrant(ctx, token, id, request)
}
func (a ClientAPI) StreamSandboxEvents(ctx context.Context, token, id, cursor string) (EventStream, error) {
	if a.wire == nil {
		return nil, errors.New("strict sandbox event transport is unavailable")
	}
	return a.wire.StreamSandboxEvents(ctx, token, id, cursor)
}
func (a ClientAPI) ExecuteSandboxGrant(ctx context.Context, id, token string, request client.SandboxExecRequest) (client.SandboxExecResult, error) {
	if a.wire == nil {
		return client.SandboxExecResult{}, errors.New("strict sandbox exec transport is unavailable")
	}
	return a.wire.ExecuteSandboxGrant(ctx, id, token, request)
}
func (a ClientAPI) UploadSandboxGrantFile(ctx context.Context, id, token, path, digest string, body io.Reader, size int64) (client.SandboxFileTransferResult, error) {
	return a.Client.UploadSandboxGrantFile(ctx, id, token, path, digest, body, size)
}
func (a ClientAPI) DownloadSandboxGrantFile(ctx context.Context, id, token, path string) (io.ReadCloser, int64, string, error) {
	return a.Client.DownloadSandboxGrantFile(ctx, id, token, path)
}

type TokenProvider interface {
	AccessToken(context.Context, bool) (string, error)
}
type workspaceTokenProvider struct{ sessions workspacepkg.SessionProvider }

func (p workspaceTokenProvider) AccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	session, err := p.sessions.Session(ctx, forceRefresh)
	return session.AccessToken, err
}
func NewWorkspaceTokenProvider(provider workspacepkg.SessionProvider) TokenProvider {
	return workspaceTokenProvider{sessions: provider}
}

type Service struct {
	api       API
	tokens    TokenProvider
	reconnect time.Duration
	maxErrors int
	now       func() time.Time
}

func NewService(api API, tokens TokenProvider) *Service {
	return &Service{api: api, tokens: tokens, reconnect: 100 * time.Millisecond, maxErrors: 3, now: time.Now}
}

func NewDefaultService() (*Service, error) {
	sessions, err := workspacepkg.NewDefaultSessionProvider()
	if err != nil {
		return nil, err
	}
	apiURL := os.Getenv("BLAZN_API_URL")
	if apiURL == "" {
		apiURL = sessions.Origin()
	}
	// Access exec is bounded to ten minutes by the controller. Keep the
	// client deadline above that boundary while retaining one finite timeout
	// for every Sandbox API operation.
	httpClient := &http.Client{Timeout: 11 * time.Minute}
	generated, err := client.New(apiURL, httpClient)
	if err != nil {
		return nil, err
	}
	wire, err := newStrictTransport(apiURL, httpClient)
	if err != nil {
		return nil, err
	}
	return NewService(ClientAPI{Client: generated, wire: wire}, NewWorkspaceTokenProvider(sessions)), nil
}

func (s *Service) token(ctx context.Context, forceRefresh bool) (string, error) {
	value, err := s.tokens.AccessToken(ctx, forceRefresh)
	if err != nil {
		return "", &UnavailableError{Cause: err}
	}
	if value == "" {
		return "", &UnavailableError{Cause: errors.New("authenticated access token is unavailable")}
	}
	return value, nil
}

func withAccessToken[T any](ctx context.Context, s *Service, action func(string) (T, error)) (T, error) {
	var zero T
	token, err := s.token(ctx, false)
	if err != nil {
		return zero, err
	}
	result, err := action(token)
	if !client.IsCode(err, "access_expired") {
		return result, err
	}
	token, refreshErr := s.token(ctx, true)
	if refreshErr != nil {
		return zero, refreshErr
	}
	return action(token)
}

func (s *Service) Publish(ctx context.Context, workspace, requestID string, manifest []byte) (TemplatePublish, error) {
	validation := ValidateTemplate(manifest)
	if !validation.Valid {
		return TemplatePublish{}, fmt.Errorf("template invalid: %s", strings.Join(validation.Errors, "; "))
	}
	name, err := TemplateName(manifest)
	if err != nil {
		return TemplatePublish{}, err
	}
	draft, found, err := s.findTemplate(ctx, workspace, name)
	if err != nil {
		return TemplatePublish{}, err
	}
	if found {
		draft, err = withAccessToken(ctx, s, func(token string) (client.SandboxTemplateEnvelope, error) {
			return s.api.ReplaceSandboxTemplateDraft(ctx, token, draft.Template.ID, requestID, client.ReplaceSandboxTemplateDraftRequest{ExpectedDraftVersion: draft.Template.DraftVersion, Manifest: client.SandboxManifest(append([]byte(nil), manifest...))})
		})
	} else {
		draft, err = withAccessToken(ctx, s, func(token string) (client.SandboxTemplateEnvelope, error) {
			return s.api.CreateSandboxTemplate(ctx, token, workspace, requestID, client.SandboxManifest(append([]byte(nil), manifest...)))
		})
	}
	if err != nil {
		return TemplatePublish{}, err
	}
	published, err := withAccessToken(ctx, s, func(token string) (client.SandboxTemplateVersionEnvelope, error) {
		return s.api.PublishSandboxTemplateVersion(ctx, token, draft.Template.ID, requestID, client.PublishSandboxTemplateVersionRequest{ExpectedDraftVersion: draft.Template.DraftVersion})
	})
	if err != nil {
		return TemplatePublish{}, err
	}
	return TemplatePublish{Template: published.Template, Version: published.Version}, nil
}

func (s *Service) findTemplate(ctx context.Context, workspace, name string) (client.SandboxTemplateEnvelope, bool, error) {
	cursor := ""
	for page := 0; page < 100; page++ {
		list, err := withAccessToken(ctx, s, func(token string) (client.SandboxTemplateList, error) {
			return s.api.ListSandboxTemplates(ctx, token, workspace, cursor)
		})
		if err != nil {
			return client.SandboxTemplateEnvelope{}, false, err
		}
		for _, item := range list.Items {
			if item.Name == name {
				return client.SandboxTemplateEnvelope{Template: item}, true, nil
			}
		}
		if list.NextCursor == nil || *list.NextCursor == "" {
			return client.SandboxTemplateEnvelope{}, false, nil
		}
		if *list.NextCursor == cursor {
			return client.SandboxTemplateEnvelope{}, false, errors.New("template pagination cursor did not advance")
		}
		cursor = *list.NextCursor
	}
	return client.SandboxTemplateEnvelope{}, false, errors.New("template lookup exceeded pagination limit")
}

func (s *Service) Create(ctx context.Context, workspace, requestID string, request client.CreateSandboxRequest) (client.SandboxMutation, error) {
	return withAccessToken(ctx, s, func(token string) (client.SandboxMutation, error) {
		return s.api.CreateSandbox(ctx, token, workspace, requestID, request)
	})
}
func (s *Service) List(ctx context.Context, workspace, cursor string) (client.SandboxList, error) {
	return withAccessToken(ctx, s, func(token string) (client.SandboxList, error) {
		return s.api.ListSandboxes(ctx, token, workspace, cursor)
	})
}
func (s *Service) Get(ctx context.Context, id string) (client.Sandbox, error) {
	return withAccessToken(ctx, s, func(token string) (client.Sandbox, error) { return s.api.GetSandbox(ctx, token, id) })
}
func (s *Service) Stop(ctx context.Context, id, requestID string) (client.SandboxMutation, error) {
	return s.mutate(ctx, id, requestID, client.SandboxOperationStop)
}
func (s *Service) Delete(ctx context.Context, id, requestID string) (client.SandboxMutation, error) {
	return s.mutate(ctx, id, requestID, client.SandboxOperationDelete)
}
func (s *Service) mutate(ctx context.Context, id, requestID string, operation client.SandboxOperationType) (client.SandboxMutation, error) {
	current, err := withAccessToken(ctx, s, func(token string) (client.Sandbox, error) { return s.api.GetSandbox(ctx, token, id) })
	if err != nil {
		return client.SandboxMutation{}, err
	}
	return withAccessToken(ctx, s, func(token string) (client.SandboxMutation, error) {
		return s.api.CreateSandboxOperation(ctx, token, id, requestID, client.CreateSandboxOperationRequest{Type: operation, ExpectedVersion: current.Version})
	})
}

func (s *Service) Exec(ctx context.Context, id string, command []string) (ExecResult, error) {
	if len(command) < 1 || len(command) > 32 {
		return ExecResult{}, errors.New("exec command must contain 1 to 32 arguments")
	}
	for _, arg := range command {
		if arg == "" || len(arg) > 1024 {
			return ExecResult{}, errors.New("exec arguments must contain 1 to 1024 characters")
		}
	}
	grant, err := s.createGrant(ctx, id, client.SandboxGrantExec)
	if err != nil {
		return ExecResult{}, err
	}
	result, err := s.api.ExecuteSandboxGrant(ctx, grant.Grant.ID, grant.AccessToken, client.SandboxExecRequest{Command: append([]string(nil), command...)})
	output := ExecResult{SandboxID: id, GrantID: grant.Grant.ID, RemoteExitCode: result.RemoteExitCode, StdoutBase64: result.StdoutBase64, StderrBase64: result.StderrBase64, Truncated: result.Truncated}
	grant.AccessToken = ""
	if err != nil {
		// The grant is single-use and the transport did not return a complete,
		// validated result. Mark the receipt truncated so JSON callers cannot
		// mistake Go zero values for a successful command with empty output.
		output.Truncated = true
		return output, &PartialError{Cause: err}
	}
	if err := validateExecResult(result); err != nil {
		return ExecResult{SandboxID: id, GrantID: output.GrantID, Truncated: true}, &PartialError{Cause: err}
	}
	if result.Truncated {
		return output, &PartialError{Cause: errors.New("remote output was truncated")}
	}
	if result.RemoteExitCode != 0 {
		return output, &RemoteExitError{Code: result.RemoteExitCode}
	}
	return output, nil
}

func (s *Service) Upload(ctx context.Context, id, source, destination string) (TransferResult, error) {
	if !ValidTransferPath(destination) {
		return TransferResult{}, errors.New("sandbox upload destination path is invalid")
	}
	file, info, err := openRegularNoFollow(source)
	if err != nil {
		return TransferResult{}, fmt.Errorf("open upload source: %w", err)
	}
	defer file.Close()
	if info.Size() > client.SandboxMaxFileBytes {
		return TransferResult{}, fmt.Errorf("upload exceeds %d bytes", client.SandboxMaxFileBytes)
	}
	hasher := sha256.New()
	count, err := io.Copy(hasher, io.LimitReader(file, client.SandboxMaxFileBytes+1))
	if err != nil {
		return TransferResult{}, fmt.Errorf("hash upload source: %w", err)
	}
	if count != info.Size() || count > client.SandboxMaxFileBytes {
		return TransferResult{}, errors.New("upload source changed while it was read or exceeds the size limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return TransferResult{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	grant, err := s.createGrant(ctx, id, client.SandboxGrantUpload)
	if err != nil {
		return TransferResult{}, err
	}
	remote, err := s.api.UploadSandboxGrantFile(ctx, grant.Grant.ID, grant.AccessToken, destination, digest, file, count)
	grant.AccessToken = ""
	output := TransferResult{SandboxID: id, GrantID: grant.Grant.ID, Source: source, Destination: destination, Size: count, SHA256: digest}
	if err != nil {
		return output, err
	}
	if remote.Path != destination || remote.Size != count || remote.SHA256 != digest {
		return output, errors.New("upload receipt does not match the transferred file")
	}
	return output, nil
}

func (s *Service) Download(ctx context.Context, id, source, destination string) (TransferResult, error) {
	if !ValidTransferPath(source) {
		return TransferResult{}, errors.New("sandbox download source path is invalid")
	}
	if err := validateDownloadDestination(destination); err != nil {
		return TransferResult{}, err
	}
	grant, err := s.createGrant(ctx, id, client.SandboxGrantDownload)
	if err != nil {
		return TransferResult{}, err
	}
	body, size, digest, err := s.api.DownloadSandboxGrantFile(ctx, grant.Grant.ID, grant.AccessToken, source)
	grant.AccessToken = ""
	output := TransferResult{SandboxID: id, GrantID: grant.Grant.ID, Source: source, Destination: destination, Size: size, SHA256: digest}
	if err != nil {
		return output, err
	}
	defer body.Close()
	if size > client.SandboxMaxFileBytes || !ValidDigest(digest) {
		return output, errors.New("download metadata is invalid")
	}
	if err := writeVerifiedFile(destination, body, size, digest); err != nil {
		return output, err
	}
	return output, nil
}

func (s *Service) createGrant(ctx context.Context, id string, kind client.SandboxGrantKind) (client.SandboxAccessGrantCreated, error) {
	grant, err := withAccessToken(ctx, s, func(token string) (client.SandboxAccessGrantCreated, error) {
		return s.api.CreateSandboxAccessGrant(ctx, token, id, client.CreateSandboxAccessGrantRequest{Kind: kind, ExpiresInSeconds: 60})
	})
	if err != nil {
		return client.SandboxAccessGrantCreated{}, err
	}
	if err := validateGrant(grant, id, kind, s.now().UTC()); err != nil {
		return client.SandboxAccessGrantCreated{}, err
	}
	return grant, nil
}

func writeVerifiedFile(destination string, body io.Reader, expected int64, digest string) error {
	directory := filepath.Dir(destination)
	temp, err := os.CreateTemp(directory, ".blazn-download-*")
	if err != nil {
		return fmt.Errorf("create download destination: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	hasher := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(body, client.SandboxMaxFileBytes+1))
	closeErr := temp.Close()
	if copyErr != nil {
		return fmt.Errorf("write download: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close download: %w", closeErr)
	}
	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if count != expected || count > client.SandboxMaxFileBytes || got != digest {
		return errors.New("download bytes do not match declared size and digest")
	}
	if err := validateDownloadDestination(destination); err != nil {
		return err
	}
	if err := os.Rename(tempName, destination); err != nil {
		return fmt.Errorf("install download: %w", err)
	}
	return nil
}

func validateDownloadDestination(path string) error {
	if path == "" {
		return errors.New("download destination is required")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("download destination must be a regular file and not a symlink")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return errors.New("download destination parent must be an existing directory")
	}
	return nil
}
