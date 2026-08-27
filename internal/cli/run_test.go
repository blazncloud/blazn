package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

const cliRunID = "00000000-0000-4000-8000-000000000003"

type fakeRunCommands struct {
	runID, cursor, requestID, messageID, claimID string
	leaseSeconds                                 int
	request                                      client.SendRunMessageRequest
}

func (f *fakeRunCommands) ListMessages(_ context.Context, runID, cursor string) (client.RunMessageList, error) {
	f.runID, f.cursor = runID, cursor
	return client.RunMessageList{Items: []client.RunMessage{{ID: "00000000-0000-4000-8000-000000000004", RunID: runID, Ordinal: 1, Kind: client.RunMessageKindPrompt, Status: "queued", Content: "Inspect the repository"}}}, nil
}
func (f *fakeRunCommands) SendMessage(_ context.Context, runID, requestID string, request client.SendRunMessageRequest) (client.RunMessageEnvelope, error) {
	f.runID, f.requestID, f.request = runID, requestID, request
	return client.RunMessageEnvelope{Message: client.RunMessage{ID: "00000000-0000-4000-8000-000000000005", RunID: runID, Ordinal: 2, Kind: request.Kind, Status: "queued", Content: request.Content}}, nil
}
func (f *fakeRunCommands) ClaimMessage(_ context.Context, runID, requestID string, leaseSeconds int) (client.RunMessageClaimEnvelope, error) {
	f.runID, f.requestID, f.leaseSeconds = runID, requestID, leaseSeconds
	return client.RunMessageClaimEnvelope{Claim: &client.RunMessageClaim{Message: client.RunMessage{ID: "00000000-0000-4000-8000-000000000005", RunID: runID, Ordinal: 2, Kind: client.RunMessageKindSteer, Status: "claimed", Content: "Only update docs"}, ClaimID: "00000000-0000-4000-8000-000000000006", LeaseExpiresAt: "2026-08-26T15:00:30Z"}}, nil
}
func (f *fakeRunCommands) DeliverMessage(_ context.Context, runID, messageID, claimID, requestID string) (client.RunMessageEnvelope, error) {
	f.runID, f.messageID, f.claimID, f.requestID = runID, messageID, claimID, requestID
	return client.RunMessageEnvelope{Message: client.RunMessage{ID: messageID, RunID: runID, Ordinal: 2, Kind: client.RunMessageKindSteer, Status: "delivered", Content: "Only update docs"}}, nil
}

func runCommandApp(fake *fakeRunCommands) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := New(stdout, stderr, testBuild)
	app.run = func() (runCommands, error) { return fake, nil }
	return app, stdout, stderr
}

func TestRunMessagesAndSendCommands(t *testing.T) {
	fake := &fakeRunCommands{}
	app, stdout, stderr := runCommandApp(fake)
	if code := app.Run([]string{"run", "messages", cliRunID, "--cursor", "1"}); code != ExitSuccess || stderr.Len() != 0 || fake.cursor != "1" || !strings.Contains(stdout.String(), "Inspect the repository") {
		t.Fatalf("code=%d stdout=%q stderr=%q fake=%#v", code, stdout.String(), stderr.String(), fake)
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "send", cliRunID, "--kind", "steer", "--content", "Only update docs", "--parent", "00000000-0000-4000-8000-000000000004", "--request-id", "message-send-1", "--output=json"}); code != ExitSuccess || stderr.Len() != 0 || fake.requestID != "message-send-1" || fake.request.Kind != client.RunMessageKindSteer || fake.request.ParentMessageID == "" || !strings.Contains(stdout.String(), `"kind":"steer"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q fake=%#v", code, stdout.String(), stderr.String(), fake)
	}
}

func TestRunClaimAndDeliverCommands(t *testing.T) {
	fake := &fakeRunCommands{}
	app, stdout, stderr := runCommandApp(fake)
	if code := app.Run([]string{"run", "claim", cliRunID, "--lease", "45", "--request-id", "message-claim-1", "--output=json"}); code != ExitSuccess || stderr.Len() != 0 || fake.leaseSeconds != 45 || fake.requestID != "message-claim-1" || !strings.Contains(stdout.String(), `"claimId"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q fake=%#v", code, stdout.String(), stderr.String(), fake)
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "deliver", cliRunID, "--message", "00000000-0000-4000-8000-000000000005", "--claim", "00000000-0000-4000-8000-000000000006", "--request-id", "message-deliver-1"}); code != ExitSuccess || stderr.Len() != 0 || fake.messageID == "" || fake.claimID == "" || fake.requestID != "message-deliver-1" || !strings.Contains(stdout.String(), "delivered message") {
		t.Fatalf("code=%d stdout=%q stderr=%q fake=%#v", code, stdout.String(), stderr.String(), fake)
	}
}

func TestRunSendRejectsIncompleteOrUnknownMessageKind(t *testing.T) {
	for _, args := range [][]string{{"run", "send", cliRunID, "--kind", "prompt"}, {"run", "send", cliRunID, "--kind", "replace", "--content", "x", "--request-id", "message-send-1"}, {"run", "claim", cliRunID, "--lease", "4", "--request-id", "message-claim-1"}, {"run", "deliver", cliRunID, "--message", "missing"}} {
		fake := &fakeRunCommands{}
		app, _, stderr := runCommandApp(fake)
		if code := app.Run(args); code != ExitUsage || stderr.Len() == 0 || fake.runID != "" {
			t.Fatalf("args=%v code=%d stderr=%q fake=%#v", args, code, stderr.String(), fake)
		}
	}
}
