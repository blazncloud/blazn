package sandboxcontroller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/blazncloud/blazn/internal/sandbox"
)

const maxDatabaseURLBytes = 16 * 1024

const (
	defaultServiceAccountDirectory = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultKubernetesCAFile        = defaultServiceAccountDirectory + "/ca.crt"
	defaultKubernetesTokenFile     = defaultServiceAccountDirectory + "/token"
)

var kubernetesDNSNamePattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*$`)

type KubernetesConfig struct {
	BaseURL                                          string
	ClusterID                                        string
	CAFile                                           string
	TokenFile                                        string
	HelperImage                                      string
	SourceDNSCIDRs                                   []string
	SourceHostCIDRs                                  map[string][]string
	ArtifactEndpoint, ArtifactRegion, ArtifactBucket string
	ArtifactAccessKeyFile, ArtifactSecretKeyFile     string
	ArtifactCAFile                                   string
}

type RuntimeConfig struct {
	Controller  Config
	DatabaseURL string
	Kubernetes  KubernetesConfig
}

type secretFileOps struct {
	lstat     func(string) (os.FileInfo, error)
	open      func(string) (*os.File, error)
	afterRead func() error
}

func ConfigFromEnv(getenv func(string) string) (RuntimeConfig, error) {
	if getenv == nil {
		return RuntimeConfig{}, errors.New("environment reader is required")
	}
	databaseURL, err := readSecretFile(getenv("BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	config := Config{
		WorkerID:         getenv("BLAZN_SANDBOX_CONTROLLER_WORKER_ID"),
		Lease:            30 * time.Second,
		RenewEvery:       10 * time.Second,
		PollEvery:        time.Second,
		OperationTimeout: 15 * time.Minute,
		IdleDelay:        time.Second,
		RetryDelay:       10 * time.Second,
		ExpiryEvery:      30 * time.Second,
		ExpiryBatch:      25,
	}
	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"BLAZN_SANDBOX_CONTROLLER_LEASE", &config.Lease},
		{"BLAZN_SANDBOX_CONTROLLER_RENEW_EVERY", &config.RenewEvery},
		{"BLAZN_SANDBOX_CONTROLLER_POLL_EVERY", &config.PollEvery},
		{"BLAZN_SANDBOX_CONTROLLER_OPERATION_TIMEOUT", &config.OperationTimeout},
		{"BLAZN_SANDBOX_CONTROLLER_IDLE_DELAY", &config.IdleDelay},
		{"BLAZN_SANDBOX_CONTROLLER_RETRY_DELAY", &config.RetryDelay},
		{"BLAZN_SANDBOX_CONTROLLER_EXPIRY_EVERY", &config.ExpiryEvery},
	}
	for _, entry := range durations {
		if value := getenv(entry.name); value != "" {
			parsed, parseErr := time.ParseDuration(value)
			if parseErr != nil {
				return RuntimeConfig{}, fmt.Errorf("%s is invalid", entry.name)
			}
			*entry.target = parsed
		}
	}
	if value := getenv("BLAZN_SANDBOX_CONTROLLER_EXPIRY_BATCH"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return RuntimeConfig{}, errors.New("BLAZN_SANDBOX_CONTROLLER_EXPIRY_BATCH is invalid")
		}
		config.ExpiryBatch = parsed
	}
	if err := validateConfig(config); err != nil {
		return RuntimeConfig{}, err
	}
	kubernetes, err := kubernetesConfigFromEnv(getenv)
	if err != nil {
		return RuntimeConfig{}, err
	}
	return RuntimeConfig{Controller: config, DatabaseURL: databaseURL, Kubernetes: kubernetes}, nil
}

func kubernetesConfigFromEnv(getenv func(string) string) (KubernetesConfig, error) {
	clusterID := getenv("BLAZN_SANDBOX_CONTROLLER_KUBERNETES_CLUSTER_ID")
	if clusterID == "" || len(clusterID) > 128 || strings.TrimSpace(clusterID) != clusterID {
		return KubernetesConfig{}, errors.New("sandbox controller Kubernetes cluster ID is invalid")
	}
	host := getenv("BLAZN_SANDBOX_CONTROLLER_KUBERNETES_HOST")
	if host == "" {
		host = getenv("KUBERNETES_SERVICE_HOST")
	}
	port := getenv("BLAZN_SANDBOX_CONTROLLER_KUBERNETES_PORT")
	if port == "" {
		port = getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	}
	if host == "" || port == "" || !validKubernetesHost(host) || !validKubernetesPort(port) {
		return KubernetesConfig{}, errors.New("sandbox controller Kubernetes API endpoint is invalid")
	}
	caFile := getenv("BLAZN_SANDBOX_CONTROLLER_KUBERNETES_CA_FILE")
	if caFile == "" {
		caFile = defaultKubernetesCAFile
	}
	tokenFile := getenv("BLAZN_SANDBOX_CONTROLLER_KUBERNETES_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = defaultKubernetesTokenFile
	}
	if !validAbsoluteFilePath(caFile) || !validAbsoluteFilePath(tokenFile) || caFile == tokenFile {
		return KubernetesConfig{}, errors.New("sandbox controller Kubernetes credential paths are invalid")
	}
	helperImage := getenv("BLAZN_SANDBOX_IO_IMAGE")
	if !sandbox.IsImmutableOCIReference(helperImage) {
		return KubernetesConfig{}, errors.New("sandbox I/O helper image is invalid")
	}
	dnsRaw, hostsRaw := getenv("BLAZN_SANDBOX_SOURCE_DNS_CIDRS"), getenv("BLAZN_SANDBOX_SOURCE_HOST_CIDRS_JSON")
	var dnsCIDRs []string
	var hostCIDRs map[string][]string
	if dnsRaw != "" || hostsRaw != "" {
		if dnsRaw == "" || hostsRaw == "" {
			return KubernetesConfig{}, errors.New("sandbox source egress configuration is incomplete")
		}
		for _, value := range strings.Split(dnsRaw, ",") {
			if value == "" || strings.TrimSpace(value) != value {
				return KubernetesConfig{}, errors.New("sandbox source DNS CIDRs are invalid")
			}
			dnsCIDRs = append(dnsCIDRs, value)
		}
		decoder := json.NewDecoder(strings.NewReader(hostsRaw))
		if err := decoder.Decode(&hostCIDRs); err != nil || decoder.Decode(&struct{}{}) != io.EOF || len(hostCIDRs) == 0 {
			return KubernetesConfig{}, errors.New("sandbox source host CIDRs are invalid")
		}
		if _, err := canonicalCIDRs(dnsCIDRs); err != nil {
			return KubernetesConfig{}, errors.New("sandbox source DNS CIDRs are invalid")
		}
		for host, cidrs := range hostCIDRs {
			if !validSourceHostname(host) || host != strings.ToLower(host) {
				return KubernetesConfig{}, errors.New("sandbox source host is invalid")
			}
			if _, err := canonicalCIDRs(cidrs); err != nil {
				return KubernetesConfig{}, errors.New("sandbox source host CIDRs are invalid")
			}
		}
	}
	artifactValues := []string{getenv("BLAZN_SANDBOX_ARTIFACT_ENDPOINT"), getenv("BLAZN_SANDBOX_ARTIFACT_REGION"),
		getenv("BLAZN_SANDBOX_ARTIFACT_BUCKET"), getenv("BLAZN_SANDBOX_ARTIFACT_ACCESS_KEY_FILE"), getenv("BLAZN_SANDBOX_ARTIFACT_SECRET_KEY_FILE")}
	artifactConfigured := false
	for _, value := range artifactValues {
		artifactConfigured = artifactConfigured || value != ""
	}
	if artifactConfigured {
		for _, value := range artifactValues {
			if value == "" {
				return KubernetesConfig{}, errors.New("sandbox artifact object configuration is incomplete")
			}
		}
		if !validAbsoluteFilePath(artifactValues[3]) || !validAbsoluteFilePath(artifactValues[4]) || artifactValues[3] == artifactValues[4] {
			return KubernetesConfig{}, errors.New("sandbox artifact credential paths are invalid")
		}
	}
	artifactCAFile := getenv("BLAZN_SANDBOX_ARTIFACT_CA_FILE")
	if artifactCAFile != "" {
		if !artifactConfigured {
			return KubernetesConfig{}, errors.New("sandbox artifact object configuration is incomplete")
		}
		if !validAbsoluteFilePath(artifactCAFile) || artifactCAFile == artifactValues[3] || artifactCAFile == artifactValues[4] {
			return KubernetesConfig{}, errors.New("sandbox artifact CA path is invalid")
		}
	}
	endpoint := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}
	return KubernetesConfig{BaseURL: endpoint.String(), ClusterID: clusterID, CAFile: caFile, TokenFile: tokenFile, HelperImage: helperImage,
		SourceDNSCIDRs: dnsCIDRs, SourceHostCIDRs: hostCIDRs,
		ArtifactEndpoint: artifactValues[0], ArtifactRegion: artifactValues[1], ArtifactBucket: artifactValues[2],
		ArtifactAccessKeyFile: artifactValues[3], ArtifactSecretKeyFile: artifactValues[4], ArtifactCAFile: artifactCAFile}, nil
}

func validKubernetesHost(value string) bool {
	if value == "" || value != strings.ToLower(value) || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "[]/%\x00") || len(value) > 253 {
		return false
	}
	if parsed := net.ParseIP(value); parsed != nil {
		return parsed.String() == value && !parsed.IsUnspecified() && !parsed.IsMulticast()
	}
	return !strings.ContainsRune(value, ':') && kubernetesDNSNamePattern.MatchString(value) && value != "localhost"
}

func validKubernetesPort(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	return err == nil && parsed > 0
}

func validAbsoluteFilePath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsRune(value, '\x00')
}

func readSecretFile(path string) (string, error) {
	return readSecretFileWithOps(path, secretFileOps{
		lstat: os.Lstat,
		open: func(name string) (*os.File, error) {
			return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		},
	})
}

func readSecretFileWithOps(path string, ops secretFileOps) (string, error) {
	if path == "" {
		return "", errors.New("BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE is required")
	}
	if ops.lstat == nil || ops.open == nil {
		return "", errors.New("sandbox controller database URL file cannot be inspected")
	}
	info, err := ops.lstat(path)
	if err != nil {
		return "", errors.New("sandbox controller database URL file cannot be inspected")
	}
	if info.Mode()&os.ModeSymlink != 0 || !secureSecretInfo(info, os.Getuid()) {
		return "", errors.New("sandbox controller database URL file is unsafe")
	}
	file, err := ops.open(path)
	if err != nil {
		return "", errors.New("sandbox controller database URL file cannot be read")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !secureSecretInfo(opened, os.Getuid()) {
		return "", errors.New("sandbox controller database URL file changed during inspection")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxDatabaseURLBytes+1))
	if err != nil || len(contents) > maxDatabaseURLBytes {
		return "", errors.New("sandbox controller database URL file cannot be read")
	}
	if ops.afterRead != nil {
		if err := ops.afterRead(); err != nil {
			return "", errors.New("sandbox controller database URL file changed during read")
		}
	}
	finalFD, fdErr := file.Stat()
	finalPath, pathErr := ops.lstat(path)
	if fdErr != nil || pathErr != nil || finalPath.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(info, finalFD) || !os.SameFile(finalFD, finalPath) ||
		!secureSecretInfo(finalFD, os.Getuid()) || !secureSecretInfo(finalPath, os.Getuid()) {
		return "", errors.New("sandbox controller database URL file changed during read")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", errors.New("sandbox controller database URL file is invalid")
	}
	return value, nil
}

func secureSecretInfo(info os.FileInfo, expectedUID int) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxDatabaseURLBytes || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(expectedUID) && uint64(stat.Nlink) == 1
}
