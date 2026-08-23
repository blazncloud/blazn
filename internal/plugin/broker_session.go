package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

type brokerRequest struct {
	SchemaVersion int             `json:"schemaVersion"`
	RequestID     string          `json:"requestId"`
	Method        string          `json:"method"`
	Params        json.RawMessage `json:"params"`
}
type brokerResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	RequestID     string       `json:"requestId"`
	OK            bool         `json:"ok"`
	ResultSchema  string       `json:"resultSchema,omitempty"`
	Payload       string       `json:"payload,omitempty"`
	Error         *brokerError `json:"error,omitempty"`
}
type brokerError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type brokerDescription struct {
	ProtocolVersion       int      `json:"protocolVersion"`
	Transport             string   `json:"transport"`
	MaxControlBytes       int      `json:"maxControlBytes"`
	MaxDataBytes          int      `json:"maxDataBytes"`
	MaxStreams            int      `json:"maxStreams"`
	AvailableCapabilities []string `json:"availableCapabilities"`
}

type brokerSession struct {
	connection *os.File
	pluginName string
	context    RuntimeContext
	writeMu    sync.Mutex
	seenMu     sync.Mutex
	seen       map[uint32]bool
}

func newBrokerSession(connection *os.File, pluginName string, runtimeContext RuntimeContext) *brokerSession {
	return &brokerSession{connection: connection, pluginName: pluginName, context: runtimeContext, seen: map[uint32]bool{}}
}
func (s *brokerSession) close() error { return s.connection.Close() }
func (s *brokerSession) cancel() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = writeBrokerFrame(s.connection, brokerFrame{Type: brokerFrameCancel, StreamID: brokerControlStreamID})
}
func (s *brokerSession) serve() error {
	for {
		frame, err := readBrokerFrame(s.connection)
		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if frame.Type != brokerFrameRequest {
			return errors.New("plugin sent a non-request control frame")
		}
		if frame.StreamID == brokerControlStreamID {
			return errors.New("plugin used the reserved broker control stream")
		}
		s.seenMu.Lock()
		duplicate := s.seen[frame.StreamID]
		overLimit := len(s.seen) >= brokerMaxStreams
		s.seen[frame.StreamID] = true
		s.seenMu.Unlock()
		if duplicate {
			return errors.New("plugin reused a broker stream ID")
		}
		if overLimit {
			return errors.New("plugin exceeded the broker stream limit")
		}
		response := s.handle(frame.Payload)
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		s.writeMu.Lock()
		err = writeBrokerFrame(s.connection, brokerFrame{Type: brokerFrameResponse, StreamID: frame.StreamID, Payload: encoded})
		s.writeMu.Unlock()
		if err != nil {
			return err
		}
	}
}
func (s *brokerSession) handle(payload []byte) brokerResponse {
	request, err := decodeBrokerRequest(payload)
	if err != nil {
		return brokerFailure("00000000000000000000000000000000", "invalid_request", err.Error(), false)
	}
	if request.Method != "broker.describe" {
		return brokerFailure(request.RequestID, "broker_method_unavailable", "broker method is not available in this runtime", false)
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(request.Params, &params) != nil || len(params) != 0 {
		return brokerFailure(request.RequestID, "invalid_request", "broker.describe params must be empty", false)
	}
	description := brokerDescription{ProtocolVersion: brokerProtocolVersion, Transport: "inherited-socket", MaxControlBytes: brokerMaxControlBytes, MaxDataBytes: brokerMaxDataBytes, MaxStreams: brokerMaxStreams, AvailableCapabilities: []string{"broker.describe"}}
	result, _ := json.Marshal(description)
	return brokerResponse{SchemaVersion: 1, RequestID: request.RequestID, OK: true, ResultSchema: "broker-description/v1", Payload: string(result)}
}
func decodeBrokerRequest(payload []byte) (brokerRequest, error) {
	if len(payload) == 0 || len(payload) > brokerMaxControlBytes {
		return brokerRequest{}, errors.New("broker request has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request brokerRequest
	if err := decoder.Decode(&request); err != nil {
		return brokerRequest{}, errors.New("broker request is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return brokerRequest{}, errors.New("broker request contains trailing data")
	}
	if request.SchemaVersion != 1 || !invocationIdentifier.MatchString(request.RequestID) || request.Method == "" || len(request.Params) == 0 {
		return brokerRequest{}, errors.New("broker request identity is invalid")
	}
	return request, nil
}
func brokerFailure(requestID, code, message string, retryable bool) brokerResponse {
	return brokerResponse{SchemaVersion: 1, RequestID: requestID, OK: false, Error: &brokerError{Code: code, Message: message, Retryable: retryable}}
}

func appendBrokerEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if name, _, _ := bytes.Cut([]byte(entry), []byte("=")); string(name) != BrokerFDEnvironment {
			result = append(result, entry)
		}
	}
	return append(result, BrokerFDEnvironment+"=3")
}
func brokerProcessError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("plugin broker failed: %w", err)
}
