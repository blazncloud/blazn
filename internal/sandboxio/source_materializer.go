package sandboxio

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

const (
	SourceReceiptVersion       = "blazn.dev/sandbox-source-materialization/v1"
	MaxSourceFiles             = 100_000
	MaxSourceBytes       int64 = 256 << 20
	MaxGitNetworkBytes         = 512 << 20
	sourceMarkerName           = ".blazn-source-materialization.json"
)

type SourceMaterialization struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Destination   string `json:"destination"`
	Commit        string `json:"commit"`
	Tree          string `json:"tree"`
	ContentDigest string `json:"contentDigest"`
	FileCount     int    `json:"fileCount"`
	TotalBytes    int64  `json:"totalBytes"`
	Writable      bool   `json:"writable"`
}

type SourceMaterializationReceipt struct {
	SchemaVersion  string                  `json:"schemaVersion"`
	ManifestDigest string                  `json:"manifestDigest"`
	Sources        []SourceMaterialization `json:"sources"`
	Digest         string                  `json:"digest"`
}

type GitRepositoryFetcher interface {
	Fetch(context.Context, Source) (*git.Repository, error)
}

type GitMaterializer struct {
	Fetcher            GitRepositoryFetcher
	ResolveDestination func(Source) string
}

type SecureGitFetcher struct {
	MaxNetworkBytes int64
}

type sourceFile struct {
	path string
	mode filemode.FileMode
	blob *object.Blob
	sha  string
}

func (m GitMaterializer) Materialize(ctx context.Context, manifest SourceManifest, canonicalManifest []byte) (SourceMaterializationReceipt, error) {
	if m.Fetcher == nil {
		return SourceMaterializationReceipt{}, protocolError("source_fetcher_unavailable", nil)
	}
	validated, canonical, err := ValidateSourceManifest(canonicalManifest)
	if err != nil || !sameManifest(validated, manifest) || string(canonical) != string(canonicalManifest) {
		return SourceMaterializationReceipt{}, protocolError("source_manifest_invalid", err)
	}
	manifestHash := sha256.Sum256(canonical)
	receipt := SourceMaterializationReceipt{SchemaVersion: SourceReceiptVersion, ManifestDigest: "sha256:" + hex.EncodeToString(manifestHash[:]), Sources: make([]SourceMaterialization, 0, len(manifest.Sources))}
	for _, source := range manifest.Sources {
		materialized, err := m.materializeSource(ctx, source)
		if err != nil {
			return SourceMaterializationReceipt{}, err
		}
		receipt.Sources = append(receipt.Sources, materialized)
	}
	receipt.Digest, err = sourceReceiptDigest(receipt)
	if err != nil {
		return SourceMaterializationReceipt{}, protocolError("source_receipt_invalid", err)
	}
	return receipt, nil
}

func (m GitMaterializer) materializeSource(ctx context.Context, source Source) (SourceMaterialization, error) {
	destination := source.Destination
	if m.ResolveDestination != nil {
		destination = m.ResolveDestination(source)
	}
	root, entries, err := openEmptyOrMaterializedDestination(destination)
	if err != nil {
		return SourceMaterialization{}, err
	}
	defer root.Close()
	if len(entries) != 0 {
		return adoptMaterialization(ctx, root, source)
	}
	repository, err := m.Fetcher.Fetch(ctx, source)
	if err != nil {
		return SourceMaterialization{}, protocolError("source_fetch_failed", err)
	}
	commit, err := repository.CommitObject(plumbing.NewHash(source.Commit))
	if err != nil || commit.Hash.String() != source.Commit {
		return SourceMaterialization{}, protocolError("source_commit_unavailable", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return SourceMaterialization{}, protocolError("source_tree_unavailable", err)
	}
	files, contentDigest, total, err := canonicalSourceFiles(repository, tree)
	if err != nil {
		return SourceMaterialization{}, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return SourceMaterialization{}, err
		}
		if directory := path.Dir(file.path); directory != "." {
			if err := root.MkdirAll(directory, 0o700); err != nil {
				return SourceMaterialization{}, protocolError("source_write_failed", err)
			}
		}
		mode := os.FileMode(0o400)
		if source.Writable {
			mode = 0o600
		}
		if file.mode == filemode.Executable {
			mode |= 0o100
		}
		target, err := root.OpenFile(file.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return SourceMaterialization{}, protocolError("source_write_failed", err)
		}
		reader, err := file.blob.Reader()
		if err == nil {
			_, err = io.CopyN(target, reader, file.blob.Size)
		}
		closeErr := target.Close()
		if reader != nil {
			closeErr = errors.Join(closeErr, reader.Close())
		}
		if err != nil || closeErr != nil {
			return SourceMaterialization{}, protocolError("source_write_failed", errors.Join(err, closeErr))
		}
	}
	result := SourceMaterialization{Name: source.Name, URL: source.URL, Destination: source.Destination, Commit: source.Commit,
		Tree: tree.Hash.String(), ContentDigest: contentDigest, FileCount: len(files), TotalBytes: total, Writable: source.Writable}
	marker, err := json.Marshal(result)
	if err != nil {
		return SourceMaterialization{}, protocolError("source_receipt_invalid", err)
	}
	markerFile, err := root.OpenFile(sourceMarkerName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return SourceMaterialization{}, protocolError("source_write_failed", err)
	}
	if _, err := markerFile.Write(append(marker, '\n')); err != nil {
		_ = markerFile.Close()
		return SourceMaterialization{}, protocolError("source_write_failed", err)
	}
	if err := markerFile.Sync(); err != nil {
		_ = markerFile.Close()
		return SourceMaterialization{}, protocolError("source_write_failed", err)
	}
	if err := markerFile.Close(); err != nil {
		return SourceMaterialization{}, protocolError("source_write_failed", err)
	}
	return result, nil
}

