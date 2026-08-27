package sandboxcontroller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	maxKubernetesCABytes       = 1 << 20
	maxServiceAccountTokenSize = 64 << 10
	maxKubernetesResponseBytes = 4 << 20
	maxKubernetesHeaderBytes   = 64 << 10
	kubernetesRequestTimeout   = 30 * time.Second
	kubernetesCertificatePEM   = "-----BEGIN CERTIFICATE-----"
)

type kubernetesFileOps struct {
	lstat        func(string) (os.FileInfo, error)
	evalSymlinks func(string) (string, error)
	open         func(string) (*os.File, error)
	afterRead    func() error
}

type kubernetesDirectoryIdentity struct {
	path string
	info os.FileInfo
}

type bearerTokenTransport struct {
	base      http.RoundTripper
	readToken func() (string, error)
}

type boundedKubernetesTransport struct{ base http.RoundTripper }

type noArtifactExporter struct{}

func (noArtifactExporter) Export(context.Context, sandboxcontrol.SandboxRecord, []sandboxcontrol.ArtifactExport) ([]sandboxcontrol.ArtifactReceipt, error) {
	return []sandboxcontrol.ArtifactReceipt{}, nil
}

// NewKubernetesBackendFromConfig constructs the complete authenticated
// in-cluster boundary or returns an error before a controller can claim work.
// The projected token is deliberately absent from both configuration and the
// adapter: bearerTokenTransport reads and validates it for every request.
func NewKubernetesBackendFromConfig(config KubernetesConfig) (*KubernetesBackend, error) {
	client, err := newKubernetesHTTPClient(config)
	if err != nil {
		return nil, err
	}
	adapter, err := sandboxcontrol.New(sandboxcontrol.Config{
		BaseURL: config.BaseURL, HTTPClient: client, RuntimeClasses: map[string]sandboxcontrol.RuntimeCapability{},
		Exporter: noArtifactExporter{}, MaxResponseBytes: maxKubernetesResponseBytes,
	})
	if err != nil {
		return nil, errors.New("sandbox controller Kubernetes adapter configuration is invalid")
	}
	health := func(ctx context.Context) error { return kubernetesAPIHealth(ctx, client, config.BaseURL) }
	var sourceRuntime *KubernetesSourceRuntime
	var artifactRuntime *KubernetesArtifactRuntime
	var ioController *sandboxio.Controller
	needsIO := len(config.SourceDNSCIDRs) != 0 || len(config.SourceHostCIDRs) != 0 || config.ArtifactEndpoint != ""
	if needsIO {
		execTransport, err := newKubernetesExecTransport(config)
		if err != nil {
			return nil, err
		}
		ioController, err = sandboxio.NewController(sandboxio.ControllerConfig{Transport: execTransport,
			Owners: kubernetesPodOwnerVerifier{baseURL: config.BaseURL, client: client}, Timeout: sandboxio.SourceTimeout})
		if err != nil {
			return nil, err
		}
	}
	if len(config.SourceDNSCIDRs) != 0 || len(config.SourceHostCIDRs) != 0 {
		network, err := NewKubernetesSourceNetwork(KubernetesSourceNetworkConfig{BaseURL: config.BaseURL, HTTPClient: client,
			DNSCIDRs: config.SourceDNSCIDRs, SourceCIDRs: config.SourceHostCIDRs})
		if err != nil {
			return nil, err
		}
		sourceRuntime, err = NewKubernetesSourceRuntime(KubernetesSourceRuntimeConfig{Network: network, IO: ioController})
		if err != nil {
			return nil, err
		}
	}
	if config.ArtifactEndpoint != "" {
		var roots *x509.CertPool
		if config.ArtifactCAFile != "" {
			// A private object endpoint presents a certificate from a private
			// CA the image's system roots cannot know; trust exactly that CA,
			// loaded through the same hardened PEM pool reader as the API CA.
			pool, err := readKubernetesCA(config.ArtifactCAFile)
			if err != nil {
				return nil, errors.New("artifact object CA file is invalid")
			}
			roots = pool
		} else {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				return nil, errors.New("artifact object system roots are unavailable")
			}
			roots = pool
		}
		objects, err := NewS3ArtifactStore(S3ArtifactStoreConfig{Endpoint: config.ArtifactEndpoint, Region: config.ArtifactRegion,
			Bucket: config.ArtifactBucket, AccessKeyFile: config.ArtifactAccessKeyFile, SecretKeyFile: config.ArtifactSecretKeyFile, RootCAs: roots})
		if err != nil {
			return nil, err
		}
		artifactRuntime, err = NewKubernetesArtifactRuntime(KubernetesArtifactRuntimeConfig{IO: ioController, Objects: objects})
		if err != nil {
			return nil, err
		}
	}
	return NewKubernetesBackend(KubernetesBackendConfig{Adapter: adapter, Health: health, ArtifactExportSupported: artifactRuntime != nil,
		HelperImage: config.HelperImage, SourceRuntime: sourceRuntime, ArtifactRuntime: artifactRuntime})
}

