package sandboxio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ProtocolVersion       = "blazn.dev/sandbox-io/v1"
	SourceManifestVersion = "blazn.dev/sandbox-source-manifest/v1"
	OperationBootstrap    = "bootstrap.materialize"
	OperationRelease      = "bootstrap.release"
	OperationArtifact     = "artifact.read"
	StatusOK              = "ok"
	StatusError           = "error"
	MaxHeaderBytes        = 16 << 10
	MaxManifestBytes      = 64 << 10
	MaxArtifactBytes      = 8 << 20
	MaxSources            = 32
	DefaultTimeout        = 15 * time.Second
	SourceTimeout         = 2 * time.Minute
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type RequestHeader struct {
	Version   string `json:"version"`
	Operation string `json:"operation"`
	BodyBytes int64  `json:"bodyBytes"`
}

type ResponseHeader struct {
	Version   string `json:"version"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	BodyBytes int64  `json:"bodyBytes"`
	SHA256    string `json:"sha256,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
}

type Source struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
	Commit      string `json:"commit"`
	Writable    bool   `json:"writable"`
}

type SourceManifest struct {
	SchemaVersion string   `json:"schemaVersion"`
	Sources       []Source `json:"sources"`
}

type ArtifactRequest struct {
	Path string `json:"path"`
}

type ReleaseRequest struct {
	ReceiptDigest string `json:"receiptDigest"`
}

type ReleaseReceipt struct {
	SchemaVersion string `json:"schemaVersion"`
	ReceiptDigest string `json:"receiptDigest"`
	Released      bool   `json:"released"`
}

type ProtocolError struct {
	Code string
	Err  error
}

func (e *ProtocolError) Error() string { return e.Code }
func (e *ProtocolError) Unwrap() error { return e.Err }

func EncodeRequest(writer io.Writer, operation string, body []byte) error {
	limit, err := requestBodyLimit(operation)
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return protocolError("request_body_too_large", nil)
	}
	header := RequestHeader{Version: ProtocolVersion, Operation: operation, BodyBytes: int64(len(body))}
	if err := writeJSONFrame(writer, header); err != nil {
		return err
	}
	return writeAll(writer, body)
}

func DecodeRequest(ctx context.Context, reader io.Reader, expectedOperation string) (RequestHeader, []byte, error) {
	var header RequestHeader
	if err := readJSONFrame(ctx, reader, &header); err != nil {
		return RequestHeader{}, nil, err
	}
	if header.Version != ProtocolVersion || header.Operation != expectedOperation {
		return RequestHeader{}, nil, protocolError("request_header_invalid", nil)
	}
	limit, err := requestBodyLimit(header.Operation)
	if err != nil || header.BodyBytes < 0 || header.BodyBytes > limit {
		return RequestHeader{}, nil, protocolError("request_body_too_large", err)
	}
	body, err := readExact(ctx, reader, header.BodyBytes)
	if err != nil {
		return RequestHeader{}, nil, protocolError("request_body_invalid", err)
	}
	return header, body, nil
}

func EncodeResponse(writer io.Writer, header ResponseHeader, body []byte) error {
	if header.Version != ProtocolVersion || header.Operation != OperationBootstrap && header.Operation != OperationRelease && header.Operation != OperationArtifact ||
		header.Status != StatusOK && header.Status != StatusError || header.BodyBytes != int64(len(body)) || header.BodyBytes < 0 || header.BodyBytes > MaxArtifactBytes {
		return protocolError("response_header_invalid", nil)
	}
	if header.Status == StatusOK {
		if header.ErrorCode != "" || !digestPattern.MatchString(header.SHA256) {
			return protocolError("response_header_invalid", nil)
		}
	} else if header.ErrorCode == "" || header.SHA256 != "" || len(body) != 0 {
		return protocolError("response_header_invalid", nil)
	}
	if err := writeJSONFrame(writer, header); err != nil {
		return err
	}
	return writeAll(writer, body)
}

func DecodeResponse(ctx context.Context, reader io.Reader, expectedOperation string, maxBody int64) (ResponseHeader, []byte, error) {
	if maxBody < 0 || maxBody > MaxArtifactBytes {
		return ResponseHeader{}, nil, protocolError("response_limit_invalid", nil)
	}
	var header ResponseHeader
	if err := readJSONFrame(ctx, reader, &header); err != nil {
		return ResponseHeader{}, nil, err
	}
	if header.Version != ProtocolVersion || header.Operation != expectedOperation || header.BodyBytes < 0 || header.BodyBytes > maxBody {
		return ResponseHeader{}, nil, protocolError("response_header_invalid", nil)
	}
	if header.Status == StatusError {
		if header.ErrorCode == "" || header.BodyBytes != 0 || header.SHA256 != "" {
			return ResponseHeader{}, nil, protocolError("response_header_invalid", nil)
		}
		return header, nil, protocolError(header.ErrorCode, nil)
	}
	if header.Status != StatusOK || header.ErrorCode != "" || !digestPattern.MatchString(header.SHA256) {
		return ResponseHeader{}, nil, protocolError("response_header_invalid", nil)
	}
	body, err := readExact(ctx, reader, header.BodyBytes)
	if err != nil {
		return ResponseHeader{}, nil, protocolError("response_body_invalid", err)
	}
	digest := sha256.Sum256(body)
	if header.SHA256 != "sha256:"+hex.EncodeToString(digest[:]) {
		return ResponseHeader{}, nil, protocolError("response_digest_mismatch", nil)
	}
	return header, body, nil
}