func openEmptyOrMaterializedDestination(destination string) (*os.Root, []os.DirEntry, error) {
	info, err := os.Lstat(destination)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, protocolError("source_destination_unsafe", err)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, nil, protocolError("source_destination_unsafe", err)
	}
	entries, err := readRootDir(root, ".")
	if err != nil {
		root.Close()
		return nil, nil, protocolError("source_destination_unsafe", err)
	}
	return root, entries, nil
}

func adoptMaterialization(ctx context.Context, root *os.Root, source Source) (SourceMaterialization, error) {
	marker, err := root.Open(sourceMarkerName)
	if err != nil {
		return SourceMaterialization{}, protocolError("source_destination_dirty", err)
	}
	defer marker.Close()
	encoded, err := io.ReadAll(io.LimitReader(marker, MaxManifestBytes+1))
	if err != nil || len(encoded) > MaxManifestBytes {
		return SourceMaterialization{}, protocolError("source_receipt_invalid", err)
	}
	var receipt SourceMaterialization
	if err := decodeClosed(encoded, &receipt); err != nil || receipt.Name != source.Name || receipt.URL != source.URL || receipt.Destination != source.Destination || receipt.Commit != source.Commit || receipt.Writable != source.Writable || !commitPattern.MatchString(receipt.Tree) || !digestPattern.MatchString(receipt.ContentDigest) || receipt.FileCount < 0 || receipt.FileCount > MaxSourceFiles || receipt.TotalBytes < 0 || receipt.TotalBytes > MaxSourceBytes {
		return SourceMaterialization{}, protocolError("source_receipt_invalid", err)
	}
	if err := verifyMaterializedFiles(ctx, root, receipt); err != nil {
		return SourceMaterialization{}, err
	}
	return receipt, nil
}