func newKubernetesHTTPClient(config KubernetesConfig) (*http.Client, error) {
	endpoint, err := url.Parse(config.BaseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawPath != "" ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Hostname() == "" ||
		!validKubernetesHost(endpoint.Hostname()) || !validKubernetesPort(endpoint.Port()) ||
		endpoint.Host != net.JoinHostPort(endpoint.Hostname(), endpoint.Port()) ||
		!validAbsoluteFilePath(config.CAFile) || !validAbsoluteFilePath(config.TokenFile) || config.CAFile == config.TokenFile {
		return nil, errors.New("sandbox controller Kubernetes client configuration is invalid")
	}
	roots, err := readKubernetesCA(config.CAFile)
	if err != nil {
		return nil, err
	}
	// Validate the projected token during construction, then discard it. The
	// transport performs the same read afresh immediately before every request.
	if _, err := readProjectedServiceAccountToken(config.TokenFile); err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           8,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        8,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxKubernetesHeaderBytes,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	}
	authenticated := &bearerTokenTransport{base: transport, readToken: func() (string, error) {
		return readProjectedServiceAccountToken(config.TokenFile)
	}}
	return &http.Client{
		Transport: &boundedKubernetesTransport{base: authenticated},
		Timeout:   kubernetesRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func (transport *bearerTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.base == nil || transport.readToken == nil {
		return nil, errors.New("Kubernetes authentication transport is unavailable")
	}
	if request == nil || request.URL == nil || request.URL.Scheme != "https" || request.Header.Get("Authorization") != "" {
		return nil, errors.New("Kubernetes request authentication boundary is invalid")
	}
	token, err := transport.readToken()
	if err != nil {
		return nil, err
	}
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+token)
	return transport.base.RoundTrip(copy)
}

func (transport *boundedKubernetesTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.base == nil {
		return nil, errors.New("Kubernetes HTTP transport is unavailable")
	}
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	closeWithError := func(message string) (*http.Response, error) {
		_ = response.Body.Close()
		return nil, errors.New(message)
	}
	if response.ContentLength > maxKubernetesResponseBytes {
		return closeWithError("Kubernetes API response exceeds the configured limit")
	}
	if response.StatusCode != http.StatusNoContent && response.ContentLength != 0 {
		mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if parseErr != nil || mediaType != "application/json" {
			return closeWithError("Kubernetes API response content type is invalid")
		}
	}
	response.Body = &boundedReadCloser{reader: io.LimitReader(response.Body, maxKubernetesResponseBytes+1), closer: response.Body}
	return response, nil
}

type boundedReadCloser struct {
	reader io.Reader
	closer io.Closer
	read   int64
}

func (body *boundedReadCloser) Read(buffer []byte) (int, error) {
	if body.read >= maxKubernetesResponseBytes {
		probe := make([]byte, 1)
		count, err := body.reader.Read(probe)
		if count != 0 {
			return 0, errors.New("Kubernetes API response exceeds the configured limit")
		}
		return 0, err
	}
	if int64(len(buffer)) > maxKubernetesResponseBytes-body.read {
		buffer = buffer[:maxKubernetesResponseBytes-body.read]
	}
	count, err := body.reader.Read(buffer)
	body.read += int64(count)
	return count, err
}

func (body *boundedReadCloser) Close() error { return body.closer.Close() }

func kubernetesAPIHealth(ctx context.Context, client *http.Client, baseURL string) error {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/apis/agents.x-k8s.io/v1beta1/namespaces/" + sandboxcontrol.Namespace + "/sandboxes?limit=1"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("Kubernetes API health request cannot be constructed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "blazn-sandbox-controller/v1")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Kubernetes API health request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Kubernetes API health returned HTTP %d", response.StatusCode)
	}
	contents, err := io.ReadAll(response.Body)
	if err != nil || len(contents) == 0 || len(contents) > maxKubernetesResponseBytes {
		return errors.New("Kubernetes API health response is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var document struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := decoder.Decode(&document); err != nil || decoder.Decode(&struct{}{}) != io.EOF || document.Items == nil {
		return errors.New("Kubernetes API health response is invalid")
	}
	return nil
}

