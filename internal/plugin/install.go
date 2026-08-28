package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Installer interface {
	Install(context.Context, Definition, string, *Store) (Receipt, error)
}

type LocalInstaller struct {
	Manifest Manifest
	Binary   string
}

func (i LocalInstaller) Install(_ context.Context, definition Definition, coreVersion string, store *Store) (Receipt, error) {
	if err := i.Manifest.Validate(); err != nil {
		return Receipt{}, err
	}
	if err := Compatible(coreVersion, i.Manifest); err != nil {
		return Receipt{}, err
	}
	return store.Activate(definition, i.Manifest, i.Binary)
}

type commandRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) ([]byte, error)
}

type systemCommandRunner struct{}

func (systemCommandRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (systemCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %s", name, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type GitHubInstaller struct{ runner commandRunner }

func NewGitHubInstaller() *GitHubInstaller { return &GitHubInstaller{runner: systemCommandRunner{}} }

func (i *GitHubInstaller) Install(ctx context.Context, definition Definition, coreVersion string, store *Store) (Receipt, error) {
	gh, err := i.runner.LookPath("gh")
	if err != nil {
		return Receipt{}, errors.New("GitHub CLI is required; install gh and run 'gh auth login'")
	}
	if _, err := i.runner.Run(ctx, gh, "auth", "status"); err != nil {
		return Receipt{}, errors.New("GitHub authentication is required; run 'gh auth login'")
	}
	requestedTag := strings.TrimSpace(os.Getenv("BLAZN_PLUGIN_VERSION"))
	releaseArgs, err := releaseViewArgs(definition, requestedTag)
	if err != nil {
		return Receipt{}, err
	}
	tagOutput, err := i.runner.Run(ctx, gh, releaseArgs...)
	if err != nil {
		return Receipt{}, fmt.Errorf("find latest signed plugin release: %w", err)
	}
	var release struct {
		TagName string `json:"tagName"`
	}
	if err := json.Unmarshal(tagOutput, &release); err != nil || release.TagName == "" {
		return Receipt{}, errors.New("GitHub returned invalid plugin release metadata")
	}
	if requestedTag != "" && release.TagName != requestedTag {
		return Receipt{}, errors.New("GitHub returned a different plugin release than requested")
	}
	temp, err := os.MkdirTemp("", "blazn-plugin-install-*")
	if err != nil {
		return Receipt{}, err
	}
	defer os.RemoveAll(temp)
	assetVersion := strings.TrimPrefix(release.TagName, "v")
	archiveName := fmt.Sprintf("%s_%s_%s_%s.tar.gz", definition.Executable, assetVersion, runtime.GOOS, runtime.GOARCH)
	assets := []string{"plugin.json", "SHA256SUMS", "SHA256SUMS.sig", archiveName}
	args := []string{"release", "download", release.TagName, "--repo", definition.Repository, "--dir", temp}
	for _, asset := range assets {
		args = append(args, "--pattern", asset)
	}
	if _, err := i.runner.Run(ctx, gh, args...); err != nil {
		return Receipt{}, fmt.Errorf("download signed plugin release: %w", err)
	}
	for _, asset := range assets {
		if err := validateOwnedRegular(filepath.Join(temp, asset), 0o022); err != nil {
			return Receipt{}, fmt.Errorf("downloaded plugin asset %s is unsafe: %w", asset, err)
		}
	}
	if err := verifySignature(ctx, i.runner, definition, temp); err != nil {
		return Receipt{}, err
	}
	checksums, err := parseChecksums(filepath.Join(temp, "SHA256SUMS"))
	if err != nil {
		return Receipt{}, err
	}
	for _, asset := range []string{"plugin.json", archiveName} {
		expected, ok := checksums[asset]
		if !ok {
			return Receipt{}, fmt.Errorf("signed checksum manifest omits %s", asset)
		}
		actual, err := fileSHA256(filepath.Join(temp, asset))
		if err != nil || actual != expected {
			return Receipt{}, fmt.Errorf("checksum mismatch for %s", asset)
		}
	}
	manifestFile, err := os.Open(filepath.Join(temp, "plugin.json"))
	if err != nil {
		return Receipt{}, err
	}
	manifest, decodeErr := DecodeManifest(manifestFile)
	closeErr := manifestFile.Close()
	if decodeErr != nil {
		return Receipt{}, decodeErr
	}
	if closeErr != nil {
		return Receipt{}, closeErr
	}
	if manifest.Version != release.TagName && manifest.Version != assetVersion {
		return Receipt{}, errors.New("plugin manifest version does not match release tag")
	}
	if err := Compatible(coreVersion, manifest); err != nil {
		return Receipt{}, err
	}
	binary := filepath.Join(temp, definition.Executable)
	if err := extractSingleBinary(filepath.Join(temp, archiveName), definition.Executable, binary); err != nil {
		return Receipt{}, err
	}
	if err := smokeTestCandidate(ctx, binary, manifest); err != nil {
		return Receipt{}, err
	}
	return store.Activate(definition, manifest, binary)
}

func releaseViewArgs(definition Definition, requestedTag string) ([]string, error) {
	args := []string{"release", "view"}
	if requestedTag != "" {
		if _, err := parseVersion(requestedTag); err != nil {
			return nil, errors.New("BLAZN_PLUGIN_VERSION must be a semantic release tag")
		}
		args = append(args, requestedTag)
	}
	return append(args, "--repo", definition.Repository, "--json", "tagName"), nil
}

func smokeTestCandidate(ctx context.Context, binary string, expected Manifest) error {
	// A signed executable is still untrusted until this handshake succeeds. Do
	// not let a candidate that never exits wedge installation indefinitely.
	handshakeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(handshakeCtx, binary, "__plugin", "describe", "--json")
	command.WaitDelay = 2 * time.Second
	command.Env = pluginEnvironment(os.Environ())
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("plugin candidate handshake failed: %w", err)
	}
	actual, err := DecodeManifest(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return fmt.Errorf("plugin candidate returned an invalid manifest: %w", err)
	}
	if actual.SchemaVersion != expected.SchemaVersion || actual.Name != expected.Name ||
		actual.Version != expected.Version || actual.ProtocolVersion != expected.ProtocolVersion ||
		actual.MinimumCoreVersion != expected.MinimumCoreVersion || actual.Executable != expected.Executable ||
		strings.Join(actual.Commands, "\x00") != strings.Join(expected.Commands, "\x00") ||
		strings.Join(actual.Capabilities, "\x00") != strings.Join(expected.Capabilities, "\x00") {
		return errors.New("plugin candidate manifest does not match signed plugin.json")
	}
	return nil
}

func verifySignature(ctx context.Context, runner commandRunner, definition Definition, directory string) error {
	sshKeygen, err := runner.LookPath("ssh-keygen")
	if err != nil {
		return errors.New("ssh-keygen with signed-manifest support is required")
	}
	allowed := filepath.Join(directory, "allowed_signers")
	if err := os.WriteFile(allowed, []byte(definition.AllowedSigner+"\n"), 0o600); err != nil {
		return err
	}
	manifest, err := os.Open(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return err
	}
	defer manifest.Close()
	command := exec.CommandContext(ctx, sshKeygen, "-Y", "verify", "-f", allowed, "-I", definition.SigningIdentity, "-n", definition.SignatureNamespace, "-s", filepath.Join(directory, "SHA256SUMS.sig"))
	command.Stdin = manifest
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("plugin checksum signature verification failed: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func parseChecksums(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, maxManifestSize+1))
	if err != nil || len(encoded) > maxManifestSize {
		return nil, errors.New("signed checksum manifest is too large")
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, errors.New("signed checksum manifest is invalid")
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" || filepath.Base(name) != name || result[name] != "" {
			return nil, errors.New("signed checksum manifest contains an invalid or duplicate asset")
		}
		for _, char := range fields[0] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
				return nil, errors.New("signed checksum manifest contains an invalid digest")
			}
		}
		result[name] = strings.ToLower(fields[0])
	}
	return result, nil
}

func extractSingleBinary(archivePath, expected, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return errors.New("plugin archive is not gzip compressed")
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil {
		return errors.New("plugin archive is empty")
	}
	if header.Name != expected || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > 512*1024*1024 {
		return errors.New("plugin archive must contain only the expected regular executable")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, tarReader, header.Size); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if _, err := tarReader.Next(); err != io.EOF {
		return errors.New("plugin archive contains unexpected additional entries")
	}
	return nil
}
