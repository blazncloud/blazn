package sandboxio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"
)

type BootstrapCompleter interface {
	Complete(context.Context, SourceManifest, []byte) error
}

func ServeBootstrap(ctx context.Context, input io.Reader, output io.Writer, completer BootstrapCompleter) error {
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
	if completer != nil {
		if err := completer.Complete(ctx, manifest, canonical); err != nil {
			return respondError(output, OperationBootstrap, protocolError("bootstrap_completion_failed", err))
		}
	}
	return EncodeResponse(output, SuccessHeader(OperationBootstrap, canonical), canonical)
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

type FileCompleter struct {
	Directory string
	Name      string
}

func (f FileCompleter) Complete(ctx context.Context, _ SourceManifest, canonical []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.Directory == "" || f.Name == "" || f.Name == "." || f.Name == ".." {
		return errors.New("bootstrap marker configuration is invalid")
	}
	root, err := os.OpenRoot(f.Directory)
	if err != nil {
		return err
	}
	defer root.Close()
	digest := sha256.Sum256(canonical)
	content := []byte("sha256:" + hex.EncodeToString(digest[:]) + "\n")
	file, err := root.OpenFile(f.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := root.Open(f.Name)
		if readErr != nil {
			return readErr
		}
		defer existing.Close()
		body, readErr := io.ReadAll(io.LimitReader(existing, int64(len(content)+1)))
		if readErr != nil || string(body) != string(content) {
			return errors.New("bootstrap marker differs from validated manifest")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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
