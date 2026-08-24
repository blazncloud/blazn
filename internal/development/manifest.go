package development

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gowebpki/jcs"
)

const maxManifestBytes = 1024 * 1024

type Validation struct {
	Valid          bool     `json:"valid"`
	ManifestDigest *string  `json:"manifestDigest"`
	Errors         []string `json:"errors"`
	Warnings       []string `json:"warnings"`
}

type Manifest struct {
	SchemaVersion     string                    `json:"schemaVersion"`
	ProjectID         string                    `json:"projectId"`
	Repository        Repository                `json:"repository"`
	Template          Template                  `json:"template"`
	PublicationTarget PublicationTarget         `json:"publicationTarget"`
	Platforms         []string                  `json:"platforms"`
	Build             Build                     `json:"build"`
	DependencyLocks   map[string]string         `json:"dependencyLocks"`
	Tests             map[string]TestDefinition `json:"tests"`
	Policy            Policy                    `json:"policy"`
}
type Repository struct {
	URL string `json:"url"`
}
type Template struct {
	VersionID string `json:"versionId"`
	Digest    string `json:"digest"`
}
type PublicationTarget struct {
	TemplateID string `json:"templateId"`
}
type Build struct {
	Context, Dockerfile, RegistryRepository string
}
type TestDefinition struct {
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}
type Policy struct{ BuilderProfile, NetworkProfile, ResourceProfile, PublicationPolicy string }

