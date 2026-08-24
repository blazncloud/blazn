package sandboxio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"strings"
)

type ArtifactFile interface {
	io.Reader
	io.Closer
	Stat() (fs.FileInfo, error)
}

type ArtifactFileSystem interface {
	Lstat(string) (fs.FileInfo, error)
	Open(string) (ArtifactFile, error)
}

type RootFileSystem struct{ root *os.Root }

func OpenRootFileSystem(directory string) (*RootFileSystem, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &RootFileSystem{root: root}, nil
}

func (f *RootFileSystem) Close() error { return f.root.Close() }
func (f *RootFileSystem) Lstat(name string) (fs.FileInfo, error) {
	return f.root.Lstat(name)
}
func (f *RootFileSystem) Open(name string) (ArtifactFile, error) { return f.root.Open(name) }

type Artifact struct {
	Body   []byte
	SHA256 string
	Size   int64
}

func ReadArtifact(fileSystem ArtifactFileSystem, absolutePath string, maxBytes int64) (Artifact, error) {
	if fileSystem == nil || maxBytes <= 0 || maxBytes > MaxArtifactBytes || !validWorkspacePath(absolutePath, "/workspace/artifacts/") {
		return Artifact{}, protocolError("artifact_request_invalid", nil)
	}
	relative := strings.TrimPrefix(absolutePath, "/workspace/artifacts/")
	parts := strings.Split(relative, "/")
	for index := range parts {
		candidate := path.Join(parts[:index+1]...)
		info, err := fileSystem.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return Artifact{}, protocolError("artifact_not_found", err)
		}
		if err != nil || info.Mode()&fs.ModeSymlink != 0 || index < len(parts)-1 && !info.IsDir() {
			return Artifact{}, protocolError("artifact_path_unsafe", err)
		}
	}
	before, err := fileSystem.Lstat(relative)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxBytes || linkCount(before) != 1 {
		return Artifact{}, protocolError("artifact_file_unsafe", err)
	}
	file, err := fileSystem.Open(relative)
	if err != nil {
		return Artifact{}, protocolError("artifact_file_unsafe", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != before.Size() || linkCount(opened) != 1 || !sameFile(before, opened) {
		return Artifact{}, protocolError("artifact_file_changed", err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(body)) != opened.Size() || int64(len(body)) > maxBytes {
		return Artifact{}, protocolError("artifact_file_changed", err)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || after.Size() != opened.Size() || linkCount(after) != 1 || !sameFile(opened, after) || !sameSnapshot(opened, after) {
		return Artifact{}, protocolError("artifact_file_changed", err)
	}
	digest := sha256.Sum256(body)
	return Artifact{Body: body, Size: int64(len(body)), SHA256: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func sameSnapshot(left, right fs.FileInfo) bool {
	return left.Size() == right.Size() && left.Mode() == right.Mode() && left.ModTime().Equal(right.ModTime()) && sameFile(left, right)
}

func sameFile(left, right fs.FileInfo) bool {
	if left == nil || right == nil {
		return false
	}
	if left.Sys() != nil && right.Sys() != nil {
		return os.SameFile(left, right)
	}
	return left.Name() == right.Name() && left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func linkCount(info fs.FileInfo) uint64 {
	if info == nil || info.Sys() == nil {
		return 1
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 1
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 1
	}
	field := value.FieldByName("Nlink")
	if !field.IsValid() {
		return 1
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() < 0 {
			return 0
		}
		return uint64(field.Int())
	default:
		return 1
	}
}

func IsProtocolError(err error, code string) bool {
	var value *ProtocolError
	return errors.As(err, &value) && value.Code == code
}