func canonicalSourceFiles(repository *git.Repository, tree *object.Tree) ([]sourceFile, string, int64, error) {
	files := make([]sourceFile, 0)
	if err := collectTreeFiles(repository, tree, "", &files); err != nil {
		return nil, "", 0, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	if len(files) > MaxSourceFiles {
		return nil, "", 0, protocolError("source_tree_too_large", nil)
	}
	hash := sha256.New()
	var total int64
	for index := range files {
		file := &files[index]
		if file.blob.Size < 0 || file.blob.Size > MaxSourceBytes-total {
			return nil, "", 0, protocolError("source_tree_too_large", nil)
		}
		reader, err := file.blob.Reader()
		if err != nil {
			return nil, "", 0, protocolError("source_object_invalid", err)
		}
		contentHash := sha256.New()
		written, copyErr := io.CopyN(contentHash, reader, file.blob.Size)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil || written != file.blob.Size {
			return nil, "", 0, protocolError("source_object_invalid", errors.Join(copyErr, closeErr))
		}
		probe, err := file.blob.Reader()
		if err != nil {
			return nil, "", 0, protocolError("source_object_invalid", err)
		}
		prefix := make([]byte, 64)
		n, probeErr := io.ReadFull(probe, prefix)
		if probeErr != nil && !errors.Is(probeErr, io.ErrUnexpectedEOF) && !errors.Is(probeErr, io.EOF) {
			_ = probe.Close()
			return nil, "", 0, protocolError("source_object_invalid", probeErr)
		}
		if closeErr := probe.Close(); closeErr != nil {
			return nil, "", 0, protocolError("source_object_invalid", closeErr)
		}
		if strings.HasPrefix(string(prefix[:n]), "version https://git-lfs.github.com/spec/v1") {
			return nil, "", 0, protocolError("source_lfs_unsupported", nil)
		}
		file.sha = hex.EncodeToString(contentHash.Sum(nil))
		writeDigestField(hash, file.path)
		writeDigestField(hash, file.mode.String())
		writeDigestField(hash, fmt.Sprintf("%d", file.blob.Size))
		writeDigestField(hash, file.sha)
		total += file.blob.Size
	}
	return files, "sha256:" + hex.EncodeToString(hash.Sum(nil)), total, nil
}

func collectTreeFiles(repository *git.Repository, tree *object.Tree, prefix string, files *[]sourceFile) error {
	for _, entry := range tree.Entries {
		if entry.Name == "" || entry.Name == "." || entry.Name == ".." || strings.ContainsAny(entry.Name, "/\\\x00") {
			return protocolError("source_tree_unsafe", nil)
		}
		name := path.Join(prefix, entry.Name)
		switch entry.Mode {
		case filemode.Dir:
			child, err := repository.TreeObject(entry.Hash)
			if err != nil {
				return protocolError("source_object_invalid", err)
			}
			if err := collectTreeFiles(repository, child, name, files); err != nil {
				return err
			}
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			blob, err := repository.BlobObject(entry.Hash)
			if err != nil {
				return protocolError("source_object_invalid", err)
			}
			*files = append(*files, sourceFile{path: name, mode: entry.Mode, blob: blob})
			if len(*files) > MaxSourceFiles {
				return protocolError("source_tree_too_large", nil)
			}
		case filemode.Symlink, filemode.Submodule:
			return protocolError("source_tree_unsafe", nil)
		default:
			return protocolError("source_tree_unsafe", nil)
		}
	}
	return nil
}

func verifyMaterializedFiles(ctx context.Context, root *os.Root, receipt SourceMaterialization) error {
	entries, err := readRootDir(root, ".")
	if err != nil || len(entries) == 0 {
		return protocolError("source_materialization_changed", err)
	}
	hash := sha256.New()
	count, total := 0, int64(0)
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		items, err := readRootDir(root, directory)
		if err != nil {
			return err
		}
		for _, item := range items {
			name := path.Join(directory, item.Name())
			if directory == "." {
				name = item.Name()
			}
			if name == sourceMarkerName {
				continue
			}
			info, err := root.Lstat(name)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return errors.Join(err, errors.New("unsafe materialized source entry"))
			}
			if info.IsDir() {
				if err := walk(name); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxSourceBytes-total {
				return errors.New("unsafe materialized source file")
			}
			file, err := root.Open(name)
			if err != nil {
				return err
			}
			contentHash := sha256.New()
			written, copyErr := io.CopyN(contentHash, file, info.Size())
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil || written != info.Size() {
				return errors.Join(copyErr, closeErr)
			}
			mode := filemode.Regular
			if info.Mode().Perm()&0o100 != 0 {
				mode = filemode.Executable
			}
			writeDigestField(hash, name)
			writeDigestField(hash, mode.String())
			writeDigestField(hash, fmt.Sprintf("%d", info.Size()))
			writeDigestField(hash, hex.EncodeToString(contentHash.Sum(nil)))
			count++
			total += info.Size()
		}
		return nil
	}
	if err := walk("."); err != nil || count != receipt.FileCount || total != receipt.TotalBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != receipt.ContentDigest {
		return protocolError("source_materialization_changed", err)
	}
	return nil
}

func readRootDir(root *os.Root, name string) ([]os.DirEntry, error) {
	directory, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(-1)
	return entries, errors.Join(readErr, directory.Close())
}

