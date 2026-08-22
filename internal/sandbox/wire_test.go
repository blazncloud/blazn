package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

type wireExecAPI struct {
	*fakeAPI
	transport *strictTransport
}

func (a *wireExecAPI) ExecuteSandboxGrant(ctx context.Context, id, token string, request client.SandboxExecRequest) (client.SandboxExecResult, error) {
	return a.transport.ExecuteSandboxGrant(ctx, id, token, request)
}

func TestServiceExecStrictWireDecoding(t *testing.T) {
	valid := `{"remoteExitCode":0,"stdoutBase64":"","stderrBase64":"","truncated":false}`
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "zero values present", body: valid},
		{name: "empty object", body: `{}`, wantErr: true},
		{name: "required zero omitted", body: `{"stdoutBase64":"","stderrBase64":"","truncated":false}`, wantErr: true},
		{name: "required zero replaced by null", body: `{"remoteExitCode":null,"stdoutBase64":"","stderrBase64":"","truncated":false}`, wantErr: true},
		{name: "duplicate required property", body: `{"remoteExitCode":0,"remoteExitCode":1,"stdoutBase64":"","stderrBase64":"","truncated":false}`, wantErr: true},
		{name: "unknown property", body: `{"remoteExitCode":0,"stdoutBase64":"","stderrBase64":"","truncated":false,"extra":true}`, wantErr: true},
		{name: "trailing document", body: valid + ` {}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			transport, err := newStrictTransport(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			id := "11111111-1111-4111-8111-111111111111"
			api := &wireExecAPI{fakeAPI: &fakeAPI{grant: grant(client.SandboxGrantExec, id)}, transport: transport}
			_, err = NewService(api, &staticTokens{value: "session-secret"}).Exec(context.Background(), id, []string{"true"})
			if test.wantErr && !IsPartial(err) {
				t.Fatalf("malformed response accepted: %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("valid zero-valued response rejected: %v", err)
			}
		})
	}
}

func TestStrictSSEWireDecoding(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	eventID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	valid := `{"eventId":"` + eventID + `","sandboxId":"` + id + `","operationId":null,"sequence":0,"type":"sandbox.ready","payload":{},"createdAt":"2026-08-22T00:00:00Z"}`
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{name: "zero and null values present", data: valid},
		{name: "empty object", data: `{}`, wantErr: true},
		{name: "required nullable property omitted", data: `{"eventId":"` + eventID + `","sandboxId":"` + id + `","sequence":0,"type":"sandbox.ready","payload":{},"createdAt":"2026-08-22T00:00:00Z"}`, wantErr: true},
		{name: "duplicate required property", data: `{"eventId":"` + eventID + `","sandboxId":"` + id + `","operationId":null,"sequence":0,"sequence":0,"type":"sandbox.ready","payload":{},"createdAt":"2026-08-22T00:00:00Z"}`, wantErr: true},
		{name: "non-nullable payload is null", data: `{"eventId":"` + eventID + `","sandboxId":"` + id + `","operationId":null,"sequence":0,"type":"sandbox.ready","payload":null,"createdAt":"2026-08-22T00:00:00Z"}`, wantErr: true},
		{name: "unknown property", data: `{"eventId":"` + eventID + `","sandboxId":"` + id + `","operationId":null,"sequence":0,"type":"sandbox.ready","payload":{},"createdAt":"2026-08-22T00:00:00Z","extra":true}`, wantErr: true},
		{name: "trailing document", data: valid + ` {}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", eventID, test.data)
			}))
			defer server.Close()
			transport, err := newStrictTransport(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			stream, err := transport.StreamSandboxEvents(context.Background(), "access", id, "")
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			event, err := stream.Next()
			if test.wantErr && err == nil {
				t.Fatalf("malformed event accepted: %+v", event)
			}
			if !test.wantErr && (err != nil || event.Sequence != 0 || event.OperationID != nil) {
				t.Fatalf("valid zero/null event rejected: event=%+v err=%v", event, err)
			}
		})
	}
}
