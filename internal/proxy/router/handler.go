package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.URL.Path == "/healthz" {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		status, code := "not_ready", http.StatusServiceUnavailable
		if h.ready(request.Context()) {
			status, code = "ready", http.StatusOK
		}
		writer.WriteHeader(code)
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": status})
		return
	}
	if !authenticateAndStrip(request.Header, h.config.ListenerToken) {
		writeError(writer, safeError("authentication_failed", "listener authentication failed", http.StatusUnauthorized, false))
		return
	}
	if request.URL.Path == "/v1/models" {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.writeModels(writer)
		return
	}
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if media := request.Header.Get("Content-Type"); media != "" && !strings.HasPrefix(strings.ToLower(media), "application/json") {
		writeError(writer, safeError("invalid_request", "Content-Type must be application/json", 415, false))
		return
	}
	limited := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	defer limited.Close()
	var routed routedRequest
	var err error
	switch request.URL.Path {
	case "/v1/chat/completions":
		routed, err = normalizeChatIncoming(limited, h.config.Policy, h.config.Now())
	case "/v1/responses":
		routed, err = normalizeResponsesIncoming(limited, h.config.Policy, h.config.Now())
	case "/v1/messages":
		writeError(writer, unsupported("Anthropic source handling is provided by the isolated Anthropic lane"))
		return
	default:
		http.NotFound(writer, request)
		return
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	normalized := routed.normalized
	if err = ensureContextLimit(routed, h.config.Policy); err != nil {
		writeError(writer, err)
		return
	}
	routes, err := h.routes.selectRoutesFor(routed)
	if err != nil {
		writeError(writer, err)
		return
	}
	h.emit(normalized, routes[0], 1, proxycontract.EventRequestStarted, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, 0, nil)
	deadline, parseErr := time.Parse(time.RFC3339, normalized.Limits.DeadlineAt)
	if parseErr != nil {
		writeError(writer, safeError("invalid_request", "request deadline is invalid", 400, false))
		return
	}
	remaining := deadline.Sub(h.config.Now())
	if remaining <= 0 {
		writeError(writer, safeError("timeout_before_first_byte", "request deadline elapsed", 504, false))
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), remaining)
	defer cancel()
	started := h.config.Now()
	result, err := h.dispatch(ctx, routed, routes)
	if err != nil {
		failureRoute, failureAttempt := routes[0], 1
		if result.route.ID != "" {
			failureRoute, failureAttempt = result.route, result.attempt
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			h.emit(normalized, failureRoute, failureAttempt, proxycontract.EventRequestCancelled, proxycontract.OutcomeCancelled, proxycontract.EventReasonCancelled, h.config.Now().Sub(started), nil)
		} else {
			h.emit(normalized, failureRoute, failureAttempt, proxycontract.EventRequestFinished, proxycontract.OutcomeFailed, reasonForError(err), h.config.Now().Sub(started), nil)
		}
		writeError(writer, err)
		return
	}
	if normalized.Stream {
		usage, err := streamResponse(ctx, writer, result, normalized)
		latency := h.config.Now().Sub(started)
		if errors.Is(err, ctx.Err()) {
			h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestCancelled, proxycontract.OutcomeCancelled, proxycontract.EventReasonCancelled, latency, nil)
		} else if err == nil {
			h.emit(normalized, result.route, result.attempt, proxycontract.EventAttemptFinished, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, h.config.Now().Sub(result.attemptStarted), usage)
			h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestFinished, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, latency, usage)
		} else {
			h.emit(normalized, result.route, result.attempt, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, reasonForError(err), h.config.Now().Sub(result.attemptStarted), usage)
			h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestFinished, proxycontract.OutcomeFailed, reasonForError(err), latency, usage)
		}
		return
	}
	var responseMeta responseMetadata
	response, err := decodeUpstreamResponseDetailed(result, normalized, &responseMeta)
	if err != nil {
		h.emit(normalized, result.route, result.attempt, proxycontract.EventAttemptFinished, proxycontract.OutcomeFailed, reasonForError(err), h.config.Now().Sub(result.attemptStarted), nil)
		h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestFinished, proxycontract.OutcomeFailed, reasonForError(err), h.config.Now().Sub(started), nil)
		writeError(writer, err)
		return
	}
	if normalized.Protocol == proxycontract.ProtocolOpenAIResponses && response.FinishReason == "length" && responseMeta.status == "" {
		responseMeta.status = "incomplete"
		responseMeta.incompleteDetails = json.RawMessage(`{"reason":"max_output_tokens"}`)
	}
	if err = writeSourceResponseDetailed(writer, normalized.Protocol, response, responseMeta); err != nil {
		return
	}
	latency := h.config.Now().Sub(started)
	h.emit(normalized, result.route, result.attempt, proxycontract.EventAttemptFinished, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, h.config.Now().Sub(result.attemptStarted), &response.Usage)
	h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestFinished, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, latency, &response.Usage)
}

