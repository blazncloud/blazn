package sandboxio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

type BootstrapState interface {
	Store(context.Context, SourceMaterializationReceipt, []byte) error
	Release(context.Context, string) (ReleaseReceipt, []byte, error)
}

func ServeBootstrap(ctx context.Context, input io.Reader, output io.Writer, materializer SourceMaterializer, state BootstrapState) error {
	_, body, err := DecodeRequest(ctx, input, OperationBootstrap)
	if err != nil {
		return respondError(output, OperationBootstrap, err)
	}
	if err := requireEOF(ctx, input); err != nil {
		return respondError(output, OperationBootstrap, protocolError("protocol_injection", err))
	}
	manifest, canonical, err := ValidateSourceManifest(body)
	if err != nil {
		return respondError(output, OperationBootstrap, err)
	}
	if materializer == nil || state == nil {
		return respondError(output, OperationBootstrap, protocolError("source_materializer_unavailable", nil))
	}
	receipt, err := materializer.Materialize(ctx, manifest, canonical)
	if err != nil {
		return respondError(output, OperationBootstrap, err)
	}
	encoded, err := MarshalSourceMaterializationReceipt(receipt)
	if err != nil {
		return respondError(output, OperationBootstrap, err)
	}
	if err := state.Store(ctx, receipt, encoded); err != nil {
		return respondError(output, OperationBootstrap, protocolError("source_receipt_store_failed", err))
	}
	return EncodeResponse(output, SuccessHeader(OperationBootstrap, encoded), encoded)
}

func ServeRelease(ctx context.Context, input io.Reader, output io.Writer, state BootstrapState) error {
	_, body, err := DecodeRequest(ctx, input, OperationRelease)
	if err != nil {
		return respondError(output, OperationRelease, err)
	}
	if err := requireEOF(ctx, input); err != nil {
		return respondError(output, OperationRelease, protocolError("protocol_injection", err))
	}
	request, err := DecodeReleaseRequest(body)
	if err != nil || state == nil {
		return respondError(output, OperationRelease, protocolError("release_request_invalid", err))
	}
	_, encoded, err := state.Release(ctx, request.ReceiptDigest)
	if err != nil {
		return respondError(output, OperationRelease, err)
	}
	return EncodeResponse(output, SuccessHeader(OperationRelease, encoded), encoded)
}

func ServeArtifact(ctx context.Context, input io.Reader, output io.Writer, fileSystem ArtifactFileSystem) error {
	_, body, err := DecodeRequest(ctx, input, OperationArtifact)
	if err != nil {
		return respondError(output, OperationArtifact, err)
	}
	if err := requireEOF(ctx, input); err != nil {
		return respondError(output, OperationArtifact, protocolError("protocol_injection", err))
	}
	request, err := DecodeArtifactRequest(body)
	if err != nil {
		return respondError(output, OperationArtifact, err)
	}
	artifact, err := ReadArtifact(fileSystem, request.Path, MaxArtifactBytes)
	if err != nil {
		return respondError(output, OperationArtifact, err)
	}
	header := SuccessHeader(OperationArtifact, artifact.Body)
	if header.SHA256 != artifact.SHA256 || header.BodyBytes != artifact.Size {
		return respondError(output, OperationArtifact, protocolError("artifact_file_changed", nil))
	}
	return EncodeResponse(output, header, artifact.Body)
}

type FileBootstrapState struct {
	Directory   string
	ReceiptName string
	MarkerName  string
}