func readKubernetesCA(path string) (*x509.CertPool, error) {
	contents, err := readStableRegularFile(path, maxKubernetesCABytes, false, kubernetesFileOps{
		lstat: os.Lstat,
		open: func(name string) (*os.File, error) {
			return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		},
	})
	if err != nil {
		return nil, errors.New("sandbox controller Kubernetes CA file is unsafe")
	}
	pool := x509.NewCertPool()
	remaining, certificates := bytes.TrimSpace(contents), 0
	for len(remaining) != 0 {
		if !bytes.HasPrefix(remaining, []byte(kubernetesCertificatePEM)) {
			return nil, errors.New("sandbox controller Kubernetes CA file is invalid")
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("sandbox controller Kubernetes CA file is invalid")
		}
		consumed := remaining[:len(remaining)-len(rest)]
		if bytes.Count(consumed, []byte("-----BEGIN ")) != 1 {
			return nil, errors.New("sandbox controller Kubernetes CA file is invalid")
		}
		certificate, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			return nil, errors.New("sandbox controller Kubernetes CA file is invalid")
		}
		pool.AddCert(certificate)
		certificates++
		remaining = bytes.TrimSpace(rest)
	}
	if certificates == 0 {
		return nil, errors.New("sandbox controller Kubernetes CA file is invalid")
	}
	return pool, nil
}

func readProjectedServiceAccountToken(path string) (string, error) {
	return readProjectedServiceAccountTokenWithOps(path, kubernetesFileOps{
		lstat: os.Lstat, evalSymlinks: filepath.EvalSymlinks,
		open: func(name string) (*os.File, error) {
			return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		},
	})
}

func readProjectedServiceAccountTokenWithOps(path string, ops kubernetesFileOps) (string, error) {
	unsafe := func() (string, error) {
		return "", errors.New("sandbox controller projected ServiceAccount token is unsafe")
	}
	if !validAbsoluteFilePath(path) || ops.lstat == nil || ops.evalSymlinks == nil || ops.open == nil {
		return unsafe()
	}
	original, err := ops.lstat(path)
	if err != nil || original.Mode()&os.ModeSymlink == 0 && !original.Mode().IsRegular() || !safeKubernetesOwner(original) {
		return unsafe()
	}
	realDirectory, err := ops.evalSymlinks(filepath.Dir(path))
	if err != nil {
		return unsafe()
	}
	resolved, err := ops.evalSymlinks(path)
	if err != nil || !pathWithin(realDirectory, resolved) {
		return unsafe()
	}
	directories, err := inspectKubernetesDirectoryChain(realDirectory, filepath.Dir(resolved), ops.lstat)
	if err != nil {
		return unsafe()
	}
	contents, err := readStableRegularFileWithOps(resolved, maxServiceAccountTokenSize, true, ops)
	if err != nil {
		return unsafe()
	}
	realDirectoryAfter, directoryErr := ops.evalSymlinks(filepath.Dir(path))
	resolvedAfter, err := ops.evalSymlinks(path)
	if directoryErr != nil || realDirectoryAfter != realDirectory || err != nil || resolvedAfter != resolved ||
		!stableKubernetesDirectoryChain(directories, ops.lstat) {
		return unsafe()
	}
	token := strings.TrimSpace(string(contents))
	if token == "" || len(token) > maxServiceAccountTokenSize || strings.ContainsAny(token, "\x00\r\n\t ") {
		return "", errors.New("sandbox controller projected ServiceAccount token is invalid")
	}
	return token, nil
}

