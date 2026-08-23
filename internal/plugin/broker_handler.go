package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

const (
	resultBrokerDescription = "broker-description/v1"
	resultProjectEnvelope   = "project-envelope/v1"
	resultRunEnvelope       = "run-envelope/v1"
	resultRunList           = "run-list/v1"
	resultArtifactEnvelope  = "artifact-envelope/v1"
	resultArtifactList      = "artifact-list/v1"
)

var contentBrokerCapabilities = []string{"artifact.read", "broker.describe", "project.read", "run.cancel", "run.create", "run.read"}

var (
	brokerUUIDPattern       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	brokerDigestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	brokerRunKindPattern    = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,95}$`)
	brokerOutputNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type brokerMethodHandler interface {
	Handle(context.Context, string, RuntimeContext, brokerRequest) (string, any, *brokerError)
}

type describeOnlyBrokerHandler struct{}

func (describeOnlyBrokerHandler) Handle(_ context.Context, _ string, _ RuntimeContext, request brokerRequest) (string, any, *brokerError) {
	if request.Method != "broker.describe" {
		return "", nil, brokerMethodFailure("broker_method_unavailable", "broker method is not available in this runtime", false)
	}
	if err := decodeBrokerParams(request.Params, &struct{}{}); err != nil {
		return "", nil, brokerMethodFailure("invalid_request", "broker.describe params must be empty", false)
	}
	return resultBrokerDescription, newBrokerDescription([]string{"broker.describe"}), nil
}

type brokerAPI interface {
	GetProject(context.Context, string, string, string) (client.ProjectEnvelope, error)
	CreateRun(context.Context, string, string, string, string, client.CreateRunRequest) (client.RunEnvelope, error)
	ListRuns(context.Context, string, string, string, string, string) (client.RunList, error)
	GetRun(context.Context, string, string, string, string) (client.RunEnvelope, error)
	CancelRun(context.Context, string, string, string, string, string, client.CancelRunRequest) (client.RunEnvelope, error)
	ListArtifacts(context.Context, string, string, string, string, string) (client.ArtifactList, error)
	GetArtifact(context.Context, string, string, string, string) (client.ArtifactEnvelope, error)
}

type brokerSessionProvider interface {
	Session(context.Context, bool) (workspacepkg.Session, error)
	Origin() string
}

type brokerAuthority struct {
	api      brokerAPI
	sessions brokerSessionProvider
}

type authenticatedBrokerHandler struct {
	runtimeContext RuntimeContext
	once           sync.Once
	authority      *brokerAuthority
	initializeErr  error
	initialize     func(RuntimeContext) (*brokerAuthority, error)
}

func newDefaultBrokerHandler(runtimeContext RuntimeContext) brokerMethodHandler {
	return &authenticatedBrokerHandler{runtimeContext: runtimeContext, initialize: newDefaultBrokerAuthority}
}

func newDefaultBrokerAuthority(runtimeContext RuntimeContext) (*brokerAuthority, error) {
	if runtimeContext.Status != "selected" || runtimeContext.ProjectID == "" {
		return nil, errors.New("an authenticated Workspace Project is not selected")
	}
	sessions, err := workspacepkg.NewDefaultSessionProvider()
	if err != nil {
		return nil, err
	}
	if sessions.Origin() != runtimeContext.APIOrigin {
		return nil, errors.New("selected API origin changed during plugin dispatch")
	}
	api, err := client.New(runtimeContext.APIOrigin, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	return &brokerAuthority{api: api, sessions: sessions}, nil
}

func (h *authenticatedBrokerHandler) Handle(ctx context.Context, pluginName string, runtimeContext RuntimeContext, request brokerRequest) (string, any, *brokerError) {
	if request.Method == "broker.describe" {
		if err := decodeBrokerParams(request.Params, &struct{}{}); err != nil {
			return "", nil, brokerMethodFailure("invalid_request", "broker.describe params must be empty", false)
		}
		capabilities := []string{"broker.describe"}
		if pluginName == "content" && runtimeContext.Status == "selected" && runtimeContext.ProjectID != "" {
			capabilities = append([]string(nil), contentBrokerCapabilities...)
		}
		return resultBrokerDescription, newBrokerDescription(capabilities), nil
	}
	capability, ok := brokerMethodCapability(request.Method)
	if !ok || pluginName != "content" || !containsBrokerCapability(contentBrokerCapabilities, capability) {
		return "", nil, brokerMethodFailure("broker_method_unavailable", "broker method is not available in this runtime", false)
	}
	if runtimeContext != h.runtimeContext || runtimeContext.Status != "selected" || runtimeContext.ProjectID == "" {
		return "", nil, brokerMethodFailure("broker_context_unavailable", "an authenticated Workspace Project is required", false)
	}
	h.once.Do(func() { h.authority, h.initializeErr = h.initialize(runtimeContext) })
	if h.initializeErr != nil || h.authority == nil {
		return "", nil, brokerMethodFailure("broker_context_unavailable", "authenticated broker authority is unavailable", false)
	}
	return h.handleAuthenticated(ctx, pluginName, request)
}

func (h *authenticatedBrokerHandler) handleAuthenticated(ctx context.Context, pluginName string, request brokerRequest) (string, any, *brokerError) {
	c := h.runtimeContext
	switch request.Method {
	case "project.get":
		if err := decodeBrokerParams(request.Params, &struct{}{}); err != nil {
			return invalidBrokerParams()
		}
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.ProjectEnvelope, error) {
			return h.authority.api.GetProject(ctx, token, c.WorkspaceID, c.ProjectID)
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		if value.Project.ID != c.ProjectID || value.Project.WorkspaceID != c.WorkspaceID {
			return invalidBrokerResult()
		}
		return resultProjectEnvelope, value, nil
	case "run.create":
		var params brokerRunCreateParams
		if err := decodeBrokerParams(request.Params, &params); err != nil || !validBrokerRunCreateParams(params) {
			return invalidBrokerParams()
		}
		key := scopedBrokerIdempotencyKey(c, pluginName, request)
		input := client.CreateRunRequest{Kind: params.Kind, ProofClass: params.ProofClass, PlanDigest: params.PlanDigest, InputArtifactIDs: params.InputArtifactIDs, OutputNames: params.OutputNames}
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.RunEnvelope, error) {
			return h.authority.api.CreateRun(ctx, token, c.WorkspaceID, c.ProjectID, key, input)
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		if !validBrokerRun(value.Run, c) || value.Run.ProofClass != client.ProofClassSynthetic || value.Run.PlanDigest != params.PlanDigest {
			return invalidBrokerResult()
		}
		return resultRunEnvelope, value, nil
	case "run.list":
		var params brokerListParams
		if err := decodeBrokerParams(request.Params, &params); err != nil || !validBrokerRunListParams(params) {
			return invalidBrokerParams()
		}
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.RunList, error) {
			return h.authority.api.ListRuns(ctx, token, c.WorkspaceID, c.ProjectID, params.Status, params.Cursor)
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		for _, item := range value.Items {
			if !validBrokerRun(item, c) {
				return invalidBrokerResult()
			}
		}
		return resultRunList, value, nil
	case "run.get":
		var params brokerRunIdentityParams
		if err := decodeBrokerParams(request.Params, &params); err != nil || !brokerUUIDPattern.MatchString(params.RunID) {
			return invalidBrokerParams()
		}
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.RunEnvelope, error) {
			return h.authority.api.GetRun(ctx, token, c.WorkspaceID, c.ProjectID, params.RunID)
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		if value.Run.ID != params.RunID || !validBrokerRun(value.Run, c) {
			return invalidBrokerResult()
		}
		return resultRunEnvelope, value, nil
	case "run.cancel":
		var params brokerRunCancelParams
		if err := decodeBrokerParams(request.Params, &params); err != nil || !brokerUUIDPattern.MatchString(params.RunID) || params.ExpectedVersion < 1 || !validBrokerIdempotencyKey(params.IdempotencyKey) {
			return invalidBrokerParams()
		}
		key := scopedBrokerIdempotencyKey(c, pluginName, request)
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.RunEnvelope, error) {
			return h.authority.api.CancelRun(ctx, token, c.WorkspaceID, c.ProjectID, params.RunID, key, client.CancelRunRequest{ExpectedVersion: params.ExpectedVersion})
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		if value.Run.ID != params.RunID || !validBrokerRun(value.Run, c) {
			return invalidBrokerResult()
		}
		return resultRunEnvelope, value, nil
	case "artifact.list":
		var params brokerListParams
		if err := decodeBrokerParams(request.Params, &params); err != nil || !validBrokerArtifactListParams(params) {
			return invalidBrokerParams()
		}
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.ArtifactList, error) {
			return h.authority.api.ListArtifacts(ctx, token, c.WorkspaceID, c.ProjectID, params.Status, params.Cursor)
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		for _, item := range value.Items {
			if !validBrokerArtifact(item, c) {
				return invalidBrokerResult()
			}
		}
		return resultArtifactList, value, nil
	case "artifact.get":
		var params brokerArtifactIdentityParams
		if err := decodeBrokerParams(request.Params, &params); err != nil || !brokerUUIDPattern.MatchString(params.ArtifactID) {
			return invalidBrokerParams()
		}
		value, err := brokerWithSession(ctx, h.authority, c, func(token string) (client.ArtifactEnvelope, error) {
			return h.authority.api.GetArtifact(ctx, token, c.WorkspaceID, c.ProjectID, params.ArtifactID)
		})
		if err != nil {
			return brokerCallFailure(err)
		}
		if value.Artifact.ID != params.ArtifactID || !validBrokerArtifact(value.Artifact, c) {
			return invalidBrokerResult()
		}
		return resultArtifactEnvelope, value, nil
	default:
		return "", nil, brokerMethodFailure("broker_method_unavailable", "broker method is not available in this runtime", false)
	}
}

type brokerRunCreateParams struct {
	Kind             string            `json:"kind"`
	ProofClass       client.ProofClass `json:"proofClass"`
	PlanDigest       string            `json:"planDigest"`
	InputArtifactIDs []string          `json:"inputArtifactIds"`
	OutputNames      []string          `json:"outputNames"`
	IdempotencyKey   string            `json:"idempotencyKey"`
}
type brokerListParams struct {
	Status string `json:"status,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}