func (f FileBootstrapState) Store(ctx context.Context, receipt SourceMaterializationReceipt, canonical []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.validate(); err != nil {
		return err
	}
	root, err := os.OpenRoot(f.Directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if decoded, err := DecodeSourceMaterializationReceipt(canonical, nil); err != nil || decoded.Digest != receipt.Digest {
		return errors.New("bootstrap receipt is invalid")
	}
	file, err := root.OpenFile(f.ReceiptName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := root.Open(f.ReceiptName)
		if readErr != nil {
			return readErr
		}
		defer existing.Close()
		body, readErr := io.ReadAll(io.LimitReader(existing, MaxManifestBytes+1))
		if readErr != nil || string(body) != string(canonical) {
			return errors.New("bootstrap receipt differs from materialized sources")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(canonical); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (f FileBootstrapState) Release(ctx context.Context, receiptDigest string) (ReleaseReceipt, []byte, error) {
	if err := ctx.Err(); err != nil {
		return ReleaseReceipt{}, nil, err
	}
	if err := f.validate(); err != nil || !digestPattern.MatchString(receiptDigest) {
		return ReleaseReceipt{}, nil, protocolError("release_request_invalid", err)
	}
	root, err := os.OpenRoot(f.Directory)
	if err != nil {
		return ReleaseReceipt{}, nil, err
	}
	defer root.Close()
	receiptFile, err := root.Open(f.ReceiptName)
	if err != nil {
		return ReleaseReceipt{}, nil, protocolError("source_receipt_unavailable", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(receiptFile, MaxManifestBytes+1))
	closeErr := receiptFile.Close()
	if readErr != nil || closeErr != nil || len(body) > MaxManifestBytes {
		return ReleaseReceipt{}, nil, protocolError("source_receipt_invalid", errors.Join(readErr, closeErr))
	}
	receipt, err := DecodeSourceMaterializationReceipt(body, nil)
	if err != nil || receipt.Digest != receiptDigest {
		return ReleaseReceipt{}, nil, protocolError("source_receipt_mismatch", err)
	}
	markerContent := []byte(receiptDigest + "\n")
	marker, err := root.OpenFile(f.MarkerName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if errors.Is(err, os.ErrExist) {
		existing, openErr := root.Open(f.MarkerName)
		if openErr != nil {
			return ReleaseReceipt{}, nil, openErr
		}
		existingBody, readErr := io.ReadAll(io.LimitReader(existing, int64(len(markerContent)+1)))
		closeErr := existing.Close()
		if readErr != nil || closeErr != nil || string(existingBody) != string(markerContent) {
			return ReleaseReceipt{}, nil, protocolError("bootstrap_release_changed", errors.Join(readErr, closeErr))
		}
	} else if err != nil {
		return ReleaseReceipt{}, nil, err
	} else {
		if _, err := marker.Write(markerContent); err != nil {
			_ = marker.Close()
			return ReleaseReceipt{}, nil, err
		}
		if err := marker.Sync(); err != nil {
			_ = marker.Close()
			return ReleaseReceipt{}, nil, err
		}
		if err := marker.Close(); err != nil {
			return ReleaseReceipt{}, nil, err
		}
	}
	release := ReleaseReceipt{SchemaVersion: SourceReceiptVersion, ReceiptDigest: receiptDigest, Released: true}
	encoded, err := json.Marshal(release)
	if err != nil {
		return ReleaseReceipt{}, nil, err
	}
	return release, encoded, nil
}

func (f FileBootstrapState) validate() error {
	for _, name := range []string{f.ReceiptName, f.MarkerName} {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return errors.New("bootstrap state configuration is invalid")
		}
	}
	if f.Directory == "" || f.ReceiptName == f.MarkerName {
		return errors.New("bootstrap state configuration is invalid")
	}
	return nil
}

func WaitForBootstrap(ctx context.Context, directory, name string, interval time.Duration) error {
	if interval <= 0 || interval > time.Second || directory == "" || name == "" || name == "." || name == ".." {
		return errors.New("bootstrap wait configuration is invalid")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		info, statErr := root.Lstat(name)
		if statErr == nil {
			if !info.Mode().IsRegular() || info.Size() != 72 || linkCount(info) != 1 {
				return errors.New("bootstrap marker is unsafe")
			}
			return nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func WaitForSignal(ctx context.Context) error {
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

func respondError(output io.Writer, operation string, err error) error {
	code := "request_failed"
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) && protocolErr.Code != "" {
		code = protocolErr.Code
	}
	if responseErr := EncodeResponse(output, ErrorHeader(operation, code), nil); responseErr != nil {
		return responseErr
	}
	// The logical failure is now represented by the authenticated, bounded
	// response frame. Returning nil lets an exec transport preserve that frame;
	// non-zero process status is reserved for failures to emit a response.
	return nil
}

func requireEOF(ctx context.Context, reader io.Reader) error {
	trailing, err := readExactAtMostOne(ctx, reader)
	if err != nil {
		return err
	}
	if trailing {
		return errors.New("trailing protocol bytes")
	}
	return nil
}

func readExactAtMostOne(ctx context.Context, reader io.Reader) (bool, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		var one [1]byte
		n, err := reader.Read(one[:])
		done <- result{n: n, err: err}
	}()
	select {
	case outcome := <-done:
		if outcome.n > 0 {
			return true, nil
		}
		if outcome.err == nil {
			return false, io.ErrNoProgress
		}
		if errors.Is(outcome.err, io.EOF) {
			return false, nil
		}
		return false, outcome.err
	case <-ctx.Done():
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return false, ctx.Err()
	}
}
