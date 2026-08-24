package sandboxcontroller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxS3CredentialBytes = 4 << 10
	maxS3ResponseBytes   = 64 << 10
	maxArtifactBytes     = 8 << 20
)

var (
	s3BucketPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{1,61}[a-z0-9])$`)
	s3RegionPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	s3AccessPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
	artifactNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type S3ArtifactStoreConfig struct {
	Endpoint      string
	Region        string
	Bucket        string
	AccessKeyFile string
	SecretKeyFile string
	RootCAs       *x509.CertPool
	Now           func() time.Time
}

type ArtifactObjectSpec struct {
	Key, WorkspaceID, SandboxID, Name, MediaType, Digest string
	Size                                                 int64
}

type ArtifactObjectHead struct {
	Size, DigestSize                                int64
	MediaType, Digest, WorkspaceID, SandboxID, Name string
}

type S3ArtifactStore struct {
	endpoint      *url.URL
	region        string
	bucket        string
	accessKeyFile string
	secretKeyFile string
	client        *http.Client
	now           func() time.Time
}

func NewS3ArtifactStore(config S3ArtifactStoreConfig) (*S3ArtifactStore, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Host == "" || endpoint.Hostname() == "" ||
		endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		!validKubernetesHost(endpoint.Hostname()) || !validKubernetesPort(endpoint.Port()) ||
		endpoint.Host != net.JoinHostPort(endpoint.Hostname(), endpoint.Port()) || !s3BucketPattern.MatchString(config.Bucket) ||
		strings.Contains(config.Bucket, "..") || net.ParseIP(config.Bucket) != nil || !s3RegionPattern.MatchString(config.Region) ||
		!validAbsoluteFilePath(config.AccessKeyFile) || !validAbsoluteFilePath(config.SecretKeyFile) || config.AccessKeyFile == config.SecretKeyFile || config.RootCAs == nil {
		return nil, errors.New("artifact object store configuration is invalid")
	}
	if _, _, err := readS3Credentials(config.AccessKeyFile, config.SecretKeyFile); err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	transport := &http.Transport{Proxy: nil,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second, ExpectContinueTimeout: time.Second,
		MaxResponseHeaderBytes: maxKubernetesHeaderBytes,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: config.RootCAs, ServerName: endpoint.Hostname()}}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &S3ArtifactStore{endpoint: endpoint, region: config.Region, bucket: config.Bucket,
		accessKeyFile: config.AccessKeyFile, secretKeyFile: config.SecretKeyFile, client: client, now: now}, nil
}

func ArtifactObjectKey(workspaceID, sandboxID, name string) (string, error) {
	if !canonicalUUID(workspaceID) || !canonicalUUID(sandboxID) || !artifactNamePattern.MatchString(name) {
		return "", errors.New("artifact object identity is invalid")
	}
	return "workspaces/" + workspaceID + "/sandboxes/" + sandboxID + "/artifacts/" + name, nil
}

func (s *S3ArtifactStore) Put(ctx context.Context, spec ArtifactObjectSpec, body []byte) (bool, error) {
	if err := validateArtifactObjectSpec(spec); err != nil || int64(len(body)) != spec.Size {
		return false, errors.New("artifact object is invalid")
	}
	digest := sha256.Sum256(body)
	if spec.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
		return false, errors.New("artifact object digest differs")
	}
	request, err := s.request(ctx, http.MethodPut, spec, body)
	if err != nil {
		return false, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return false, errors.New("artifact object PUT failed")
	}
	defer response.Body.Close()
	if err := drainS3Response(response); err != nil {
		return false, err
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return true, nil
	case http.StatusPreconditionFailed:
		return false, nil
	default:
		return false, fmt.Errorf("artifact object PUT returned HTTP %d", response.StatusCode)
	}
}

func (s *S3ArtifactStore) Head(ctx context.Context, spec ArtifactObjectSpec) (ArtifactObjectHead, bool, error) {
	if err := validateArtifactObjectSpec(spec); err != nil {
		return ArtifactObjectHead{}, false, err
	}
	request, err := s.request(ctx, http.MethodHead, spec, nil)
	if err != nil {
		return ArtifactObjectHead{}, false, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return ArtifactObjectHead{}, false, errors.New("artifact object HEAD failed")
	}
	defer response.Body.Close()
	if err := drainS3Response(response); err != nil {
		return ArtifactObjectHead{}, false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return ArtifactObjectHead{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return ArtifactObjectHead{}, false, fmt.Errorf("artifact object HEAD returned HTTP %d", response.StatusCode)
	}
	size, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 || size > maxArtifactBytes {
		return ArtifactObjectHead{}, false, errors.New("artifact object HEAD size is invalid")
	}
	head := ArtifactObjectHead{Size: size, DigestSize: size, MediaType: response.Header.Get("Content-Type"),
		Digest: response.Header.Get("X-Amz-Meta-Blazn-Digest"), WorkspaceID: response.Header.Get("X-Amz-Meta-Blazn-Workspace"),
		SandboxID: response.Header.Get("X-Amz-Meta-Blazn-Sandbox"), Name: response.Header.Get("X-Amz-Meta-Blazn-Artifact")}
	return head, true, nil
}

func (s *S3ArtifactStore) request(ctx context.Context, method string, spec ArtifactObjectSpec, body []byte) (*http.Request, error) {
	access, secret, err := readS3Credentials(s.accessKeyFile, s.secretKeyFile)
	if err != nil {
		return nil, err
	}
	objectURL := *s.endpoint
	objectURL.Path = "/" + s.bucket + "/" + escapeS3Key(spec.Key)
	payloadHash := sha256.Sum256(body)
	payloadDigest := hex.EncodeToString(payloadHash[:])
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, objectURL.String(), reader)
	if err != nil {
		return nil, errors.New("artifact object request cannot be constructed")
	}
	request.Host = s.endpoint.Host
	request.Header.Set("X-Amz-Content-Sha256", payloadDigest)
	now := s.now().UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	request.Header.Set("X-Amz-Date", amzDate)
	if method == http.MethodPut {
		request.ContentLength = spec.Size
		request.Header.Set("Content-Type", spec.MediaType)
		request.Header.Set("If-None-Match", "*")
		request.Header.Set("X-Amz-Meta-Blazn-Digest", spec.Digest)
		request.Header.Set("X-Amz-Meta-Blazn-Workspace", spec.WorkspaceID)
		request.Header.Set("X-Amz-Meta-Blazn-Sandbox", spec.SandboxID)
		request.Header.Set("X-Amz-Meta-Blazn-Artifact", spec.Name)
	}
	canonicalHeaders, signedHeaders := canonicalS3Headers(request)
	canonicalRequest := method + "\n" + objectURL.EscapedPath() + "\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadDigest
	scope := date + "/" + s.region + "/s3/aws4_request"
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	dateKey := hmacSHA256([]byte("AWS4"+secret), date)
	regionKey := hmacSHA256(dateKey, s.region)
	serviceKey := hmacSHA256(regionKey, "s3")
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+access+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	request.Header.Set("User-Agent", "blazn-sandbox-controller-artifact/v1")
	return request, nil
}

func validateArtifactObjectSpec(spec ArtifactObjectSpec) error {
	wanted, err := ArtifactObjectKey(spec.WorkspaceID, spec.SandboxID, spec.Name)
	if err != nil || spec.Key != wanted || spec.Size < 0 || spec.Size > maxArtifactBytes ||
		!sha256Pattern.MatchString(spec.Digest) || !mediaTypePattern.MatchString(spec.MediaType) {
		return errors.New("artifact object specification is invalid")
	}
	return nil
}

var mediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$`)

func canonicalUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return value[14] >= '1' && value[14] <= '8' && strings.ContainsRune("89ab", rune(value[19]))
}

func escapeS3Key(key string) string {
	parts := strings.Split(key, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func canonicalS3Headers(request *http.Request) (string, string) {
	headers := map[string]string{"host": request.Host}
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "user-agent" {
			continue
		}
		headers[lower] = strings.Join(values, ",")
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteByte(':')
		canonical.WriteString(strings.TrimSpace(headers[name]))
		canonical.WriteByte('\n')
	}
	return canonical.String(), strings.Join(names, ";")
}

func hmacSHA256(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}

func readS3Credentials(accessPath, secretPath string) (string, string, error) {
	read := func(path string) ([]byte, error) {
		return readStableRegularFile(path, maxS3CredentialBytes, false, kubernetesFileOps{lstat: os.Lstat,
			open: func(name string) (*os.File, error) {
				return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			}})
	}
	accessBytes, accessErr := read(accessPath)
	secretBytes, secretErr := read(secretPath)
	access, secret := strings.TrimSpace(string(accessBytes)), strings.TrimSpace(string(secretBytes))
	if accessErr != nil || secretErr != nil || !s3AccessPattern.MatchString(access) || len(secret) < 16 || len(secret) > 256 || strings.ContainsAny(secret, "\x00\r\n\t ") {
		return "", "", errors.New("artifact object credentials are unsafe")
	}
	return access, secret, nil
}

func drainS3Response(response *http.Response) error {
	if response.ContentLength > maxS3ResponseBytes && (response.Request == nil || response.Request.Method != http.MethodHead) {
		return errors.New("artifact object response exceeds the configured limit")
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxS3ResponseBytes+1))
	if err != nil || len(contents) > maxS3ResponseBytes {
		return errors.New("artifact object response is invalid")
	}
	return nil
}
