package sandboxio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func testManifest() SourceManifest {
	return SourceManifest{SchemaVersion: SourceManifestVersion, Sources: []Source{{
		Name: "source", URL: "https://github.com/blazncloud/blazn.git", Destination: "/workspace/src/blazn",
		Commit: strings.Repeat("a", 40), Writable: false,
	}}}
}

func TestBootstrapProtocolIsClosedBoundedAndCanonical(t *testing.T) {
	body, err := MarshalSourceManifest(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	var request, response bytes.Buffer
	if err := EncodeRequest(&request, OperationBootstrap, body); err != nil {
		t.Fatal(err)
	}
	if err := ServeBootstrap(context.Background(), &request, &response, nil); err != nil {
		t.Fatal(err)
	}
	_, echoed, err := DecodeResponse(context.Background(), &response, OperationBootstrap, MaxManifestBytes)
	if err != nil || !bytes.Equal(echoed, body) {
		t.Fatalf("echo=%q err=%v", echoed, err)
	}

	invalid := [][]byte{
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[],"injected":true}`),
		[]byte(`{"schemaVersion":"wrong","sources":[]}`),
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[{"name":"source","url":"https://user:pass@example.test/repo","destination":"/workspace/src/repo","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","writable":false}]}`),
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[{"name":"source","url":"https://example.test/repo","destination":"/workspace/src/../escape","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","writable":false}]}`),
	}
	for _, candidate := range invalid {
		if _, _, err := ValidateSourceManifest(candidate); err == nil {
			t.Fatalf("invalid manifest accepted: %s", candidate)
		}
	}
}

func TestProtocolRejectsMalformedOversizedAndInjectedFrames(t *testing.T) {
	var oversized bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], MaxHeaderBytes+1)
	overlies := append(prefix[:], bytes.Repeat([]byte{'x'}, MaxHeaderBytes+1)...)
	overlies = append(overlies, 0)
	oversized.Write(overlies)
	if _, _, err := DecodeRequest(context.Background(), &oversized, OperationBootstrap); !IsProtocolError(err, "frame_header_too_large") {
		t.Fatalf("oversized header error=%v", err)
	}

	header, _ := json.Marshal(RequestHeader{Version: ProtocolVersion, Operation: OperationBootstrap, BodyBytes: MaxManifestBytes + 1})
	var oversizedBody bytes.Buffer
	binary.BigEndian.PutUint32(prefix[:], uint32(len(header)))
	oversizedBody.Write(prefix[:])
	oversizedBody.Write(header)
	if _, _, err := DecodeRequest(context.Background(), &oversizedBody, OperationBootstrap); !IsProtocolError(err, "request_body_too_large") {
		t.Fatalf("oversized body error=%v", err)
	}

	body, _ := MarshalSourceManifest(testManifest())
	var injected, response bytes.Buffer
	if err := EncodeRequest(&injected, OperationBootstrap, body); err != nil {
		t.Fatal(err)
	}
	injected.WriteString("injected")
	if err := ServeBootstrap(context.Background(), &injected, &response, nil); err != nil {
		t.Fatalf("framed injection error was reported as a process failure: %v", err)
	}
	if _, _, err := DecodeResponse(context.Background(), &response, OperationBootstrap, MaxManifestBytes); !IsProtocolError(err, "protocol_injection") {
		t.Fatalf("error response=%v", err)
	}

	malformed := bytes.NewBuffer([]byte{0, 0, 0, 2, '{', '}'})
	if _, _, err := DecodeRequest(context.Background(), malformed, OperationBootstrap); err == nil {
		t.Fatal("malformed header accepted")
	}
}

func TestProtocolReadHonorsDeadlineOnStall(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := DecodeRequest(ctx, reader, OperationBootstrap)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("stall err=%v duration=%s", err, time.Since(started))
	}
}

func TestArtifactRequestIsClosedAndConfined(t *testing.T) {
	for _, value := range []string{"../escape", "/workspace/artifacts/../escape", "/workspace/artifacts/link\\x", "/workspace/artifacts/"} {
		body, _ := json.Marshal(map[string]any{"path": value})
		if _, err := DecodeArtifactRequest(body); err == nil {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	}
	if _, err := DecodeArtifactRequest([]byte(`{"path":"/workspace/artifacts/result","extra":true}`)); err == nil {
		t.Fatal("unknown artifact request field accepted")
	}
}
