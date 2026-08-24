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

const (
	maxEvidenceFiles     = 100
	maxEvidenceFileBytes = 8 * 1024 * 1024
	maxEvidenceBytes     = 64 * 1024 * 1024
	evidenceManifestPath = "manifest.json"
)

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
type evidenceFile struct {
	ArtifactID    string `json:"artifactId"`
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
}

type validatedEvidence struct {
	canonical   []byte
	artifactIDs []string
	files       []validatedEvidenceFile
}

type validatedEvidenceFile struct {
	artifactID string
	path       string
	content    []byte
}

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
			if credentialField(normalized) || containsForbidden(child) {
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

func credentialField(normalized string) bool {
	switch normalized {
	case "apikey", "authorization", "buildkitendpoint", "credential", "credentials", "objectkey", "password", "registrycredential", "secret", "secretvalue", "signedurl", "token", "accesstoken", "refreshtoken":
		return true
	}
	return strings.HasSuffix(normalized, "apikey") || strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "secret") || strings.HasSuffix(normalized, "token") || strings.HasSuffix(normalized, "credential")
}

func writeEvidence(directory, buildID string, bundle evidenceBundle) (result EvidenceExport, resultErr error) {
	validated, err := validateEvidence(bundle)
	if err != nil {
		return EvidenceExport{}, err
	}
	if directory == "" {
		return EvidenceExport{}, errors.New("evidence export is invalid")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return EvidenceExport{}, err
	}
	parent := filepath.Dir(absolute)
	base := filepath.Base(absolute)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent || base == "." || base == string(filepath.Separator) {
		return EvidenceExport{}, errors.New("evidence output parent is unsafe")
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return EvidenceExport{}, errors.New("evidence output parent cannot be opened safely")
	}
	defer parentRoot.Close()
	if err := parentRoot.Mkdir(base, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return EvidenceExport{}, errors.New("evidence output already exists")
		}
		return EvidenceExport{}, err
	}
	createdRootInfo, err := parentRoot.Lstat(base)
	if err != nil || !createdRootInfo.IsDir() || createdRootInfo.Mode()&os.ModeSymlink != 0 {
		cleanupErr := removeCreatedRoot(parentRoot, base, createdRootInfo)
		return EvidenceExport{}, errors.Join(errors.New("evidence output was substituted"), cleanupFailure(cleanupErr))
	}
	created := []string{}
	root, err := parentRoot.OpenRoot(base)
	if err != nil {
		cleanupErr := removeCreatedRoot(parentRoot, base, createdRootInfo)
		if cleanupErr != nil {
			return EvidenceExport{}, errors.Join(err, fmt.Errorf("evidence cleanup failed: %w", cleanupErr))
		}
		return EvidenceExport{}, err
	}
	openedRootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(createdRootInfo, openedRootInfo) {
		closeErr := root.Close()
		cleanupErr := removeCreatedRoot(parentRoot, base, createdRootInfo)
		return EvidenceExport{}, errors.Join(errors.New("evidence output was substituted"), cleanupFailure(errors.Join(closeErr, cleanupErr)))
	}
	defer func() {
		if resultErr == nil {
			if closeErr := root.Close(); closeErr != nil {
				resultErr = fmt.Errorf("evidence output close failed: %w", closeErr)
			}
			return
		}
		cleanupErr := cleanupEvidence(root, parentRoot, base, createdRootInfo, created)
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("evidence cleanup failed: %w", cleanupErr))
		}
	}()
	createdDirectories := map[string]bool{}
	for _, file := range validated.files {
		if err := createEvidenceParents(root, filepath.FromSlash(file.path), createdDirectories, &created); err != nil {
			return EvidenceExport{}, err
		}
		if err := writeEvidenceFile(root, filepath.FromSlash(file.path), file.content); err != nil {
			return EvidenceExport{}, err
		}
		created = append(created, filepath.FromSlash(file.path))
	}
	if err := writeEvidenceFile(root, evidenceManifestPath, validated.canonical); err != nil {
		return EvidenceExport{}, err
	}
	created = append(created, evidenceManifestPath)
	digest := sha256.Sum256(validated.canonical)
	return EvidenceExport{BuildID: buildID, Directory: absolute, ManifestDigest: "sha256:" + hex.EncodeToString(digest[:]), ArtifactIDs: validated.artifactIDs}, nil
}

