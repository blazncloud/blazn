package sandboxio

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactReadRejectsTraversalSymlinkNonregularHardlinkAndSize(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "result.txt"), []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileSystem, err := OpenRootFileSystem(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer fileSystem.Close()
	artifact, err := ReadArtifact(fileSystem, "/workspace/artifacts/result.txt", MaxArtifactBytes)
	if err != nil || string(artifact.Body) != "result" || artifact.Size != 6 || !digestPattern.MatchString(artifact.SHA256) {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}

	if err := os.Symlink("result.txt", filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArtifact(fileSystem, "/workspace/artifacts/link", MaxArtifactBytes); !IsProtocolError(err, "artifact_path_unsafe") {
		t.Fatalf("symlink error=%v", err)
	}
	if err := os.Mkdir(filepath.Join(directory, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArtifact(fileSystem, "/workspace/artifacts/folder", MaxArtifactBytes); !IsProtocolError(err, "artifact_file_unsafe") {
		t.Fatalf("directory error=%v", err)
	}
	if err := os.Link(filepath.Join(directory, "result.txt"), filepath.Join(directory, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArtifact(fileSystem, "/workspace/artifacts/result.txt", MaxArtifactBytes); !IsProtocolError(err, "artifact_file_unsafe") {
		t.Fatalf("hardlink error=%v", err)
	}
	if _, err := ReadArtifact(fileSystem, "/workspace/artifacts/../result.txt", MaxArtifactBytes); !IsProtocolError(err, "artifact_request_invalid") {
		t.Fatalf("traversal error=%v", err)
	}
	if _, err := ReadArtifact(fileSystem, "/workspace/artifacts/hardlink", 2); !IsProtocolError(err, "artifact_file_unsafe") {
		t.Fatalf("size error=%v", err)
	}
}

func TestArtifactReadRejectsChangeAroundRead(t *testing.T) {
	now := time.Unix(1, 0)
	before := fakeInfo{name: "result", size: 6, mode: 0o600, modified: now}
	after := fakeInfo{name: "result", size: 7, mode: 0o600, modified: now.Add(time.Second)}
	file := &fakeArtifactFile{body: strings.NewReader("result"), stats: []fs.FileInfo{before, after}}
	fileSystem := &fakeArtifactFS{info: before, file: file}
	if _, err := ReadArtifact(fileSystem, "/workspace/artifacts/result", MaxArtifactBytes); !IsProtocolError(err, "artifact_file_changed") {
		t.Fatalf("change error=%v", err)
	}
}

type fakeArtifactFS struct {
	info fs.FileInfo
	file ArtifactFile
}

func (f *fakeArtifactFS) Lstat(string) (fs.FileInfo, error) { return f.info, nil }
func (f *fakeArtifactFS) Open(string) (ArtifactFile, error)  { return f.file, nil }

type fakeArtifactFile struct {
	body  *strings.Reader
	stats []fs.FileInfo
	index int
}

func (f *fakeArtifactFile) Read(body []byte) (int, error) { return f.body.Read(body) }
func (f *fakeArtifactFile) Close() error                  { return nil }
func (f *fakeArtifactFile) Stat() (fs.FileInfo, error) {
	index := f.index
	if index >= len(f.stats) {
		index = len(f.stats) - 1
	}
	f.index++
	return f.stats[index], nil
}

type fakeInfo struct {
	name     string
	size     int64
	mode     fs.FileMode
	modified time.Time
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return f.modified }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }
