package sandboxio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

type fixedRepositoryFetcher struct{ repository *git.Repository }

func (f fixedRepositoryFetcher) Fetch(context.Context, Source, string) (*git.Repository, error) {
	return f.repository, nil
}

func TestGitMaterializerWritesExactRegularTreeAndAdoptsReceipt(t *testing.T) {
	repository, commit := testRepository(t, map[string]testSourceFile{
		"README.md":        {body: "hello\n", mode: 0o644},
		"bin/run":          {body: "#!/bin/false\n", mode: 0o755},
		"nested/value.txt": {body: "value\n", mode: 0o644},
	})
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "partial-from-crash"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(destination, sourceScratchName), 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "source", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/source", Commit: commit, Writable: false}
	manifest := SourceManifest{SchemaVersion: SourceManifestVersion, Sources: []Source{source}}
	canonical, err := MarshalSourceManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := GitMaterializer{Fetcher: fixedRepositoryFetcher{repository: repository}, ResolveDestination: func(Source) string { return destination }}
	receipt, err := materializer.Materialize(context.Background(), manifest, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SchemaVersion != SourceReceiptVersion || !digestPattern.MatchString(receipt.ManifestDigest) || !digestPattern.MatchString(receipt.Digest) || len(receipt.Sources) != 1 {
		t.Fatalf("receipt=%#v", receipt)
	}
	materialized := receipt.Sources[0]
	if materialized.Commit != commit || materialized.Tree == "" || materialized.FileCount != 3 || materialized.TotalBytes != int64(len("hello\n#!/bin/false\nvalue\n")) || !digestPattern.MatchString(materialized.ContentDigest) {
		t.Fatalf("materialized=%#v", materialized)
	}
	if _, err := os.Stat(filepath.Join(destination, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Git metadata escaped into the Sandbox source mount")
	}
	for _, absent := range []string{"partial-from-crash", sourceScratchName} {
		if _, err := os.Stat(filepath.Join(destination, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restart residue %s remains", absent)
		}
	}
	for name, mode := range map[string]os.FileMode{"README.md": 0o400, "bin/run": 0o500, "nested/value.txt": 0o400} {
		info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("%s stat: %v", name, err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s mode=%v", name, info.Mode().Perm())
		}
	}
	adopted, err := materializer.Materialize(context.Background(), manifest, canonical)
	if err != nil || adopted.Digest != receipt.Digest {
		t.Fatalf("adopted=%#v err=%v", adopted, err)
	}
	if err := os.Chmod(filepath.Join(destination, "README.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.Materialize(context.Background(), manifest, canonical); !IsProtocolError(err, "source_materialization_changed") {
		t.Fatalf("changed materialization error=%v", err)
	}
}

func TestSourceTraversalBoundsRepeatedTreesDepthAndCancellation(t *testing.T) {
	repository, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyHash := storeTree(t, repository, &object.Tree{})
	root := &object.Tree{}
	for index := range 5 {
		root.Entries = append(root.Entries, object.TreeEntry{Name: fmt.Sprintf("dir-%d", index), Mode: filemode.Dir, Hash: emptyHash})
	}
	files := []sourceFile{}
	err = collectTreeFiles(context.Background(), repository, root, "", 0,
		&sourceTreeBudget{maxEntries: 4, maxDepth: 8, maxPathBytes: 128}, &files)
	if !IsProtocolError(err, "source_tree_too_large") {
		t.Fatalf("repeated subtree error=%v", err)
	}

	childHash := emptyHash
	for depth := range 3 {
		childHash = storeTree(t, repository, &object.Tree{Entries: []object.TreeEntry{{Name: fmt.Sprintf("level-%d", depth), Mode: filemode.Dir, Hash: childHash}}})
	}
	deep, err := repository.TreeObject(childHash)
	if err != nil {
		t.Fatal(err)
	}
	err = collectTreeFiles(context.Background(), repository, deep, "", 0,
		&sourceTreeBudget{maxEntries: 10, maxDepth: 1, maxPathBytes: 128}, &files)
	if !IsProtocolError(err, "source_tree_too_large") {
		t.Fatalf("deep tree error=%v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := collectTreeFiles(canceled, repository, &object.Tree{}, "", 0,
		&sourceTreeBudget{maxEntries: 1, maxDepth: 1, maxPathBytes: 1}, &files); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled traversal error=%v", err)
	}
}

func TestCanceledMaterializationCleansScratchAndRetries(t *testing.T) {
	repository, commit := testRepository(t, map[string]testSourceFile{"file": {body: "value", mode: 0o644}})
	destination := t.TempDir()
	source := Source{Name: "source", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/source", Commit: commit}
	manifest := SourceManifest{SchemaVersion: SourceManifestVersion, Sources: []Source{source}}
	canonical, _ := MarshalSourceManifest(manifest)
	materializer := GitMaterializer{Fetcher: fixedRepositoryFetcher{repository}, ResolveDestination: func(Source) string { return destination }}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := materializer.Materialize(canceled, manifest, canonical); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled materialization error=%v", err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled materialization residue=%v err=%v", entries, err)
	}
	if _, err := materializer.Materialize(context.Background(), manifest, canonical); err != nil {
		t.Fatalf("retry after cancellation failed: %v", err)
	}
}

func TestGitMaterializerRejectsSymlinkSubstituteAndLFSPointer(t *testing.T) {
	for name, files := range map[string]map[string]testSourceFile{
		"symlink":        {"escape": {body: "/etc/passwd", mode: os.ModeSymlink | 0o777}},
		"lfs":            {"large.bin": {body: "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 123\n", mode: 0o644}},
		"scratch name":   {sourceScratchName + "/attack": {body: "hidden", mode: 0o644}},
		"receipt marker": {sourceMarkerName: {body: "substitute", mode: 0o644}},
	} {
		t.Run(name, func(t *testing.T) {
			repository, commit := testRepository(t, files)
			destination := t.TempDir()
			source := Source{Name: "source", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/source", Commit: commit}
			manifest := SourceManifest{SchemaVersion: SourceManifestVersion, Sources: []Source{source}}
			canonical, _ := MarshalSourceManifest(manifest)
			materializer := GitMaterializer{Fetcher: fixedRepositoryFetcher{repository}, ResolveDestination: func(Source) string { return destination }}
			_, err := materializer.Materialize(context.Background(), manifest, canonical)
			if name == "lfs" && !IsProtocolError(err, "source_lfs_unsupported") || name != "lfs" && !IsProtocolError(err, "source_tree_unsafe") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSourceTransportRejectsCredentialsOriginEscapeAndBudgetOverflow(t *testing.T) {
	allowed, _ := url.Parse("https://example.test/owner/repo.git/info/refs?service=git-upload-pack")
	transport := &sourceRoundTripper{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString("12345")), ContentLength: -1, Header: make(http.Header)}, nil
	}), scheme: "https", host: "example.test", pathPrefix: "/owner/repo.git/", budget: &networkBudget{remaining: 4}}
	request := &http.Request{Method: http.MethodGet, URL: allowed, Header: make(http.Header)}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("chunked response exceeded the network budget")
	}
	_ = response.Body.Close()
	for label, mutate := range map[string]func(*http.Request){
		"credential": func(value *http.Request) { value.Header.Set("Authorization", "Bearer forbidden") },
		"host":       func(value *http.Request) { value.URL.Host = "attacker.test" },
		"path":       func(value *http.Request) { value.URL.Path = "/other/repo.git/info/refs" },
		"query":      func(value *http.Request) { value.URL.RawQuery = "token=forbidden" },
	} {
		t.Run(label, func(t *testing.T) {
			candidate := request.Clone(context.Background())
			candidate.URL = cloneURL(request.URL)
			candidate.Header = request.Header.Clone()
			mutate(candidate)
			if _, err := transport.RoundTrip(candidate); err == nil {
				t.Fatal("unsafe Git request accepted")
			}
		})
	}
	redirect := request.Clone(context.Background())
	redirect.URL = cloneURL(request.URL)
	redirect.URL.Host = "attacker.test"
	if err := sameOriginRedirect(redirect, []*http.Request{request}); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
}

func TestSecureGitFetcherAgainstPublicExactCommit(t *testing.T) {
	if os.Getenv("BLAZN_TEST_PUBLIC_GIT") != "1" {
		t.Skip("set BLAZN_TEST_PUBLIC_GIT=1 on a disposable network-enabled lane")
	}
	destination := t.TempDir()
	source := Source{Name: "source", URL: "https://github.com/git-fixtures/basic.git", Destination: "/workspace/src/source", Commit: "6ecf0ef2c2dffb796033e5a02219af86ec6584e5"}
	manifest := SourceManifest{SchemaVersion: SourceManifestVersion, Sources: []Source{source}}
	canonical, err := MarshalSourceManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := GitMaterializer{Fetcher: SecureGitFetcher{MaxNetworkBytes: 64 << 20}, ResolveDestination: func(Source) string { return destination }}
	receipt, err := materializer.Materialize(context.Background(), manifest, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Sources) != 1 || receipt.Sources[0].Commit != source.Commit || receipt.Sources[0].FileCount == 0 || receipt.Sources[0].TotalBytes == 0 {
		t.Fatalf("receipt=%#v", receipt)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func cloneURL(value *url.URL) *url.URL {
	copy := *value
	return &copy
}

type testSourceFile struct {
	body string
	mode os.FileMode
}

func testRepository(t *testing.T, files map[string]testSourceFile) (*git.Repository, string) {
	t.Helper()
	directory := t.TempDir()
	repository, err := git.PlainInit(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range files {
		full := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if file.mode&os.ModeSymlink != 0 {
			if err := os.Symlink(file.body, full); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(full, []byte(file.body), file.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := worktree.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.test", When: time.Unix(1, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	return repository, hash.String()
}

func storeTree(t *testing.T, repository *git.Repository, tree *object.Tree) plumbing.Hash {
	t.Helper()
	encoded := repository.Storer.NewEncodedObject()
	if err := tree.Encode(encoded); err != nil {
		t.Fatal(err)
	}
	hash, err := repository.Storer.SetEncodedObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
