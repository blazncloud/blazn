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
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready"})
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
	var normalized proxycontract.NormalizedRequest
	var err error
	switch request.URL.Path {
	case "/v1/chat/completions":
		normalized, err = normalizeChat(limited, h.config.Policy, h.config.Now())
	case "/v1/responses":
		normalized, err = normalizeResponses(limited, h.config.Policy, h.config.Now())
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
	if err = ensureContextLimit(normalized, h.config.Policy); err != nil {
		writeError(writer, err)
		return
	}
	routes, err := h.routes.selectRoutes(normalized)
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := h.dispatch(request.Context(), normalized, routes)
	if err != nil {
		if errors.Is(request.Context().Err(), context.Canceled) {
			h.emit(normalized, routes[0], 1, proxycontract.EventRequestCancelled, proxycontract.OutcomeCancelled, proxycontract.EventReasonCancelled, 0, nil)
		}
		writeError(writer, err)
		return
	}
	h.emit(normalized, result.route, result.attempt, proxycontract.EventRouteSelected, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, 0, nil)
	if normalized.Stream {
		err = streamResponse(request.Context(), writer, result, normalized)
		if errors.Is(err, request.Context().Err()) {
			h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestCancelled, proxycontract.OutcomeCancelled, proxycontract.EventReasonCancelled, 0, nil)
		}
		return
	}
	response, err := decodeUpstreamResponse(result, normalized)
	if err != nil {
		writeError(writer, err)
		return
	}
	if err = writeSourceResponse(writer, normalized.Protocol, response); err != nil {
		return
	}
	h.emit(normalized, result.route, result.attempt, proxycontract.EventRequestFinished, proxycontract.OutcomeSuccess, proxycontract.EventReasonNone, 0, &response.Usage)
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