func readStableRegularFile(path string, limit int64, projection bool, ops kubernetesFileOps) ([]byte, error) {
	return readStableRegularFileWithOps(path, limit, projection, ops)
}

func readStableRegularFileWithOps(path string, limit int64, projection bool, ops kubernetesFileOps) ([]byte, error) {
	info, err := ops.lstat(path)
	if err != nil || !safeKubernetesRegularFile(info, limit, projection) {
		return nil, errors.New("Kubernetes credential file cannot be inspected")
	}
	file, err := ops.open(path)
	if err != nil {
		return nil, errors.New("Kubernetes credential file cannot be opened")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !safeKubernetesRegularFile(opened, limit, projection) {
		return nil, errors.New("Kubernetes credential file changed during inspection")
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(contents) == 0 || int64(len(contents)) > limit {
		return nil, errors.New("Kubernetes credential file cannot be read")
	}
	if ops.afterRead != nil {
		if err := ops.afterRead(); err != nil {
			return nil, errors.New("Kubernetes credential file changed during read")
		}
	}
	finalFD, fdErr := file.Stat()
	finalPath, pathErr := ops.lstat(path)
	if fdErr != nil || pathErr != nil || !os.SameFile(info, finalFD) || !os.SameFile(finalFD, finalPath) ||
		!safeKubernetesRegularFile(finalFD, limit, projection) || !safeKubernetesRegularFile(finalPath, limit, projection) {
		return nil, errors.New("Kubernetes credential file changed during read")
	}
	return contents, nil
}

func safeKubernetesRegularFile(info os.FileInfo, limit int64, projection bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit ||
		info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 != 0 || !safeKubernetesOwner(info) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Nlink) == 1 && (projection || info.Mode().Perm()&0o077 == 0)
}

func safeKubernetesOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || uint64(stat.Uid) == uint64(os.Getuid()))
}

func safeKubernetesDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode().Perm()&0o022 == 0 && safeKubernetesOwner(info)
}

func inspectKubernetesDirectoryChain(root, target string, lstat func(string) (os.FileInfo, error)) ([]kubernetesDirectoryIdentity, error) {
	if lstat == nil || !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return nil, errors.New("Kubernetes credential directory chain is invalid")
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return nil, errors.New("Kubernetes credential directory escapes its volume")
	}
	paths := []string{root}
	if relative != "." {
		current := root
		for _, component := range strings.Split(relative, string(os.PathSeparator)) {
			if component == "" || component == "." || component == ".." {
				return nil, errors.New("Kubernetes credential directory chain is invalid")
			}
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}
	identities := make([]kubernetesDirectoryIdentity, 0, len(paths))
	for _, directory := range paths {
		info, err := lstat(directory)
		if err != nil || !safeKubernetesDirectory(info) {
			return nil, errors.New("Kubernetes credential directory is unsafe")
		}
		identities = append(identities, kubernetesDirectoryIdentity{path: directory, info: info})
	}
	return identities, nil
}

func stableKubernetesDirectoryChain(identities []kubernetesDirectoryIdentity, lstat func(string) (os.FileInfo, error)) bool {
	if len(identities) == 0 || lstat == nil {
		return false
	}
	for _, identity := range identities {
		current, err := lstat(identity.path)
		if err != nil || !safeKubernetesDirectory(current) || !os.SameFile(identity.info, current) {
			return false
		}
	}
	return true
}

func pathWithin(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}
