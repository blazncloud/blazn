package development

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
	"github.com/gowebpki/jcs"
)

const maxResponseBytes = 16 * 1024 * 1024

type BuildDocument struct {
	raw           json.RawMessage
	ID, Status    string
	Version       int
	ReceiptDigest *string
}

func (b BuildDocument) MarshalJSON() ([]byte, error) { return append([]byte(nil), b.raw...), nil }
func (b BuildDocument) Summary() (string, string, int, *string) {
	return b.ID, b.Status, b.Version, b.ReceiptDigest
}

type EvidenceExport struct {
	BuildID, Directory, ManifestDigest string
	ArtifactIDs                        []string
}

func (e EvidenceExport) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		BuildID        string   `json:"buildId"`
		Directory      string   `json:"directory"`
		ManifestDigest string   `json:"manifestDigest"`
		ArtifactIDs    []string `json:"artifactIds"`
	}{e.BuildID, e.Directory, e.ManifestDigest, e.ArtifactIDs})
}

type evidenceBundle struct {
	Manifest json.RawMessage `json:"manifest"`
	Files    []evidenceFile  `json:"files"`
}
type evidenceFile struct{ Path, ContentBase64 string }

type Service struct {
	sessions workspacepkg.SessionProvider
	contexts workspacepkg.ContextStore
	client   *http.Client
}

func NewDefaultService() (*Service, error) {
	sessions, err := workspacepkg.NewDefaultSessionProvider()
	if err != nil {
		return nil, err
	}
	contexts, err := workspacepkg.NewFileContextStore()
	if err != nil {
		return nil, err
	}
	return &Service{sessions: sessions, contexts: contexts, client: &http.Client{Timeout: 30 * time.Second}}, nil
}
func NewService(s workspacepkg.SessionProvider, c workspacepkg.ContextStore, h *http.Client) *Service {
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{sessions: s, contexts: c, client: h}
}

func (s *Service) Build(ctx context.Context, commit, requestID string) (BuildDocument, error) {
	return s.mutate(ctx, http.MethodPost, "/development-builds", requestID, map[string]any{"commit": commit})
}
func (s *Service) Test(ctx context.Context, id, suite, requestID string) (BuildDocument, error) {
	return s.mutate(ctx, http.MethodPost, "/development-builds/"+id+"/tests", requestID, map[string]any{"suite": suite})
}
func (s *Service) Status(ctx context.Context, id string) (BuildDocument, error) {
	return s.requestBuild(ctx, http.MethodGet, "/development-builds/"+id, "", nil)
}
func (s *Service) Publish(ctx context.Context, id string, version int, requestID string) (BuildDocument, error) {
	return s.mutate(ctx, http.MethodPost, "/development-builds/"+id+"/publication", requestID, map[string]any{"expectedVersion": version})
}
func (s *Service) mutate(ctx context.Context, method, path, requestID string, body any) (BuildDocument, error) {
	if len(requestID) < 8 || len(requestID) > 128 {
		return BuildDocument{}, errors.New("request ID must contain 8 to 128 characters")
	}
	return s.requestBuild(ctx, method, path, requestID, body)
}

func (s *Service) requestBuild(ctx context.Context, method, suffix, requestID string, body any) (BuildDocument, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return BuildDocument{}, err
	}
	action := func(current workspacepkg.Session) (BuildDocument, error) {
		path := "/v1/workspaces/" + url.PathEscape(selection.WorkspaceID) + "/projects/" + url.PathEscape(selection.ProjectID) + suffix
		var encoded io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return BuildDocument{}, err
			}
			encoded = bytes.NewReader(data)
		}
		request, err := http.NewRequestWithContext(ctx, method, s.sessions.Origin()+path, encoded)
		if err != nil {
			return BuildDocument{}, err
		}
		request.Header.Set("Authorization", "Bearer "+current.AccessToken)
		request.Header.Set("Accept", "application/json")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if requestID != "" {
			request.Header.Set("Idempotency-Key", requestID)
		}
		response, err := s.client.Do(request)
		if err != nil {
			return BuildDocument{}, err
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		if err != nil || len(payload) > maxResponseBytes {
			return BuildDocument{}, errors.New("development response exceeds its bound")
		}
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return BuildDocument{}, apiError(response.StatusCode, payload)
		}
		var envelope struct {
			Build json.RawMessage `json:"build"`
		}
		if err := strictJSON(payload, &envelope); err != nil || len(envelope.Build) == 0 {
			return BuildDocument{}, errors.New("development Build response is invalid")
		}
		return DecodeBuild(envelope.Build)
	}
	result, err := action(session)
	if client.IsCode(err, "access_expired") {
		session, refreshErr := s.sessions.Session(ctx, true)
		if refreshErr != nil {
			return BuildDocument{}, refreshErr
		}
		return action(session)
	}
	return result, err
}

