package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

func TestBrokerFrameRoundTripAndBounds(t *testing.T) {
	var buffer bytes.Buffer
	input := brokerFrame{Type: brokerFrameRequest, StreamID: 7, Payload: []byte(`{"request":true}`)}
	if err := writeBrokerFrame(&buffer, input); err != nil {
		t.Fatal(err)
	}
	output, err := readBrokerFrame(&buffer)
	if err != nil || output.Type != input.Type || output.StreamID != input.StreamID || !bytes.Equal(output.Payload, input.Payload) {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	if err := writeBrokerFrame(io.Discard, brokerFrame{Type: brokerFrameRequest, StreamID: 1, Payload: make([]byte, brokerMaxControlBytes+1)}); err == nil {
		t.Fatal("oversized control frame passed")
	}
}

func TestBrokerArtifactUploadReadyDataEndAndCleanup(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	defer root.Close()
	defer child.Close()
	content := []byte("hello world!")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	size := int64(len(content))
	api := &fakeBrokerAPI{run: client.RunEnvelope{Run: client.Run{ID: brokerTestRunID, WorkspaceID: brokerTestWorkspaceID, ProjectID: brokerTestProjectID, Kind: "content.render", ProofClass: client.ProofClassSynthetic, Status: client.RunStatusRunning, Version: 2, PlanDigest: "sha256:" + strings.Repeat("a", 64), InputArtifactIDs: []string{}, OutputNames: []string{"preview.mp4"}, RequestedBy: "user-1", CreatedAt: "2026-08-23T00:00:00Z"}}, artifact: client.ArtifactEnvelope{Artifact: client.Artifact{ID: brokerTestArtifactID, WorkspaceID: brokerTestWorkspaceID, ProjectID: brokerTestProjectID, SourceRunID: brokerTestRunID, Kind: "content.video", MediaType: client.ArtifactMediaTypeVideo, Name: "preview.mp4", Status: client.ArtifactStatusReady, Version: 1, Digest: digest, SizeBytes: &size, CreatedBy: "user-1", CreatedAt: "2026-08-23T00:00:01Z", UpdatedAt: "2026-08-23T00:00:01Z", DownloadAvailable: true}}}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	handler.uploadTempDir = t.TempDir()
	session := newBrokerSessionWithHandler(context.Background(), root, "content", runtimeContext, handler)
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"dddddddddddddddddddddddddddddddd","method":"artifact.upload.begin","params":{"runId":"` + brokerTestRunID + `","name":"preview.mp4","kind":"content.video","mediaType":"video","sizeBytes":12,"digest":"` + digest + `","idempotencyKey":"upload-file-1"}}`)
	if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 10, Payload: request}); err != nil {
		t.Fatal(err)
	}
	readyFrame, err := readBrokerFrame(child)
	if err != nil {
		t.Fatal(err)
	}
	var ready brokerResponse
	if json.Unmarshal(readyFrame.Payload, &ready) != nil || !ready.OK || ready.ResultSchema != "artifact-upload-ready/v1" {
		t.Fatalf("ready=%s", readyFrame.Payload)
	}
	for _, chunk := range [][]byte{content[:5], content[5:]} {
		if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameData, StreamID: 10, Payload: chunk}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameEnd, StreamID: 10}); err != nil {
		t.Fatal(err)
	}
	terminal, err := readBrokerFrame(child)
	if err != nil {
		t.Fatal(err)
	}
	var response brokerResponse
	if json.Unmarshal(terminal.Payload, &response) != nil || !response.OK || response.ResultSchema != resultArtifactEnvelope || !bytes.Equal(api.uploadContent, content) {
		t.Fatalf("terminal=%s content=%q", terminal.Payload, api.uploadContent)
	}
	_ = child.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(handler.uploadTempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestBrokerArtifactUploadPartialEOFCleansWithoutActivation(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	defer root.Close()
	sum := sha256.Sum256([]byte("xx"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	api := &fakeBrokerAPI{run: client.RunEnvelope{Run: client.Run{ID: brokerTestRunID, WorkspaceID: brokerTestWorkspaceID, ProjectID: brokerTestProjectID, Kind: "content.render", ProofClass: client.ProofClassSynthetic, Status: client.RunStatusRunning, Version: 2, PlanDigest: "sha256:" + strings.Repeat("a", 64), InputArtifactIDs: []string{}, OutputNames: []string{"preview.mp4"}, RequestedBy: "user-1", CreatedAt: "2026-08-23T00:00:00Z"}}}
	handler, runtimeContext := brokerTestHandler(t, api, &fakeBrokerSessions{})
	handler.uploadTempDir = t.TempDir()
	session := newBrokerSessionWithHandler(context.Background(), root, "content", runtimeContext, handler)
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","method":"artifact.upload.begin","params":{"runId":"` + brokerTestRunID + `","name":"preview.mp4","kind":"content.video","mediaType":"video","sizeBytes":2,"digest":"` + digest + `","idempotencyKey":"upload-file-2"}}`)
	_ = writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 11, Payload: request})
	if _, err := readBrokerFrame(child); err != nil {
		t.Fatal(err)
	}
	_ = writeBrokerFrame(child, brokerFrame{Type: brokerFrameData, StreamID: 11, Payload: []byte("x")})
	_ = child.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(api.uploadContent) != 0 {
		t.Fatalf("partial upload activated: %q", api.uploadContent)
	}
	entries, err := os.ReadDir(handler.uploadTempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestBrokerRejectsDataBeforeUploadReadiness(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil { t.Skip(err) }
	defer root.Close();defer child.Close()
	session:=newBrokerSession(root,"content",validRuntimeContext(t));done:=make(chan error,1);go func(){done<-session.serve()}()
	if err:=writeBrokerFrame(child,brokerFrame{Type:brokerFrameData,StreamID:12,Payload:[]byte("x")});err!=nil{t.Fatal(err)}
	if err:=<-done;err==nil||!strings.Contains(err.Error(),"non-upload"){t.Fatalf("err=%v",err)}
}

func TestBrokerFrameRejectsHeaderDrift(t *testing.T) {
	valid := func() []byte {
		var buffer bytes.Buffer
		if err := writeBrokerFrame(&buffer, brokerFrame{Type: brokerFrameRequest, StreamID: 1}); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	for _, mutate := range []func([]byte){func(v []byte) { v[0] = 'X' }, func(v []byte) { v[4] = 2 }, func(v []byte) { v[5] = 99 }, func(v []byte) { v[6] = 1 }, func(v []byte) { binary.BigEndian.PutUint32(v[8:12], 0) }} {
		value := append([]byte(nil), valid()...)
		mutate(value)
		if _, err := readBrokerFrame(bytes.NewReader(value)); err == nil {
			t.Fatal("invalid broker header passed")
		}
	}
}

func TestBrokerSessionDescribesOnlyAvailableTransport(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	defer root.Close()
	defer child.Close()
	session := newBrokerSession(root, "content", validRuntimeContext(t))
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","method":"broker.describe","params":{}}`)
	if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 9, Payload: request}); err != nil {
		t.Fatal(err)
	}
	frame, err := readBrokerFrame(child)
	if err != nil {
		t.Fatal(err)
	}
	var response brokerResponse
	if json.Unmarshal(frame.Payload, &response) != nil || !response.OK || response.ResultSchema != "broker-description/v1" || !strings.Contains(response.Payload, `"availableCapabilities":["broker.describe"]`) {
		t.Fatalf("response=%s", frame.Payload)
	}
	_ = child.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrokerSessionRejectsDuplicateStreamsAndSignalsCancellation(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	session := newBrokerSession(root, "content", validRuntimeContext(t))
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","method":"broker.describe","params":{}}`)
	_ = writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 3, Payload: request})
	if _, err := readBrokerFrame(child); err != nil {
		t.Fatal(err)
	}
	_ = writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: 3, Payload: request})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("err=%v", err)
	}
	_ = child.Close()
	_ = root.Close()
	root2, child2, err := newBrokerSocketPair()
	if err != nil {
		t.Fatal(err)
	}
	session2 := newBrokerSession(root2, "content", validRuntimeContext(t))
	session2.cancel()
	frame, err := readBrokerFrame(child2)
	if err != nil || frame.Type != brokerFrameCancel || frame.StreamID != brokerControlStreamID {
		t.Fatalf("frame=%#v err=%v", frame, err)
	}
	_ = root2.Close()
	_ = child2.Close()
}

func TestBrokerSessionBoundsTotalStreams(t *testing.T) {
	root, child, err := newBrokerSocketPair()
	if err != nil {
		t.Skip(err)
	}
	session := newBrokerSession(root, "content", validRuntimeContext(t))
	for stream := uint32(1); stream <= brokerMaxStreams; stream++ {
		session.seen[stream] = true
	}
	done := make(chan error, 1)
	go func() { done <- session.serve() }()
	request := []byte(`{"schemaVersion":1,"requestId":"cccccccccccccccccccccccccccccccc","method":"broker.describe","params":{}}`)
	if err := writeBrokerFrame(child, brokerFrame{Type: brokerFrameRequest, StreamID: brokerMaxStreams + 1, Payload: request}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "stream limit") {
		t.Fatalf("err=%v", err)
	}
	_ = root.Close()
	_ = child.Close()
}
