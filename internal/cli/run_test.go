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
	runID, cursor, requestID, messageID, claimID, status string
	leaseSeconds, expectedVersion                        int
	request                                              client.SendRunMessageRequest
	createRequest                                        client.CreateRunRequest
	getStatus                                            client.RunStatus
	receipt                                              *client.RunReceipt
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

func (f *fakeRunCommands) Create(_ context.Context, requestID string, request client.CreateRunRequest) (client.RunEnvelope, error) {
	f.requestID, f.createRequest = requestID, request
	return client.RunEnvelope{Run: client.Run{ID: cliRunID, Kind: request.Kind, ProofClass: request.ProofClass, Status: client.RunStatusQueued, Version: 1, PlanDigest: request.PlanDigest, InputArtifactIDs: request.InputArtifactIDs, OutputNames: request.OutputNames}}, nil
}
func (f *fakeRunCommands) List(_ context.Context, status, cursor string) (client.RunList, error) {
	f.status, f.cursor = status, cursor
	return client.RunList{Items: []client.Run{{ID: cliRunID, Kind: "coding.deterministic", ProofClass: client.ProofClassSynthetic, Status: client.RunStatusRunning}}}, nil
}
func (f *fakeRunCommands) Get(_ context.Context, runID string) (client.RunEnvelope, error) {
	f.runID = runID
	run := client.Run{ID: runID, Kind: "coding.deterministic", ProofClass: client.ProofClassSynthetic, Status: f.getStatus, Version: 2}
	if run.Status == "" {
		run.Status = client.RunStatusSucceeded
	}
	if f.receipt != nil {
		run.Receipt = f.receipt
	}
	return client.RunEnvelope{Run: run}, nil
}
func (f *fakeRunCommands) Cancel(_ context.Context, runID, requestID string, expectedVersion int) (client.RunEnvelope, error) {
	f.runID, f.requestID, f.expectedVersion = runID, requestID, expectedVersion
	return client.RunEnvelope{Run: client.Run{ID: runID, Status: client.RunStatusCancelled, Version: expectedVersion + 1}}, nil
}
func (f *fakeRunCommands) Events(_ context.Context, runID, cursor string) (client.RunEventList, error) {
	f.runID = runID
	page := "0"
	switch cursor {
	case "":
		return client.RunEventList{Items: []client.RunEvent{{Sequence: 0, Type: "run.queued", Payload: map[string]any{}, CreatedAt: "2026-08-29T00:00:00Z"}}, NextCursor: &page}, nil
	case "0":
		return client.RunEventList{Items: []client.RunEvent{{Sequence: 1, Type: "run.succeeded", Payload: map[string]any{}, CreatedAt: "2026-08-29T00:00:01Z"}}}, nil
	default:
		return client.RunEventList{Items: []client.RunEvent{}}, nil
	}
}
func (f *fakeRunCommands) Progress(_ context.Context, runID string) (client.RunProgressList, error) {
	f.runID = runID
	return client.RunProgressList{Items: []client.RunProgressEntry{{Sequence: 0, Phase: "render.plan", Percent: 25, CreatedAt: "2026-08-29T00:00:00Z"}}}, nil
}
func (f *fakeRunCommands) Artifacts(_ context.Context, runID, cursor string) (client.ArtifactList, error) {
	f.runID, f.cursor = runID, cursor
	return client.ArtifactList{Items: []client.Artifact{{ID: "00000000-0000-4000-8000-000000000007", Name: "change.patch", Status: client.ArtifactStatusReady, Digest: "sha256:" + strings.Repeat("a", 64)}}}, nil
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

func TestRunLifecycleCommands(t *testing.T) {
	fake := &fakeRunCommands{}
	app, stdout, stderr := runCommandApp(fake)
	digest := "sha256:" + strings.Repeat("b", 64)
	if code := app.Run([]string{"run", "create", "--kind", "coding.deterministic", "--proof-class", "synthetic", "--plan-digest", digest, "--outputs", "change.patch,summary.md", "--request-id", "run-create-1"}); code != ExitSuccess || stderr.Len() != 0 || fake.createRequest.Kind != "coding.deterministic" || len(fake.createRequest.OutputNames) != 2 || !strings.Contains(stdout.String(), "queued") {
		t.Fatalf("code=%d stdout=%q stderr=%q fake=%#v", code, stdout.String(), stderr.String(), fake)
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "list", "--status", "running"}); code != ExitSuccess || stderr.Len() != 0 || fake.status != "running" || !strings.Contains(stdout.String(), cliRunID) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "get", cliRunID}); code != ExitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "succeeded") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "cancel", cliRunID, "--expected-version", "2", "--request-id", "run-cancel-1"}); code != ExitSuccess || stderr.Len() != 0 || fake.expectedVersion != 2 || !strings.Contains(stdout.String(), "cancelled") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "logs", cliRunID}); code != ExitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "render.plan") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "artifacts", cliRunID}); code != ExitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "change.patch") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	fake.receipt = &client.RunReceipt{SchemaVersion: "blazn.run/receipt/v1alpha1", ProofClass: client.ProofClassSynthetic, Outcome: client.RunReceiptOutcomeSucceeded, PlanDigest: digest, ArtifactIDs: []string{"00000000-0000-4000-8000-000000000007"}, Summary: client.RunReceiptSummary{Steps: 1, Warnings: []string{}}}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "result", cliRunID}); code != ExitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "outcome succeeded") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	fake.receipt = nil
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "result", cliRunID, "--output=json"}); code == ExitSuccess || !strings.Contains(stdout.String(), "run_not_terminal") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	app, stdout, stderr = runCommandApp(fake)
	if code := app.Run([]string{"run", "watch", cliRunID, "--interval-seconds", "1"}); code != ExitSuccess || stderr.Len() != 0 || !strings.Contains(stdout.String(), "run.queued") || !strings.Contains(stdout.String(), "run.succeeded") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
