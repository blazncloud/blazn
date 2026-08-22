package microk8sissuer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type BackendIssue struct {
	TokenCheck string
	URLs       []string
	ExpiresAt  time.Time
}
type Backend interface {
	Issue(context.Context, string, int) (BackendIssue, error)
	Revoke(context.Context, string) error
	Healthy(context.Context) error
}
type Service struct {
	stateRoot string
	key       []byte
	backend   Backend
	now       func() time.Time
}
type durableState struct {
	SchemaVersion string    `json:"schemaVersion"`
	Status        string    `json:"status"`
	Request       Request   `json:"request"`
	RequestDigest string    `json:"requestDigest"`
	TokenHash     string    `json:"tokenHash"`
	Credential    string    `json:"credential,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt,omitempty"`
}
type credentialPayload struct {
	SchemaVersion    string    `json:"schemaVersion"`
	IssuanceID       string    `json:"issuanceId"`
	ClusterID        string    `json:"clusterId"`
	ExpectedNodeName string    `json:"expectedNodeName"`
	BootstrapTaint   string    `json:"bootstrapTaint"`
	WorkerOnly       bool      `json:"workerOnly"`
	ExpiresAt        time.Time `json:"expiresAt"`
	URLs             []string  `json:"urls"`
}

func NewService(stateRoot string, key []byte, backend Backend) (*Service, error) {
	if !filepath.IsAbs(stateRoot) || len(key) < 32 || backend == nil {
		return nil, fmt.Errorf("issuer configuration is invalid")
	}
	return &Service{stateRoot: stateRoot, key: append([]byte(nil), key...), backend: backend, now: time.Now}, nil
}

func (s *Service) Handle(ctx context.Context, req Request) (any, error) {
	if req.Operation == "issue" {
		return s.issue(ctx, req)
	}
	return s.revoke(ctx, req.ProviderHandle)
}

func (s *Service) issue(ctx context.Context, req Request) (IssueResponse, error) {
	var response IssueResponse
	err := s.locked(ctx, func() error {
		digest := requestDigest(req)
		token := s.token(req)
		path := s.statePath(req.IssuanceID)
		state, exists, err := s.readState(path)
		if err != nil {
			return err
		}
		if exists && !hmac.Equal([]byte(state.RequestDigest), []byte(digest)) {
			return &ProtocolError{Code: "binding_conflict", Message: "issuance binding conflicts with durable state"}
		}
		if err := s.ensureUniqueToken(req.IssuanceID, hash(token)); err != nil {
			return err
		}
		if exists && state.Status == "issued" && s.now().Before(state.ExpiresAt) {
			response = s.response(state)
			return nil
		}
		if exists && (state.Status == "pending" || state.Status == "revoke_required" || state.Status == "issued") {
			if err := s.backend.Revoke(ctx, token); err != nil {
				return &ProtocolError{Code: "revoke_required", Message: "prior token cleanup is required"}
			}
		}
		state = durableState{SchemaVersion: SchemaVersion, Status: "pending", Request: req, RequestDigest: digest, TokenHash: hash(token)}
		if err := s.writeState(path, state); err != nil {
			return err
		}
		issued, err := s.backend.Issue(ctx, token, req.TTLSeconds)
		if err != nil {
			s.recordFailedIssue(ctx, path, &state, token)
			return &ProtocolError{Code: "microk8s_unavailable", Message: "MicroK8s credential issuance failed"}
		}
		if subtleTokenCheck(issued.TokenCheck, token) == false || !issued.ExpiresAt.After(s.now()) {
			s.recordFailedIssue(ctx, path, &state, token)
			return &ProtocolError{Code: "microk8s_unavailable", Message: "MicroK8s returned an invalid credential"}
		}
		urls := append([]string(nil), issued.URLs...)
		sort.Strings(urls)
		if len(urls) == 0 {
			s.recordFailedIssue(ctx, path, &state, token)
			return &ProtocolError{Code: "microk8s_unavailable", Message: "MicroK8s returned no worker endpoint"}
		}
		payload := credentialPayload{SchemaVersion: "blazn.dev/microk8s-worker-join/v1", IssuanceID: req.IssuanceID, ClusterID: req.ClusterID, ExpectedNodeName: req.ExpectedNodeName, BootstrapTaint: req.BootstrapTaint, WorkerOnly: true, ExpiresAt: issued.ExpiresAt.UTC(), URLs: urls}
		encoded, _ := json.Marshal(payload)
		state.Status = "issued"
		state.Credential = base64.RawURLEncoding.EncodeToString(encoded)
		state.ExpiresAt = issued.ExpiresAt.UTC()
		if err := s.writeState(path, state); err != nil {
			_ = s.backend.Revoke(ctx, token)
			return err
		}
		response = s.response(state)
		return nil
	})
	return response, err
}