func ValidateSourceManifest(body []byte) (SourceManifest, []byte, error) {
	if len(body) == 0 || len(body) > MaxManifestBytes {
		return SourceManifest{}, nil, protocolError("source_manifest_invalid", nil)
	}
	var manifest SourceManifest
	if err := decodeClosed(body, &manifest); err != nil || manifest.SchemaVersion != SourceManifestVersion || manifest.Sources == nil || len(manifest.Sources) > MaxSources {
		return SourceManifest{}, nil, protocolError("source_manifest_invalid", err)
	}
	seenNames, seenDestinations := map[string]bool{}, map[string]bool{}
	for _, source := range manifest.Sources {
		parsed, err := url.Parse(source.URL)
		if err != nil || !namePattern.MatchString(source.Name) || !commitPattern.MatchString(source.Commit) ||
			parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
			!validWorkspacePath(source.Destination, "/workspace/src/") || seenNames[source.Name] || seenDestinations[source.Destination] {
			return SourceManifest{}, nil, protocolError("source_manifest_invalid", err)
		}
		seenNames[source.Name], seenDestinations[source.Destination] = true, true
	}
	sort.Slice(manifest.Sources, func(i, j int) bool { return manifest.Sources[i].Name < manifest.Sources[j].Name })
	canonical, err := json.Marshal(manifest)
	if err != nil || len(canonical) > MaxManifestBytes {
		return SourceManifest{}, nil, protocolError("source_manifest_invalid", err)
	}
	return manifest, canonical, nil
}

func DecodeArtifactRequest(body []byte) (ArtifactRequest, error) {
	if len(body) == 0 || len(body) > 1024 {
		return ArtifactRequest{}, protocolError("artifact_request_invalid", nil)
	}
	var request ArtifactRequest
	if err := decodeClosed(body, &request); err != nil || !validWorkspacePath(request.Path, "/workspace/artifacts/") {
		return ArtifactRequest{}, protocolError("artifact_request_invalid", err)
	}
	return request, nil
}

func MarshalSourceManifest(manifest SourceManifest) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	_, canonical, err := ValidateSourceManifest(encoded)
	return canonical, err
}

func MarshalArtifactRequest(request ArtifactRequest) ([]byte, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeArtifactRequest(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func MarshalReleaseRequest(request ReleaseRequest) ([]byte, error) {
	encoded, err := json.Marshal(request)
	if err != nil || !digestPattern.MatchString(request.ReceiptDigest) {
		return nil, protocolError("release_request_invalid", err)
	}
	return encoded, nil
}

func DecodeReleaseRequest(body []byte) (ReleaseRequest, error) {
	var request ReleaseRequest
	if len(body) == 0 || len(body) > 1024 || decodeClosed(body, &request) != nil || !digestPattern.MatchString(request.ReceiptDigest) {
		return ReleaseRequest{}, protocolError("release_request_invalid", nil)
	}
	return request, nil
}

func SuccessHeader(operation string, body []byte) ResponseHeader {
	digest := sha256.Sum256(body)
	return ResponseHeader{Version: ProtocolVersion, Operation: operation, Status: StatusOK, BodyBytes: int64(len(body)), SHA256: "sha256:" + hex.EncodeToString(digest[:])}
}

func ErrorHeader(operation, code string) ResponseHeader {
	return ResponseHeader{Version: ProtocolVersion, Operation: operation, Status: StatusError, BodyBytes: 0, ErrorCode: code}
}

func validWorkspacePath(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 512 || strings.Contains(value, `\`) || strings.Contains(value, "//") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, prefix), "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			return false
		}
	}
	return true
}

func requestBodyLimit(operation string) (int64, error) {
	switch operation {
	case OperationBootstrap:
		return MaxManifestBytes, nil
	case OperationRelease:
		return 1024, nil
	case OperationArtifact:
		return 1024, nil
	default:
		return 0, protocolError("operation_unsupported", nil)
	}
}

func writeJSONFrame(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > MaxHeaderBytes {
		return protocolError("header_invalid", err)
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(encoded)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return protocolError("frame_write_failed", err)
	}
	if err := writeAll(writer, encoded); err != nil {
		return protocolError("frame_write_failed", err)
	}
	return nil
}

func readJSONFrame(ctx context.Context, reader io.Reader, output any) error {
	prefix, err := readExact(ctx, reader, 4)
	if err != nil {
		return protocolError("frame_header_invalid", err)
	}
	length := int64(binary.BigEndian.Uint32(prefix))
	if length <= 0 || length > MaxHeaderBytes {
		return protocolError("frame_header_too_large", nil)
	}
	body, err := readExact(ctx, reader, length)
	if err != nil {
		return protocolError("frame_header_invalid", err)
	}
	if err := decodeClosed(body, output); err != nil {
		return protocolError("frame_header_invalid", err)
	}
	return nil
}

func readExact(ctx context.Context, reader io.Reader, size int64) ([]byte, error) {
	if size < 0 || size > MaxArtifactBytes {
		return nil, errors.New("read size is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	buffer := make([]byte, int(size))
	if size == 0 {
		return buffer, nil
	}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := io.ReadFull(reader, buffer)
		done <- result{err: err}
	}()
	select {
	case outcome := <-done:
		return buffer, outcome.err
	case <-ctx.Done():
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, ctx.Err()
	}
}

func writeAll(writer io.Writer, body []byte) error {
	for len(body) > 0 {
		written, err := writer.Write(body)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(body) {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}

func decodeClosed(body []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func protocolError(code string, err error) error { return &ProtocolError{Code: code, Err: err} }