func (s *Service) Evidence(ctx context.Context, id, directory string) (EvidenceExport, error) {
	selection, session, err := s.selection(ctx)
	if err != nil {
		return EvidenceExport{}, err
	}
	pathValue := "/v1/workspaces/" + url.PathEscape(selection.WorkspaceID) + "/projects/" + url.PathEscape(selection.ProjectID) + "/development-builds/" + url.PathEscape(id) + "/evidence"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sessions.Origin()+pathValue, nil)
	if err != nil {
		return EvidenceExport{}, err
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return EvidenceExport{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(payload) > maxResponseBytes {
		return EvidenceExport{}, errors.New("development evidence response exceeds its bound")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return EvidenceExport{}, apiError(response.StatusCode, payload)
	}
	var envelope struct {
		BuildID string         `json:"buildId"`
		Bundle  evidenceBundle `json:"bundle"`
	}
	if err := strictJSON(payload, &envelope); err != nil || envelope.BuildID != id {
		return EvidenceExport{}, errors.New("development evidence response is invalid")
	}
	return writeEvidence(directory, envelope.BuildID, envelope.Bundle)
}

func (s *Service) selection(ctx context.Context) (workspacepkg.Selection, workspacepkg.Session, error) {
	session, err := s.sessions.Session(ctx, false)
	if err != nil {
		return workspacepkg.Selection{}, workspacepkg.Session{}, err
	}
	selection, err := s.contexts.Load(s.sessions.Origin(), session.UserID)
	if err != nil {
		return selection, session, err
	}
	if selection.WorkspaceID == "" || selection.ProjectID == "" {
		return selection, session, errors.New("Workspace and Project selections are required")
	}
	return selection, session, nil
}

func DecodeBuild(raw json.RawMessage) (BuildDocument, error) {
	var summary struct {
		ID, Status    string
		Version       int
		ReceiptDigest *string
	}
	if err := json.Unmarshal(raw, &summary); err != nil || !uuidPattern.MatchString(summary.ID) || summary.Version < 1 {
		return BuildDocument{}, errors.New("development Build response is invalid")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil || containsForbidden(value) {
		return BuildDocument{}, errors.New("development Build response contains forbidden fields")
	}
	return BuildDocument{raw: append(json.RawMessage(nil), raw...), ID: summary.ID, Status: summary.Status, Version: summary.Version, ReceiptDigest: summary.ReceiptDigest}, nil
}
func containsForbidden(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
			if normalized == "objectkey" || normalized == "signedurl" || normalized == "secretvalue" || normalized == "registrycredential" || normalized == "buildkitendpoint" || containsForbidden(child) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if containsForbidden(child) {
				return true
			}
		}
	}
	return false
}

func writeEvidence(directory, buildID string, bundle evidenceBundle) (EvidenceExport, error) {
	if directory == "" || len(bundle.Files) > 100 {
		return EvidenceExport{}, errors.New("evidence export is invalid")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return EvidenceExport{}, err
	}
	parent := filepath.Dir(absolute)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return EvidenceExport{}, errors.New("evidence output parent is unsafe")
	}
	if _, err := os.Lstat(absolute); !errors.Is(err, os.ErrNotExist) {
		return EvidenceExport{}, errors.New("evidence output already exists")
	}
	if err := os.Mkdir(absolute, 0o700); err != nil {
		return EvidenceExport{}, err
	}
	written := false
	defer func() {
		if !written {
			_ = os.Remove(absolute)
		}
	}()
	artifactIDs := []string{}
	seen := map[string]bool{}
	total := 0
	for _, file := range bundle.Files {
		if !safeEvidencePath(file.Path) || seen[file.Path] {
			return EvidenceExport{}, errors.New("evidence file path is unsafe")
		}
		seen[file.Path] = true
		content, err := base64.RawStdEncoding.DecodeString(file.ContentBase64)
		if err != nil {
			content, err = base64.StdEncoding.DecodeString(file.ContentBase64)
		}
		if err != nil || len(content) > 8*1024*1024 {
			return EvidenceExport{}, errors.New("evidence file content is invalid")
		}
		total += len(content)
		if total > 64*1024*1024 {
			return EvidenceExport{}, errors.New("evidence export exceeds its bound")
		}
		target := filepath.Join(absolute, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return EvidenceExport{}, err
		}
		handle, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return EvidenceExport{}, err
		}
		_, writeErr := handle.Write(content)
		closeErr := handle.Close()
		if writeErr != nil || closeErr != nil {
			return EvidenceExport{}, errors.New("evidence file write failed")
		}
	}
	canonical, err := jcs.Transform(bundle.Manifest)
	if err != nil {
		return EvidenceExport{}, errors.New("evidence manifest is invalid")
	}
	digest := sha256.Sum256(canonical)
	var manifest struct {
		ArtifactIDs []string `json:"artifactIds"`
	}
	if err := json.Unmarshal(bundle.Manifest, &manifest); err != nil {
		return EvidenceExport{}, errors.New("evidence manifest is invalid")
	}
	for _, id := range manifest.ArtifactIDs {
		if !uuidPattern.MatchString(id) {
			return EvidenceExport{}, errors.New("evidence artifact identity is invalid")
		}
		artifactIDs = append(artifactIDs, id)
	}
	if len(artifactIDs) > 100 {
		return EvidenceExport{}, errors.New("evidence artifact count is invalid")
	}
	written = true
	return EvidenceExport{BuildID: buildID, Directory: absolute, ManifestDigest: "sha256:" + hex.EncodeToString(digest[:]), ArtifactIDs: artifactIDs}, nil
}
func safeEvidencePath(value string) bool {
	if value == "" || len(value) > 512 || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON contains trailing data")
	}
	return nil
}
func apiError(status int, payload []byte) error {
	var body struct {
		Error struct{ Code, Message, RequestID string } `json:"error"`
	}
	if json.Unmarshal(payload, &body) == nil && body.Error.Code != "" {
		return &client.APIError{StatusCode: status, Body: client.ErrorBody{Code: body.Error.Code, Message: body.Error.Message, RequestID: body.Error.RequestID}}
	}
	return &client.APIError{StatusCode: status, Body: client.ErrorBody{Code: "development_api_error", Message: "development request failed with status " + strconv.Itoa(status)}}
}
