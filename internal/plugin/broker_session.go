package plugin

import (
	"bytes"
	"context"
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
	connection     *os.File
	pluginName     string
	runtimeContext RuntimeContext
	requestContext context.Context
	handler        brokerMethodHandler
	writeMu        sync.Mutex
	seenMu         sync.Mutex
	seen           map[uint32]bool
	uploads        map[uint32]*brokerArtifactUpload
}

func newBrokerSession(connection *os.File, pluginName string, runtimeContext RuntimeContext) *brokerSession {
	return newBrokerSessionWithHandler(context.Background(), connection, pluginName, runtimeContext, describeOnlyBrokerHandler{})
}
func newBrokerSessionWithHandler(ctx context.Context, connection *os.File, pluginName string, runtimeContext RuntimeContext, handler brokerMethodHandler) *brokerSession {
	if ctx == nil {
		ctx = context.Background()
	}
	if handler == nil {
		handler = describeOnlyBrokerHandler{}
	}
	return &brokerSession{connection: connection, pluginName: pluginName, runtimeContext: runtimeContext, requestContext: ctx, handler: handler, seen: map[uint32]bool{}, uploads: map[uint32]*brokerArtifactUpload{}}
}
func (s *brokerSession) close() error { return s.connection.Close() }
func (s *brokerSession) cancel() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = writeBrokerFrame(s.connection, brokerFrame{Type: brokerFrameCancel, StreamID: brokerControlStreamID})
}
func (s *brokerSession) serve() error {
	defer s.abortUploads()
	for {
		frame, err := readBrokerFrame(s.connection)
		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if frame.StreamID == brokerControlStreamID {
			return errors.New("plugin used the reserved broker control stream")
		}
		switch frame.Type {
		case brokerFrameRequest:
			if err := s.acceptRequestStream(frame.StreamID); err != nil {
				return err
			}
			request, err := decodeBrokerRequest(frame.Payload)
			if err != nil {
				if err := s.writeResponse(frame.StreamID, brokerFailure("00000000000000000000000000000000", "invalid_request", err.Error(), false)); err != nil {
					return err
				}
				continue
			}
			if request.Method == "artifact.upload.begin" {
				handler, ok := s.handler.(*authenticatedBrokerHandler)
				if !ok {
					if err := s.writeResponse(frame.StreamID, brokerFailure(request.RequestID, "broker_method_unavailable", "broker method is not available in this runtime", false)); err != nil {
						return err
					}
					continue
				}
				ready, upload, failure := handler.beginArtifactUpload(s.requestContext, s.pluginName, s.runtimeContext, request)
				if failure != nil {
					if err := s.writeResponse(frame.StreamID, brokerFailure(request.RequestID, failure.Code, failure.Message, failure.Retryable)); err != nil {
						return err
					}
					continue
				}
				s.uploads[frame.StreamID] = upload
				if err := s.writeResponse(frame.StreamID, brokerSuccess(request.RequestID, "artifact-upload-ready/v1", ready)); err != nil {
					return err
				}
				continue
			}
			if err := s.writeResponse(frame.StreamID, s.handleRequest(request)); err != nil {
				return err
			}
		case brokerFrameData:
			upload := s.uploads[frame.StreamID]
			if upload == nil {
				return errors.New("plugin sent data on a non-upload stream")
			}
			if err := upload.write(frame.Payload); err != nil {
				return errors.New("plugin Artifact upload exceeded its declared bounds")
			}
		case brokerFrameEnd:
			upload := s.uploads[frame.StreamID]
			if upload == nil {
				return errors.New("plugin ended a non-upload stream")
			}
			if len(frame.Payload) != 0 {
				return errors.New("plugin Artifact end frame must be empty")
			}
			delete(s.uploads, frame.StreamID)
			value, failure := upload.complete(s.requestContext)
			if failure != nil {
				if err := s.writeResponse(frame.StreamID, brokerFailure(upload.requestID, failure.Code, failure.Message, failure.Retryable)); err != nil {
					return err
				}
				continue
			}
			if err := s.writeResponse(frame.StreamID, brokerSuccess(upload.requestID, resultArtifactEnvelope, value)); err != nil {
				return err
			}
		default:
			return errors.New("plugin sent an invalid broker frame")
		}
	}
}
func (s *brokerSession) acceptRequestStream(streamID uint32) error {
	s.seenMu.Lock()
	duplicate := s.seen[streamID]
	overLimit := len(s.seen) >= brokerMaxStreams
	s.seen[streamID] = true
	s.seenMu.Unlock()
	if duplicate {
		return errors.New("plugin reused a broker stream ID")
	}
	if overLimit {
		return errors.New("plugin exceeded the broker stream limit")
	}
	return nil
}
func (s *brokerSession) writeResponse(streamID uint32, response brokerResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	err = writeBrokerFrame(s.connection, brokerFrame{Type: brokerFrameResponse, StreamID: streamID, Payload: encoded})
	s.writeMu.Unlock()
	return err
}
func (s *brokerSession) handle(payload []byte) brokerResponse {
	request, err := decodeBrokerRequest(payload)
	if err != nil {
		return brokerFailure("00000000000000000000000000000000", "invalid_request", err.Error(), false)
	}
	return s.handleRequest(request)
}
func (s *brokerSession) handleRequest(request brokerRequest) brokerResponse {
	resultSchema, value, failure := s.handler.Handle(s.requestContext, s.pluginName, s.runtimeContext, request)
	if failure != nil {
		return brokerFailure(request.RequestID, failure.Code, failure.Message, failure.Retryable)
	}
	result, err := json.Marshal(value)
	if err != nil || len(result) == 0 || len(result) > brokerMaxControlBytes {
		return brokerFailure(request.RequestID, "broker_response_invalid", "broker response could not be encoded safely", false)
	}
	return brokerResponse{SchemaVersion: 1, RequestID: request.RequestID, OK: true, ResultSchema: resultSchema, Payload: string(result)}
}
func brokerSuccess(requestID, resultSchema string, value any) brokerResponse {
	result, err := json.Marshal(value)
	if err != nil || len(result) == 0 || len(result) > brokerMaxControlBytes {
		return brokerFailure(requestID, "broker_response_invalid", "broker response could not be encoded safely", false)
	}
	return brokerResponse{SchemaVersion: 1, RequestID: requestID, OK: true, ResultSchema: resultSchema, Payload: string(result)}
}
func (s *brokerSession) abortUploads() {
	for stream, upload := range s.uploads {
		upload.abort()
		delete(s.uploads, stream)
	}
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