type brokerRunIdentityParams struct {
	RunID string `json:"runId"`
}
type brokerRunCancelParams struct {
	RunID           string `json:"runId"`
	ExpectedVersion int    `json:"expectedVersion"`
	IdempotencyKey  string `json:"idempotencyKey"`
}
type brokerArtifactIdentityParams struct {
	ArtifactID string `json:"artifactId"`
}

func newBrokerDescription(capabilities []string) brokerDescription {
	values := append([]string(nil), capabilities...)
	sort.Strings(values)
	return brokerDescription{ProtocolVersion: brokerProtocolVersion, Transport: "inherited-socket", MaxControlBytes: brokerMaxControlBytes, MaxDataBytes: brokerMaxDataBytes, MaxStreams: brokerMaxStreams, AvailableCapabilities: values}
}

func brokerMethodCapability(method string) (string, bool) {
	switch method {
	case "project.get":
		return "project.read", true
	case "run.create":
		return "run.create", true
	case "run.list", "run.get":
		return "run.read", true
	case "run.cancel":
		return "run.cancel", true
	case "artifact.list", "artifact.get":
		return "artifact.read", true
	default:
		return "", false
	}
}
func containsBrokerCapability(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func validBrokerIdempotencyKey(value string) bool { return len(value) >= 8 && len(value) <= 128 }

func validBrokerRunCreateParams(params brokerRunCreateParams) bool {
	if params.ProofClass != client.ProofClassSynthetic || !brokerRunKindPattern.MatchString(params.Kind) || !brokerDigestPattern.MatchString(params.PlanDigest) || !validBrokerIdempotencyKey(params.IdempotencyKey) || params.InputArtifactIDs == nil || params.OutputNames == nil || len(params.InputArtifactIDs) > 1000 || len(params.OutputNames) > 1000 {
		return false
	}
	seenIDs := map[string]bool{}
	for _, id := range params.InputArtifactIDs {
		if !brokerUUIDPattern.MatchString(id) || seenIDs[id] {
			return false
		}
		seenIDs[id] = true
	}
	seenNames := map[string]bool{}
	for _, name := range params.OutputNames {
		if !brokerOutputNamePattern.MatchString(name) || seenNames[name] {
			return false
		}
		seenNames[name] = true
	}
	return true
}

func validBrokerRunListParams(params brokerListParams) bool {
	return len(params.Cursor) <= 512 && (params.Status == "" || params.Status == "queued" || params.Status == "running" || params.Status == "succeeded" || params.Status == "failed" || params.Status == "cancelled" || params.Status == "all")
}

func validBrokerArtifactListParams(params brokerListParams) bool {
	return len(params.Cursor) <= 512 && (params.Status == "" || params.Status == "pending" || params.Status == "ready" || params.Status == "failed" || params.Status == "deleted" || params.Status == "all")
}

func decodeBrokerParams(raw json.RawMessage, output any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return errors.New("broker params must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("broker params contain trailing data")
	}
	return nil
}

func brokerWithSession[T any](ctx context.Context, authority *brokerAuthority, runtimeContext RuntimeContext, action func(string) (T, error)) (T, error) {
	var zero T
	session, err := authority.sessions.Session(ctx, false)
	if err != nil {
		return zero, err
	}
	if session.UserID != runtimeContext.UserID {
		return zero, errors.New("authenticated principal changed during plugin invocation")
	}
	value, err := action(session.AccessToken)
	if client.IsCode(err, "access_expired") {
		session, err = authority.sessions.Session(ctx, true)
		if err != nil {
			return zero, err
		}
		if session.UserID != runtimeContext.UserID {
			return zero, errors.New("authenticated principal changed during plugin invocation")
		}
		return action(session.AccessToken)
	}
	return value, err
}

func scopedBrokerIdempotencyKey(runtimeContext RuntimeContext, pluginName string, request brokerRequest) string {
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "blazn-plugin-broker-idempotency-v1\n%s\n%s\n%s\n%s\n%s\n", runtimeContext.UserID, runtimeContext.WorkspaceID, runtimeContext.ProjectID, pluginName, request.Method)
	digest.Write(request.Params)
	return "broker-v1-" + hex.EncodeToString(digest.Sum(nil))
}