func (s *Service) revoke(ctx context.Context, id string) (RevokeResponse, error) {
	err := s.locked(ctx, func() error {
		path := s.statePath(id)
		state, exists, err := s.readState(path)
		if err != nil {
			return err
		}
		if !exists || state.Status == "revoked" {
			return nil
		}
		token := s.token(state.Request)
		if err := s.backend.Revoke(ctx, token); err != nil {
			return &ProtocolError{Code: "revoke_required", Message: "token cleanup is required"}
		}
		state.Status = "revoked"
		state.Credential = ""
		return s.writeState(path, state)
	})
	return RevokeResponse{SchemaVersion: SchemaVersion, Operation: "revoke", ProviderHandle: id, Revoked: err == nil}, err
}
func (s *Service) response(st durableState) IssueResponse {
	return IssueResponse{SchemaVersion: SchemaVersion, Operation: "issue", ProviderHandle: st.Request.IssuanceID, Credential: st.Credential, ClusterID: st.Request.ClusterID, ClusterHealthy: true, WorkerOnly: true, ExpiresAt: st.ExpiresAt}
}
func (s *Service) token(r Request) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "blazn-microk8s-worker-token-v1\n%s\n%s\n%s\n%s\n%d\ntrue", r.IssuanceID, r.ClusterID, r.ExpectedNodeName, r.BootstrapTaint, r.TTLSeconds)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}
func requestDigest(r Request) string { raw, _ := json.Marshal(r); return hash(string(raw)) }
func hash(v string) string           { x := sha256.Sum256([]byte(v)); return hex.EncodeToString(x[:]) }
func subtleTokenCheck(check, token string) bool {
	parts := strings.Split(check, "/")
	return len(parts) == 2 && hmac.Equal([]byte(parts[0]), []byte(token)) && len(parts[1]) >= 16
}
func (s *Service) statePath(id string) string { return filepath.Join(s.stateRoot, id+".json") }
func (s *Service) locked(ctx context.Context, fn func() error) error {
	if err := os.MkdirAll(s.stateRoot, 0700); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(s.stateRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0077 != 0 || rootInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe issuer state root")
	}
	lock, err := os.OpenFile(filepath.Join(s.stateRoot, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Service) ensureUniqueToken(issuanceID, tokenHash string) error {
	entries, err := os.ReadDir(s.stateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == issuanceID+".json" {
			continue
		}
		state, exists, readErr := s.readState(filepath.Join(s.stateRoot, entry.Name()))
		if readErr != nil {
			return readErr
		}
		if exists && hmac.Equal([]byte(state.TokenHash), []byte(tokenHash)) {
			return &ProtocolError{Code: "token_collision", Message: "deterministic token collides with durable state"}
		}
	}
	return nil
}
func (s *Service) readState(path string) (durableState, bool, error) {
	var st durableState
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if info.Mode().IsRegular() == false || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return st, false, fmt.Errorf("unsafe issuer state")
	}
	file, err := os.Open(path)
	if err != nil {
		return st, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxMessageBytes+1))
	if err != nil || len(data) > MaxMessageBytes {
		return st, false, fmt.Errorf("issuer state is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&st); err != nil {
		return st, false, err
	}
	if err = s.validateState(st); err != nil {
		return st, false, err
	}
	return st, true, nil
}

func (s *Service) recordFailedIssue(ctx context.Context, path string, state *durableState, token string) {
	state.Status = "pending"
	if err := s.backend.Revoke(ctx, token); err != nil {
		state.Status = "revoke_required"
	}
	_ = s.writeState(path, *state)
}

func (s *Service) validateState(st durableState) error {
	if st.SchemaVersion != SchemaVersion || !oneOfState(st.Status) {
		return fmt.Errorf("issuer state is invalid")
	}
	raw, _ := json.Marshal(st.Request)
	req, err := DecodeRequest(raw)
	if err != nil || req.Operation != "issue" {
		return fmt.Errorf("issuer state request is invalid")
	}
	if !hmac.Equal([]byte(st.RequestDigest), []byte(requestDigest(req))) || !hmac.Equal([]byte(st.TokenHash), []byte(hash(s.token(req)))) {
		return fmt.Errorf("issuer state binding is invalid")
	}
	if st.Status == "issued" {
		if st.Credential == "" || st.ExpiresAt.IsZero() {
			return fmt.Errorf("issued state is incomplete")
		}
		return s.validateCredential(st)
	}
	if st.Credential != "" {
		return fmt.Errorf("non-issued state contains a credential")
	}
	return nil
}

func oneOfState(value string) bool {
	return value == "pending" || value == "issued" || value == "revoke_required" || value == "revoked"
}

func (s *Service) validateCredential(st durableState) error {
	data, err := base64.RawURLEncoding.DecodeString(st.Credential)
	if err != nil {
		return fmt.Errorf("issued credential is invalid")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || len(raw) != 8 {
		return fmt.Errorf("issued credential is invalid")
	}
	for _, key := range []string{"schemaVersion", "issuanceId", "clusterId", "expectedNodeName", "bootstrapTaint", "workerOnly", "expiresAt", "urls"} {
		if raw[key] == nil {
			return fmt.Errorf("issued credential is invalid")
		}
	}
	var payload credentialPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return fmt.Errorf("issued credential is invalid")
	}
	r := st.Request
	if payload.SchemaVersion != "blazn.dev/microk8s-worker-join/v1" || payload.IssuanceID != r.IssuanceID || payload.ClusterID != r.ClusterID || payload.ExpectedNodeName != r.ExpectedNodeName || payload.BootstrapTaint != r.BootstrapTaint || !payload.WorkerOnly || !payload.ExpiresAt.Equal(st.ExpiresAt) || len(payload.URLs) == 0 {
		return fmt.Errorf("issued credential binding is invalid")
	}
	prior := ""
	token := s.token(r)
	for _, candidate := range payload.URLs {
		if prior != "" && candidate <= prior {
			return fmt.Errorf("issued credential URLs are not canonical")
		}
		prior = candidate
		u, parseErr := url.Parse("https://" + candidate)
		if parseErr != nil {
			return fmt.Errorf("issued credential URL is invalid")
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 2 || !hmac.Equal([]byte(parts[0]), []byte(token)) || !validJoinURL(candidate, strings.Join(parts, "/")) {
			return fmt.Errorf("issued credential URL is invalid")
		}
	}
	return nil
}
func (s *Service) writeState(path string, st durableState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.stateRoot, ".state-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(s.stateRoot)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