var (
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	policyPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	registryPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	secretFlagPattern = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(?:api[_-]?key|token|secret|password|credential|authorization)[a-z0-9_-]*(?:=|$)`)
	secretAssign      = regexp.MustCompile(`(?i)^[A-Z0-9_]*(?:API_KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|AUTHORIZATION)[A-Z0-9_]*=`)
)

func ReadFile(name string) ([]byte, error) {
	before, err := os.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o022 != 0 || before.Size() > maxManifestBytes {
		return nil, errors.New("development manifest file is unsafe")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("development manifest cannot be opened safely")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return nil, errors.New("development manifest cannot be read safely")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return nil, errors.New("development manifest changed while reading")
	}
	return data, nil
}

func Validate(data []byte) (Validation, *Manifest) {
	result := Validation{Errors: []string{}, Warnings: []string{}}
	if len(data) == 0 || len(data) > maxManifestBytes {
		result.Errors = append(result.Errors, "manifest size is invalid")
		return result, nil
	}
	if err := validateJSONTopology(data); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		result.Errors = append(result.Errors, "manifest structure is invalid")
		return result, nil
	}
	result.Errors = append(result.Errors, validateManifest(manifest)...)
	sort.Strings(result.Errors)
	if len(result.Errors) != 0 {
		return result, &manifest
	}
	canonical, err := jcs.Transform(data)
	if err != nil {
		result.Errors = append(result.Errors, "manifest cannot be canonicalized")
		return result, &manifest
	}
	digest := sha256.Sum256(canonical)
	value := "sha256:" + hex.EncodeToString(digest[:])
	result.Valid, result.ManifestDigest = true, &value
	return result, &manifest
}

func validateManifest(value Manifest) []string {
	errorsFound := []string{}
	if value.SchemaVersion != "blazn.dev/project/v1alpha1" {
		errorsFound = append(errorsFound, "schemaVersion is invalid")
	}
	for name, id := range map[string]string{"projectId": value.ProjectID, "template.versionId": value.Template.VersionID, "publicationTarget.templateId": value.PublicationTarget.TemplateID} {
		if !uuidPattern.MatchString(id) {
			errorsFound = append(errorsFound, name+" is invalid")
		}
	}
	if !digestPattern.MatchString(value.Template.Digest) {
		errorsFound = append(errorsFound, "template.digest is invalid")
	}
	if !validRepositoryURL(value.Repository.URL) {
		errorsFound = append(errorsFound, "repository.url is invalid")
	}
	if len(value.Platforms) != 2 || value.Platforms[0] != "linux/amd64" || value.Platforms[1] != "linux/arm64" {
		errorsFound = append(errorsFound, "platforms must be exact and ordered")
	}
	for name, item := range map[string]string{"build.context": value.Build.Context, "build.dockerfile": value.Build.Dockerfile} {
		if !repositoryPath(item) {
			errorsFound = append(errorsFound, name+" is invalid")
		}
	}
	if len(value.Build.RegistryRepository) > 512 || !registryPattern.MatchString(value.Build.RegistryRepository) {
		errorsFound = append(errorsFound, "build.registryRepository is invalid")
	}
	if len(value.DependencyLocks) < 1 || len(value.DependencyLocks) > 32 {
		errorsFound = append(errorsFound, "dependencyLocks count is invalid")
	}
	for name, digest := range value.DependencyLocks {
		if !repositoryPath(name) || !digestPattern.MatchString(digest) {
			errorsFound = append(errorsFound, "dependency lock is invalid")
			break
		}
	}
	if len(value.Tests) < 1 || len(value.Tests) > 32 {
		errorsFound = append(errorsFound, "tests count is invalid")
	}
	for name, test := range value.Tests {
		if !policyPattern.MatchString(name) || len(test.Argv) < 1 || len(test.Argv) > 64 || test.TimeoutSeconds < 1 || test.TimeoutSeconds > 1800 {
			errorsFound = append(errorsFound, "test "+name+" is invalid")
			continue
		}
		if unsafeExecutable(test.Argv[0]) {
			errorsFound = append(errorsFound, "test "+name+" invokes a shell or env launcher")
			continue
		}
		for _, argument := range test.Argv {
			if len(argument) < 1 || len(argument) > 1024 || unsafeArgument(argument) {
				errorsFound = append(errorsFound, "test "+name+" contains credential-like material")
				break
			}
		}
	}
	for name, profile := range map[string]string{"builderProfile": value.Policy.BuilderProfile, "networkProfile": value.Policy.NetworkProfile, "resourceProfile": value.Policy.ResourceProfile, "publicationPolicy": value.Policy.PublicationPolicy} {
		if !policyPattern.MatchString(profile) {
			errorsFound = append(errorsFound, "policy."+name+" is invalid")
		}
	}
	return errorsFound
}

func validRepositoryURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	port := parsed.Port()
	if port != "" && port != "443" {
		return false
	}
	// Repository authorities are deliberately limited to an exact DNS hostname
	// with an optional HTTPS default port. This rejects empty, ambiguous, and
	// user-info-like authorities rather than letting url.Parse repair them.
	expectedAuthority := parsed.Hostname()
	if port != "" {
		expectedAuthority += ":" + port
	}
	if parsed.Host != expectedAuthority || !validRepositoryHostname(parsed.Hostname()) {
		return false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func validRepositoryHostname(value string) bool {
	if len(value) < 1 || len(value) > 253 || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < '0' || character > '9') && (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') {
				return false
			}
		}
	}
	return true
}

func repositoryPath(value string) bool {
	if len(value) < 1 || len(value) > 512 || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return true
}
func unsafeExecutable(value string) bool {
	parts := strings.Split(filepath.ToSlash(value), "/")
	name := strings.ToLower(parts[len(parts)-1])
	switch name {
	case "sh", "bash", "dash", "zsh", "fish", "ksh", "csh", "tcsh", "env", "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh":
		return true
	}
	return false
}
func unsafeArgument(value string) bool {
	decoded := value
	for index := 0; index < 4; index++ {
		next, err := url.QueryUnescape(decoded)
		if err != nil {
			return true
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	lower := strings.ToLower(decoded)
	return secretFlagPattern.MatchString(decoded) || secretAssign.MatchString(decoded) || containsCredentialString(decoded) || strings.Contains(lower, "bearer ")
}

func validateJSONTopology(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("manifest contains trailing data")
	}
	return nil
}
func consumeJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("manifest is not valid JSON")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return errors.New("manifest object is invalid")
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return fmt.Errorf("manifest contains duplicate or invalid key %q", key)
			}
			seen[key] = true
			if err := consumeJSON(decoder); err != nil {
				return err
			}
		}
		end, _ := decoder.Token()
		if end != json.Delim('}') {
			return errors.New("manifest object is invalid")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSON(decoder); err != nil {
				return err
			}
		}
		end, _ := decoder.Token()
		if end != json.Delim(']') {
			return errors.New("manifest array is invalid")
		}
	default:
		return errors.New("manifest delimiter is invalid")
	}
	return nil
}