func sourceReceiptDigest(receipt SourceMaterializationReceipt) (string, error) {
	receipt.Digest = ""
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func writeDigestField(writer io.Writer, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = io.WriteString(writer, value)
}

func sameManifest(left, right SourceManifest) bool {
	encodedLeft, _ := json.Marshal(left)
	encodedRight, _ := json.Marshal(right)
	return string(encodedLeft) == string(encodedRight)
}

var gitTransportMu sync.Mutex

func (f SecureGitFetcher) Fetch(ctx context.Context, source Source) (*git.Repository, error) {
	if len(source.Commit) != 40 {
		return nil, protocolError("source_commit_algorithm_unsupported", nil)
	}
	parsed, err := url.Parse(source.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, protocolError("source_url_invalid", err)
	}
	limit := f.MaxNetworkBytes
	if limit == 0 {
		limit = MaxGitNetworkBytes
	}
	if limit < 1 || limit > MaxGitNetworkBytes {
		return nil, protocolError("source_network_limit_invalid", nil)
	}
	budget := &networkBudget{remaining: limit}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.MaxResponseHeaderBytes = MaxHeaderBytes
	base.MaxConnsPerHost = 2
	base.MaxIdleConnsPerHost = 1
	transport := &sourceRoundTripper{next: base, scheme: parsed.Scheme, host: parsed.Host, pathPrefix: strings.TrimSuffix(parsed.EscapedPath(), "/") + "/", budget: budget}
	httpClient := &http.Client{Transport: transport, CheckRedirect: sameOriginRedirect}
	gitTransportMu.Lock()
	defer gitTransportMu.Unlock()
	previous := transportclient.Protocols["https"]
	transportclient.InstallProtocol("https", githttp.NewClient(httpClient))
	defer transportclient.InstallProtocol("https", previous)
	store := memory.NewStorage()
	repository, err := git.Init(store, nil)
	if err != nil {
		return nil, err
	}
	remote, err := repository.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{source.URL}})
	if err != nil {
		return nil, err
	}
	ref := plumbing.ReferenceName("refs/blazn/source")
	refspec := gitconfig.RefSpec("+" + source.Commit + ":" + ref.String())
	if err := remote.FetchContext(ctx, &git.FetchOptions{RefSpecs: []gitconfig.RefSpec{refspec}, Depth: 1, Tags: git.NoTags, Force: true}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, err
	}
	return repository, nil
}

type networkBudget struct {
	mu        sync.Mutex
	remaining int64
}

type sourceRoundTripper struct {
	next       http.RoundTripper
	scheme     string
	host       string
	pathPrefix string
	budget     *networkBudget
}

func (t *sourceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.scheme || request.URL.Host != t.host || request.URL.User != nil || !strings.HasPrefix(request.URL.EscapedPath(), t.pathPrefix) ||
		request.Method != http.MethodGet && request.Method != http.MethodPost || request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" {
		return nil, errors.New("Git transport request escaped the credential-free source boundary")
	}
	if request.URL.RawQuery != "" && request.URL.RawQuery != "service=git-upload-pack" {
		return nil, errors.New("Git transport query is outside smart HTTP")
	}
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > 0 && !t.budget.reserve(response.ContentLength) {
		response.Body.Close()
		return nil, errors.New("Git response exceeds source network budget")
	}
	response.Body = &budgetReadCloser{reader: response.Body, closer: response.Body, budget: t.budget, reserved: response.ContentLength > 0}
	return response, nil
}

func sameOriginRedirect(request *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) > 4 || request.URL.Scheme != via[0].URL.Scheme || request.URL.Host != via[0].URL.Host || request.URL.User != nil {
		return errors.New("Git redirect escaped the exact source origin")
	}
	return nil
}

func (b *networkBudget) reserve(size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if size < 0 || size > b.remaining {
		return false
	}
	b.remaining -= size
	return true
}

type budgetReadCloser struct {
	reader   io.Reader
	closer   io.Closer
	budget   *networkBudget
	reserved bool
}

func (r *budgetReadCloser) Read(value []byte) (int, error) {
	n, err := r.reader.Read(value)
	if n > 0 && !r.reserved && !r.budget.reserve(int64(n)) {
		return 0, errors.New("Git response exceeds source network budget")
	}
	return n, err
}

func (r *budgetReadCloser) Close() error { return r.closer.Close() }