func validateEvidence(bundle evidenceBundle) (validatedEvidence, error) {
	if len(bundle.Manifest) == 0 || len(bundle.Manifest) > maxManifestBytes || len(bundle.Files) == 0 || len(bundle.Files) > maxEvidenceFiles {
		return validatedEvidence{}, errors.New("evidence export is invalid")
	}
	if err := validateJSONTopology(bundle.Manifest); err != nil {
		return validatedEvidence{}, errors.New("evidence manifest is invalid")
	}
	var whole any
	if err := json.Unmarshal(bundle.Manifest, &whole); err != nil || containsForbidden(whole) {
		return validatedEvidence{}, errors.New("evidence manifest contains forbidden or invalid fields")
	}
	var manifest struct {
		ArtifactIDs []string `json:"artifactIds"`
	}
	if err := strictJSON(bundle.Manifest, &manifest); err != nil {
		// The manifest is intentionally extensible, but its complete topology and
		// values were checked above. Decode the binding field separately.
		if err := json.Unmarshal(bundle.Manifest, &manifest); err != nil {
			return validatedEvidence{}, errors.New("evidence manifest is invalid")
		}
	}
	if len(manifest.ArtifactIDs) == 0 || len(manifest.ArtifactIDs) > maxEvidenceFiles {
		return validatedEvidence{}, errors.New("evidence artifact count is invalid")
	}
	manifestIDs := make(map[string]bool, len(manifest.ArtifactIDs))
	artifactIDs := make([]string, 0, len(manifest.ArtifactIDs))
	for _, id := range manifest.ArtifactIDs {
		if !uuidPattern.MatchString(id) || manifestIDs[id] {
			return validatedEvidence{}, errors.New("evidence artifact identity is invalid")
		}
		manifestIDs[id] = true
		artifactIDs = append(artifactIDs, id)
	}
	seenPaths := map[string]bool{evidenceManifestPath: true}
	seenIDs := make(map[string]bool, len(bundle.Files))
	files := make([]validatedEvidenceFile, 0, len(bundle.Files))
	total := 0
	for _, file := range bundle.Files {
		if !safeEvidencePath(file.Path) || seenPaths[file.Path] || !uuidPattern.MatchString(file.ArtifactID) || seenIDs[file.ArtifactID] || !manifestIDs[file.ArtifactID] {
			return validatedEvidence{}, errors.New("evidence file identity or path is invalid")
		}
		seenPaths[file.Path], seenIDs[file.ArtifactID] = true, true
		content, err := base64.StdEncoding.Strict().DecodeString(file.ContentBase64)
		if err != nil {
			content, err = base64.RawStdEncoding.Strict().DecodeString(file.ContentBase64)
		}
		if err != nil || len(content) > maxEvidenceFileBytes || containsCredentialBytes(content) {
			return validatedEvidence{}, errors.New("evidence file content is invalid")
		}
		total += len(content)
		if total > maxEvidenceBytes {
			return validatedEvidence{}, errors.New("evidence export exceeds its bound")
		}
		files = append(files, validatedEvidenceFile{artifactID: file.ArtifactID, path: file.Path, content: content})
	}
	if len(seenIDs) != len(manifestIDs) {
		return validatedEvidence{}, errors.New("evidence files do not exactly match manifest artifacts")
	}
	canonical, err := jcs.Transform(bundle.Manifest)
	if err != nil {
		return validatedEvidence{}, errors.New("evidence manifest is invalid")
	}
	return validatedEvidence{canonical: canonical, artifactIDs: artifactIDs, files: files}, nil
}

func containsCredentialBytes(content []byte) bool {
	if !json.Valid(content) {
		return false
	}
	var value any
	return json.Unmarshal(content, &value) == nil && containsForbidden(value)
}

func createEvidenceParents(root *os.Root, name string, createdDirectories map[string]bool, created *[]string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if createdDirectories[current] {
			continue
		}
		if err := root.Mkdir(current, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errors.New("evidence output path was substituted")
			}
			return err
		}
		createdDirectories[current] = true
		*created = append(*created, current)
	}
	return nil
}

func writeEvidenceFile(root *os.Root, name string, content []byte) error {
	handle, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("evidence file cannot be created safely")
	}
	_, writeErr := handle.Write(content)
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(errors.New("evidence file write failed"), writeErr, closeErr)
	}
	return nil
}

func cleanupEvidence(root, parentRoot *os.Root, base string, createdRootInfo os.FileInfo, created []string) error {
	var cleanupErr error
	for index := len(created) - 1; index >= 0; index-- {
		if err := root.Remove(created[index]); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if err := root.Close(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	cleanupErr = errors.Join(cleanupErr, removeCreatedRoot(parentRoot, base, createdRootInfo))
	return cleanupErr
}

func removeCreatedRoot(parentRoot *os.Root, base string, createdRootInfo os.FileInfo) error {
	current, err := parentRoot.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if createdRootInfo == nil || !os.SameFile(createdRootInfo, current) {
		return errors.New("created evidence output path was substituted; refusing cleanup")
	}
	return parentRoot.Remove(base)
}

func cleanupFailure(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("evidence cleanup failed: %w", err)
}
func safeEvidencePath(value string) bool {
	if value == "" || len(value) > 512 || filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00") {
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
