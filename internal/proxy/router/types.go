package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

const maxRequestBytes = 8 << 20

type APIError struct {
	Code      string
	Message   string
	Status    int
	Retryable bool
	Reason    proxycontract.RetryableReason
	Cause     error
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }
func (e *APIError) Unwrap() error { return e.Cause }

func safeError(code, message string, status int, retryable bool) *APIError {
	return &APIError{Code: code, Message: message, Status: status, Retryable: retryable}
}

func writeError(writer http.ResponseWriter, err error) {
	api := &APIError{Code: "internal_error", Message: "proxy request failed", Status: http.StatusInternalServerError}
	if errors.As(err, &api) {
		// api is already safe for the caller.
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(api.Status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{"code": api.Code, "message": api.Message, "type": "blazn_proxy_error"}})
}

type EventSink interface{ Emit(proxycontract.Event) }
type EventSinkFunc func(proxycontract.Event)

func (f EventSinkFunc) Emit(event proxycontract.Event) { f(event) }

type Config struct {
	Policy          proxycontract.Policy
	PolicyDigest    string
	ActivationID    string
	ListenerToken   string
	Credentials     CredentialProvider
	CredentialApply CredentialAdapter
	Resolver        EndpointResolver
	ClientFactory   func(proxycontract.Route, ResolvedEndpoint) *http.Client
	Events          EventSink
	Now             func() time.Time
}

type Handler struct {
	config Config
	routes *routeIndex
	cursor atomic.Uint64
}

func NewHandler(config Config) (*Handler, error) {
	if config.ListenerToken == "" {
		return nil, errors.New("listener token is required")
	}
	if config.Credentials == nil {
		return nil, errors.New("credential provider is required")
	}
	if config.CredentialApply == nil {
		config.CredentialApply = BearerCredentialAdapter{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Events == nil {
		config.Events = EventSinkFunc(func(proxycontract.Event) {})
	}
	if config.PolicyDigest == "" {
		var err error
		config.PolicyDigest, err = proxycontract.ContractDigest(config.Policy)
		if err != nil {
			return nil, err
		}
	}
	index, err := newRouteIndex(config.Policy)
	if err != nil {
		return nil, fmt.Errorf("POLICY_INVALID: %w", err)
	}
	return &Handler{config: config, routes: index}, nil
}
