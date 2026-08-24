package sandboxio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
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

type testMaterializer struct{}

func (testMaterializer) Materialize(_ context.Context, manifest SourceManifest, canonical []byte) (SourceMaterializationReceipt, error) {
	receipt := SourceMaterializationReceipt{SchemaVersion: SourceReceiptVersion, ManifestDigest: sourceManifestDigest(manifest), Sources: make([]SourceMaterialization, len(manifest.Sources))}
	for index, source := range manifest.Sources {
		receipt.Sources[index] = SourceMaterialization{Name: source.Name, URL: source.URL, Destination: source.Destination, Commit: source.Commit,
			Tree: source.Commit, ContentDigest: "sha256:" + strings.Repeat("b", 64), Writable: source.Writable}
	}
	receipt.Digest, _ = sourceReceiptDigest(receipt)
	return receipt, nil
}

type memoryBootstrapState struct{ receipt SourceMaterializationReceipt }

func (s *memoryBootstrapState) Store(_ context.Context, receipt SourceMaterializationReceipt, _ []byte) error {
	s.receipt = receipt
	return nil
}

func (s *memoryBootstrapState) Release(_ context.Context, digest string) (ReleaseReceipt, []byte, error) {
	if s.receipt.Digest != digest {
		return ReleaseReceipt{}, nil, protocolError("source_receipt_mismatch", nil)
	}
	receipt := ReleaseReceipt{SchemaVersion: SourceReceiptVersion, ReceiptDigest: digest, Released: true}
	encoded, _ := json.Marshal(receipt)
	return receipt, encoded, nil
}

func TestBootstrapProtocolIsClosedBoundedAndCanonical(t *testing.T) {
	manifest := testManifest()
	body, err := MarshalSourceManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var request, response bytes.Buffer
	if err := EncodeRequest(&request, OperationBootstrap, body); err != nil {
		t.Fatal(err)
	}
	state := &memoryBootstrapState{}
	if err := ServeBootstrap(context.Background(), &request, &response, testMaterializer{}, state); err != nil {
		t.Fatal(err)
	}
	_, encodedReceipt, err := DecodeResponse(context.Background(), &response, OperationBootstrap, MaxManifestBytes)
	receipt, receiptErr := DecodeSourceMaterializationReceipt(encodedReceipt, &manifest)
	if err != nil || receiptErr != nil || receipt.Digest != state.receipt.Digest {
		t.Fatalf("receipt=%#v decode=%v err=%v", receipt, receiptErr, err)
	}

	invalid := [][]byte{
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[],"injected":true}`),
		[]byte(`{"schemaVersion":"wrong","sources":[]}`),
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[{"name":"source","url":"https://user:pass@example.test/repo","destination":"/workspace/src/repo","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","writable":false}]}`),
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[{"name":"source","url":"https://example.test:8443/repo","destination":"/workspace/src/repo","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","writable":false}]}`),
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[{"name":"source","url":"https://example.test/repo","destination":"/workspace/src/../escape","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","writable":false}]}`),
		[]byte(`{"schemaVersion":"blazn.dev/sandbox-source-manifest/v1","sources":[{"name":"parent","url":"https://example.test/parent","destination":"/workspace/src/repo","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","writable":false},{"name":"child","url":"https://example.test/child","destination":"/workspace/src/repo/vendor","commit":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","writable":false}]}`),
	}
	for _, candidate := range invalid {
		if _, _, err := ValidateSourceManifest(candidate); err == nil {
			t.Fatalf("invalid manifest accepted: %s", candidate)
		}
	}
}

func TestFileBootstrapStatePersistsBeforeIdempotentRelease(t *testing.T) {
	manifest := testManifest()
	canonical, err := MarshalSourceManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := (testMaterializer{}).Materialize(context.Background(), manifest, canonical)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalSourceMaterializationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	state := FileBootstrapState{Directory: directory, ReceiptName: "materialization.json", MarkerName: "validated"}
	if err := state.Store(context.Background(), receipt, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory + "/validated"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("materialization store released the workload early")
	}
	for range 2 {
		release, body, err := state.Release(context.Background(), receipt.Digest)
		if err != nil || !release.Released || release.ReceiptDigest != receipt.Digest || len(body) == 0 {
			t.Fatalf("release=%#v body=%q err=%v", release, body, err)
		}
	}
	if body, err := os.ReadFile(directory + "/validated"); err != nil || string(body) != receipt.Digest+"\n" {
		t.Fatalf("marker=%q err=%v", body, err)
	}
	if _, _, err := state.Release(context.Background(), "sha256:"+strings.Repeat("f", 64)); !IsProtocolError(err, "source_receipt_mismatch") {
		t.Fatalf("mismatched release error=%v", err)
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
	if err := ServeBootstrap(context.Background(), &injected, &response, testMaterializer{}, &memoryBootstrapState{}); err != nil {
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
