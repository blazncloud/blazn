package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/blazncloud/blazn/internal/client"
)

const (
	maximumExecResponseBytes = 16 << 20
	maximumEventDataBytes    = 1 << 20
)

// strictTransport owns the two response paths whose generated typed decoder
// cannot distinguish a required zero value from an omitted JSON property.
type strictTransport struct {
	baseURL *url.URL
	http    *http.Client
}

func newStrictTransport(baseURL string, httpClient *http.Client) (*strictTransport, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("API URL must contain an HTTP(S) scheme, host, and optional base path")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	previousRedirect := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !strings.EqualFold(request.URL.Scheme, parsed.Scheme) || !strings.EqualFold(request.URL.Host, parsed.Host) {
			return errors.New("refusing cross-origin API redirect")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &strictTransport{baseURL: parsed, http: &clientCopy}, nil
}

func (t *strictTransport) endpoint(path string) string {
	endpoint := *t.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawQuery = ""
	return endpoint.String()
}

func (t *strictTransport) ExecuteSandboxGrant(ctx context.Context, grantID, grantToken string, request client.SandboxExecRequest) (client.SandboxExecResult, error) {
	var result client.SandboxExecResult
	body, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint("/v1/sandbox-access-grants/"+url.PathEscape(grantID)+"/exec"), bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Blazn-Grant "+grantToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, decodeStrictAPIError(resp)
	}
	encoded, err := readBounded(resp.Body, maximumExecResponseBytes)
	if err != nil {
		return result, fmt.Errorf("decode sandbox exec result: %w", err)
	}
	if err := decodeStrictObject(encoded, []string{"remoteExitCode", "stdoutBase64", "stderrBase64", "truncated"}, nil, &result); err != nil {
		return client.SandboxExecResult{}, fmt.Errorf("decode sandbox exec result: %w", err)
	}
	return result, nil
}

func (t *strictTransport) StreamSandboxEvents(ctx context.Context, accessToken, sandboxID, lastEventID string) (EventStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint("/v1/sandboxes/"+url.PathEscape(sandboxID)+"/events"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decodeStrictAPIError(resp)
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body.Close()
		return nil, errors.New("sandbox event API returned unsupported content type")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), maximumEventDataBytes)
	return &strictEventStream{body: resp.Body, scanner: scanner}, nil
}

type strictEventStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

func (s *strictEventStream) Close() error { return s.body.Close() }

func (s *strictEventStream) Next() (client.SandboxEvent, error) {
	var event client.SandboxEvent
	var eventID string
	var data strings.Builder
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			if err := decodeStrictObject([]byte(data.String()), []string{"eventId", "sandboxId", "operationId", "sequence", "type", "payload", "createdAt"}, map[string]bool{"operationId": true}, &event); err != nil {
				return event, fmt.Errorf("decode sandbox event: %w", err)
			}
			if eventID == "" || event.EventID != eventID {
				return event, errors.New("sandbox SSE id does not match typed eventId")
			}
			return event, nil
		}
		if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if data.Len() > maximumEventDataBytes {
				return event, errors.New("sandbox event data is too large")
			}
		}
	}
	if err := s.scanner.Err(); err != nil {
		return event, err
	}
	return event, io.EOF
}

func decodeStrictObject(encoded []byte, required []string, nullable map[string]bool, output any) error {
	if err := rejectDuplicateJSONNames(encoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := decodeOneJSON(encoded, &fields, false); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("response must be a JSON object")
	}
	for _, name := range required {
		raw, present := fields[name]
		if !present {
			return fmt.Errorf("required property %q is missing", name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !nullable[name] {
			return fmt.Errorf("required property %q must not be null", name)
		}
	}
	return decodeOneJSON(encoded, output, true)
}

func decodeOneJSON(encoded []byte, output any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("response must contain exactly one JSON document")
		}
		return fmt.Errorf("response contains trailing data: %w", err)
	}
	return nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	encoded, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maximum {
		return nil, errors.New("response is too large")
	}
	return encoded, nil
}

func decodeStrictAPIError(resp *http.Response) error {
	encoded, err := readBounded(resp.Body, 1<<20)
	if err != nil {
		return fmt.Errorf("sandbox API returned HTTP %d", resp.StatusCode)
	}
	var body client.ErrorBody
	if err := decodeStrictObject(encoded, []string{"code", "message", "requestId"}, nil, &body); err != nil {
		return fmt.Errorf("sandbox API returned HTTP %d with invalid error body", resp.StatusCode)
	}
	retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	return &client.APIError{StatusCode: resp.StatusCode, RetryAfter: retryAfter, Body: body}
}