func validBrokerRun(value client.Run, runtimeContext RuntimeContext) bool {
	return value.ID != "" && value.WorkspaceID == runtimeContext.WorkspaceID && value.ProjectID == runtimeContext.ProjectID
}
func validBrokerArtifact(value client.Artifact, runtimeContext RuntimeContext) bool {
	return value.ID != "" && value.WorkspaceID == runtimeContext.WorkspaceID && value.ProjectID == runtimeContext.ProjectID
}

func brokerMethodFailure(code, message string, retryable bool) *brokerError {
	return &brokerError{Code: code, Message: message, Retryable: retryable}
}
func invalidBrokerParams() (string, any, *brokerError) {
	return "", nil, brokerMethodFailure("invalid_request", "broker method params are invalid", false)
}
func invalidBrokerResult() (string, any, *brokerError) {
	return "", nil, brokerMethodFailure("broker_response_invalid", "root API returned an invalid scoped result", false)
}
func brokerCallFailure(err error) (string, any, *brokerError) { return "", nil, mapBrokerError(err) }

func mapBrokerError(err error) *brokerError {
	if errors.Is(err, context.Canceled) {
		return brokerMethodFailure("cancelled", "broker request was cancelled", false)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return brokerMethodFailure("deadline_exceeded", "broker request deadline was exceeded", true)
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized:
			return brokerMethodFailure("not_authenticated", "Blazn authentication is required", false)
		case http.StatusForbidden, http.StatusNotFound:
			return brokerMethodFailure("resource_unavailable", "selected resource is unavailable", false)
		case http.StatusTooManyRequests:
			return brokerMethodFailure("rate_limited", "broker request was rate limited", true)
		}
		if apiErr.StatusCode >= 500 {
			return brokerMethodFailure("broker_backend_unavailable", "Blazn API is unavailable", true)
		}
		if runtimeIdentifier.MatchString(apiErr.Body.Code) && apiErr.Body.Message != "" && len(apiErr.Body.Message) <= 1024 {
			return brokerMethodFailure(apiErr.Body.Code, apiErr.Body.Message, false)
		}
	}
	return brokerMethodFailure("broker_backend_unavailable", "broker request could not be completed", true)
}
