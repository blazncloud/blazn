package sandboxio

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

const MaxAccessFileBytes = 8 << 20

func validAccessPath(absolute string) bool {
	return validWorkspacePath(absolute, "/workspace/src/") || validWorkspacePath(absolute, "/workspace/artifacts/")
}

func ReadWorkspaceFile(root *RootFileSystem, absolute string) (Artifact, error) {
	if root == nil || !validAccessPath(absolute) {
		return Artifact{}, protocolError("access_path_invalid", nil)
	}
	relative := strings.TrimPrefix(absolute, "/workspace/")
	return readRootFile(root, relative, MaxAccessFileBytes)
}

func WriteWorkspaceFile(root *RootFileSystem, absolute, digest string, size int64, input io.Reader) error {
	if root == nil || input == nil || !validAccessPath(absolute) || size < 0 || size > MaxAccessFileBytes ||
		len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		return protocolError("access_upload_invalid", nil)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
		return protocolError("access_upload_invalid", err)
	}
	relative := strings.TrimPrefix(absolute, "/workspace/")
	parent := path.Dir(relative)
	if err := inspectRootParents(root, parent); err != nil {
		return err
	}
	if current, err := root.root.Lstat(relative); err == nil {
		if !current.Mode().IsRegular() || current.Mode()&fs.ModeSymlink != 0 || linkCount(current) != 1 {
			return protocolError("access_path_unsafe", nil)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return protocolError("access_path_unsafe", err)
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return protocolError("access_upload_failed", err)
	}
	temporary := path.Join(parent, ".blazn-upload-"+hex.EncodeToString(nonce))
	file, err := root.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return protocolError("access_upload_failed", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = root.root.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(input, MaxAccessFileBytes+1))
	if copyErr != nil || written != size || written > MaxAccessFileBytes || "sha256:"+hex.EncodeToString(hasher.Sum(nil)) != digest {
		return protocolError("access_upload_mismatch", copyErr)
	}
	if err := file.Sync(); err != nil {
		return protocolError("access_upload_failed", err)
	}
	if err := file.Close(); err != nil {
		return protocolError("access_upload_failed", err)
	}
	if err := inspectRootParents(root, parent); err != nil {
		return err
	}
	if err := root.root.Rename(temporary, relative); err != nil {
		return protocolError("access_upload_failed", err)
	}
	remove = false
	return nil
}

func readRootFile(root *RootFileSystem, relative string, maxBytes int64) (Artifact, error) {
	if err := inspectRootParents(root, path.Dir(relative)); err != nil {
		return Artifact{}, err
	}
	before, err := root.root.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return Artifact{}, protocolError("access_file_not_found", err)
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxBytes || linkCount(before) != 1 {
		return Artifact{}, protocolError("access_file_unsafe", err)
	}
	file, err := root.root.Open(relative)
	if err != nil {
		return Artifact{}, protocolError("access_file_unsafe", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != before.Size() || linkCount(opened) != 1 || !sameFile(before, opened) {
		return Artifact{}, protocolError("access_file_changed", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(body)) != opened.Size() || int64(len(body)) > maxBytes {
		return Artifact{}, protocolError("access_file_changed", err)
	}
	after, err := file.Stat()
	if err != nil || !sameSnapshot(opened, after) || linkCount(after) != 1 {
		return Artifact{}, protocolError("access_file_changed", err)
	}
	hash := sha256.Sum256(body)
	return Artifact{Body: body, Size: int64(len(body)), SHA256: "sha256:" + hex.EncodeToString(hash[:])}, nil
}

func inspectRootParents(root *RootFileSystem, parent string) error {
	if parent == "." || parent == "" {
		return nil
	}
	parts := strings.Split(parent, "/")
	for index := range parts {
		info, err := root.root.Lstat(path.Join(parts[:index+1]...))
		if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return protocolError("access_path_unsafe", err)
		}
	}
	return nil
}