func (h *Handler) ready(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	return h.Preflight(ctx) == nil
}

// Preflight is the activation gate: every route referenced by a frozen alias
// must resolve and have a destination credential before listener publication.
func (h *Handler) Preflight(ctx context.Context) error {
	required := map[string]bool{}
	for _, alias := range h.config.Policy.Aliases {
		for _, id := range alias.RouteIDs {
			required[id] = true
		}
	}
	if len(required) == 0 {
		return errors.New("no policy routes are required")
	}
	for _, route := range h.config.Policy.Routes {
		if !required[route.ID] {
			continue
		}
		resolved, err := h.config.Resolver.Resolve(ctx, route)
		if err != nil || len(resolved.Addresses) == 0 {
			return fmt.Errorf("route %s is not resolvable", route.ID)
		}
		credential, err := h.config.Credentials.DestinationCredential(ctx, route.CredentialRef)
		if err != nil || credential == "" {
			return fmt.Errorf("route %s credential is unavailable", route.ID)
		}
		delete(required, route.ID)
	}
	if len(required) != 0 {
		return errors.New("one or more required policy routes are missing")
	}
	return nil
}

func reasonForError(err error) proxycontract.EventReasonCode {
	var api *APIError
	if !errors.As(err, &api) {
		return proxycontract.EventReasonPolicyDenied
	}
	switch api.Code {
	case "cancelled":
		return proxycontract.EventReasonCancelled
	case "authentication_failed", "credential_unavailable":
		return proxycontract.EventReasonAuthenticationFailed
	case "connection_failure":
		return proxycontract.EventReasonConnectionFailure
	case "timeout_before_first_byte":
		return proxycontract.EventReasonTimeoutBeforeFirstByte
	case "rate_limited":
		return proxycontract.EventReasonRateLimited
	case "upstream_5xx":
		return proxycontract.EventReasonUpstream5xx
	case "model_unavailable":
		return proxycontract.EventReasonModelUnavailable
	case "context_overflow":
		return proxycontract.EventReasonCompatibleContextOverflow
	case "unsupported_capability":
		return proxycontract.EventReasonUnsupportedCapability
	default:
		return proxycontract.EventReasonPolicyDenied
	}
}

func (h *Handler) writeModels(writer http.ResponseWriter) {
	names := make([]string, 0, len(h.config.Policy.Aliases))
	for name := range h.config.Policy.Aliases {
		names = append(names, name)
	}
	slicesSort(names)
	models := make([]any, 0, len(names))
	for _, name := range names {
		models = append(models, map[string]any{"id": name, "object": "model", "owned_by": "blazn-policy"})
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": models})
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (h *Handler) emit(request proxycontract.NormalizedRequest, route proxycontract.Route, attempt int, eventType proxycontract.EventType, outcome proxycontract.EventOutcome, reason proxycontract.EventReasonCode, latency time.Duration, usage *proxycontract.Usage) {
	cursor := h.cursor.Add(1)
	event := proxycontract.Event{EventID: newUUID(), Cursor: strconv.FormatUint(cursor, 10), Timestamp: h.config.Now().UTC().Format(time.RFC3339), Type: eventType, ActivationID: h.config.ActivationID, LogicalRequestID: request.LogicalRequestID, Attempt: attempt, Protocol: request.Protocol, ModelAlias: request.ModelAlias, Policy: proxycontract.PolicyIdentity{ID: h.config.Policy.ID, Version: h.config.Policy.Version, Digest: h.config.PolicyDigest}, RouteID: route.ID, DestinationClass: route.DestinationClass, Outcome: outcome, ReasonCode: reason, LatencyMS: max(0, int(latency.Milliseconds())), Usage: usage}
	if event.Validate() == nil {
		h.config.Events.Emit(event)
	}
}

func eventReason(reason proxycontract.RetryableReason) proxycontract.EventReasonCode {
	return proxycontract.EventReasonCode(reason)
}

func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded[:])
}
