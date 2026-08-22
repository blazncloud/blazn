package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/blazncloud/blazn/internal/client"
)

var (
	dnsNamePattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	versionPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imagePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$`)
	quantityPattern = regexp.MustCompile(`^(?:[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti)?|0\.[0-9]+)$`)
	mediaPattern    = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
)

type manifestDocument struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   manifestMeta `json:"metadata"`
	Spec       manifestSpec `json:"spec"`
}
type manifestMeta struct {
	Name string `json:"name"`
}
type manifestSpec struct {
	Version          string               `json:"version"`
	Description      string               `json:"description"`
	PolicyProfile    string               `json:"policyProfile"`
	Isolation        string               `json:"isolation"`
	ExpiresInSeconds int64                `json:"expiresInSeconds"`
	NetworkProfile   string               `json:"networkProfile"`
	Variants         []manifestVariant    `json:"variants"`
	Repositories     []manifestRepository `json:"repositories,omitempty"`
	Artifacts        []manifestArtifact   `json:"artifacts,omitempty"`
}
type manifestVariant struct {
	Name             string            `json:"name"`
	Platform         string            `json:"platform"`
	Architecture     string            `json:"architecture"`
	ImageIndex       string            `json:"imageIndex"`
	ImageDigest      string            `json:"imageDigest"`
	Command          []string          `json:"command"`
	Resources        manifestResources `json:"resources"`
	PlacementProfile string            `json:"placementProfile"`
}
type manifestResources struct {
	Requests manifestResourceSet `json:"requests"`
	Limits   manifestResourceSet `json:"limits"`
}
type manifestResourceSet struct {
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
	EphemeralStorage string `json:"ephemeralStorage"`
}
type manifestRepository struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
	Writable    *bool  `json:"writable"`
}
type manifestArtifact struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	Required  *bool  `json:"required"`
}

// ValidateTemplate performs the application-owned checks required by the
// frozen schema and computes the RFC 8785 digest over spec only.
func ValidateTemplate(manifest []byte) TemplateValidation {
	result := TemplateValidation{Errors: []string{}, Warnings: []string{}}
	if err := rejectDuplicateJSONNames(manifest); err != nil {
		result.Errors = append(result.Errors, "manifest must be strict JSON: "+err.Error())
		return result
	}
	var document manifestDocument
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		result.Errors = append(result.Errors, "manifest must be strict JSON: "+err.Error())
		return result
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		result.Errors = append(result.Errors, "manifest must contain exactly one JSON document")
		return result
	}
	validateDocument(document, &result.Errors)
	if len(result.Errors) != 0 {
		return result
	}
	digest, _, err := client.CanonicalSandboxTemplateDigest(manifest)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	result.Valid = true
	result.ManifestDigest = &digest
	return result
}

func validateDocument(d manifestDocument, failures *[]string) {
	require(d.APIVersion == client.SandboxTemplateAPIVersion, "apiVersion must be blazn.dev/v1alpha1", failures)
	require(d.Kind == client.SandboxTemplateKind, "kind must be SandboxTemplate", failures)
	validName(d.Metadata.Name, "metadata.name", failures)
	require(versionPattern.MatchString(d.Spec.Version), "spec.version is invalid", failures)
	require(len(d.Spec.Description) >= 1 && len(d.Spec.Description) <= 1024, "spec.description must contain 1 to 1024 characters", failures)
	require(d.Spec.PolicyProfile == "poc-restricted-v1", "spec.policyProfile must be poc-restricted-v1", failures)
	require(d.Spec.Isolation == "approved-non-sensitive-poc", "spec.isolation must be approved-non-sensitive-poc", failures)
	require(d.Spec.ExpiresInSeconds >= 60 && d.Spec.ExpiresInSeconds <= 7200, "spec.expiresInSeconds must be between 60 and 7200", failures)
	require(d.Spec.NetworkProfile == "default-deny-v1", "spec.networkProfile must be default-deny-v1", failures)
	require(len(d.Spec.Variants) >= 1 && len(d.Spec.Variants) <= 4, "spec.variants must contain 1 to 4 entries", failures)
	architectures, variants := map[string]bool{}, map[string]bool{}
	for i, variant := range d.Spec.Variants {
		prefix := "spec.variants[" + strconv.Itoa(i) + "]"
		validName(variant.Name, prefix+".name", failures)
		require(!variants[variant.Name], prefix+".name must be unique", failures)
		variants[variant.Name] = true
		require(variant.Platform == "linux", prefix+".platform must be linux", failures)
		require(variant.Architecture == "amd64" || variant.Architecture == "arm64", prefix+".architecture must be amd64 or arm64", failures)
		require(!architectures[variant.Architecture], prefix+".architecture must be unique", failures)
		architectures[variant.Architecture] = true
		validBoundedPattern(variant.ImageIndex, imagePattern, 512, prefix+".imageIndex", failures)
		validBoundedPattern(variant.ImageDigest, imagePattern, 512, prefix+".imageDigest", failures)
		require(len(variant.Command) >= 1 && len(variant.Command) <= 32, prefix+".command must contain 1 to 32 entries", failures)
		for j, item := range variant.Command {
			require(len(item) >= 1 && len(item) <= 1024, fmt.Sprintf("%s.command[%d] must contain 1 to 1024 characters", prefix, j), failures)
		}
		validateResources(variant.Resources, prefix+".resources", failures)
		expected := map[string]string{"amd64": "poc-linux-amd64-v1", "arm64": "poc-mac-arm64-v1"}[variant.Architecture]
		require(expected != "" && variant.PlacementProfile == expected, prefix+".placementProfile does not match architecture", failures)
	}
	require(len(d.Spec.Repositories) <= 8, "spec.repositories must contain at most 8 entries", failures)
	repositories := map[string]bool{}
	for i, repository := range d.Spec.Repositories {
		prefix := "spec.repositories[" + strconv.Itoa(i) + "]"
		validName(repository.Name, prefix+".name", failures)
		require(!repositories[repository.Name], prefix+".name must be unique", failures)
		repositories[repository.Name] = true
		require(validHTTPSRepository(repository.URL), prefix+".url must be an approved HTTPS repository identity", failures)
		require(validRemotePath(repository.Destination, "/workspace/src/"), prefix+".destination must be confined beneath /workspace/src", failures)
		require(repository.Writable != nil, prefix+".writable is required", failures)
	}
	require(len(d.Spec.Artifacts) <= 32, "spec.artifacts must contain at most 32 entries", failures)
	artifacts := map[string]bool{}
	for i, artifact := range d.Spec.Artifacts {
		prefix := "spec.artifacts[" + strconv.Itoa(i) + "]"
		validName(artifact.Name, prefix+".name", failures)
		require(!artifacts[artifact.Name], prefix+".name must be unique", failures)
		artifacts[artifact.Name] = true
		require(validRemotePath(artifact.Path, "/workspace/artifacts/"), prefix+".path must be confined beneath /workspace/artifacts", failures)
		validBoundedPattern(artifact.MediaType, mediaPattern, 128, prefix+".mediaType", failures)
		require(artifact.Required != nil, prefix+".required is required", failures)
	}
}

func validateResources(r manifestResources, prefix string, failures *[]string) {
	values := map[string]string{
		prefix + ".requests.cpu": r.Requests.CPU, prefix + ".requests.memory": r.Requests.Memory, prefix + ".requests.ephemeralStorage": r.Requests.EphemeralStorage,
		prefix + ".limits.cpu": r.Limits.CPU, prefix + ".limits.memory": r.Limits.Memory, prefix + ".limits.ephemeralStorage": r.Limits.EphemeralStorage,
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		validBoundedPattern(values[key], quantityPattern, 32, key, failures)
	}
}
func validName(value, field string, failures *[]string) {
	validBoundedPattern(value, dnsNamePattern, 63, field, failures)
}
func validBoundedPattern(value string, pattern *regexp.Regexp, maximum int, field string, failures *[]string) {
	require(len(value) <= maximum && pattern.MatchString(value), field+" is invalid", failures)
}
func require(ok bool, message string, failures *[]string) {
	if !ok {
		*failures = append(*failures, message)
	}
}
func validRemotePath(value, prefix string) bool {
	if len(value) > 512 || !strings.HasPrefix(value, prefix) || strings.Contains(value, "\\") || strings.Contains(value, "//") {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, prefix), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				return false
			}
		}
	}
	return true
}
func validHTTPSRepository(value string) bool {
	if len(value) > 2048 || !strings.HasPrefix(value, "https://") || strings.ContainsAny(value, "?#@ ") {
		return false
	}
	rest := strings.TrimPrefix(value, "https://")
	slash := strings.IndexByte(rest, '/')
	return slash > 0 && slash < len(rest)-1 && !strings.Contains(rest[:slash], "..")
}
func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

func rejectDuplicateJSONNames(document []byte) error {
	type frame struct {
		object    bool
		expectKey bool
		seen      map[string]struct{}
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	stack := []frame{}
	completeValue := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				stack = append(stack, frame{object: true, expectKey: true, seen: map[string]struct{}{}})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				if len(stack) == 0 {
					return errors.New("unbalanced JSON delimiter")
				}
				stack = stack[:len(stack)-1]
				completeValue()
			}
			continue
		}
		if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey {
			name, ok := token.(string)
			if !ok {
				return errors.New("object property name is invalid")
			}
			if _, exists := stack[len(stack)-1].seen[name]; exists {
				return fmt.Errorf("duplicate object property %q", name)
			}
			stack[len(stack)-1].seen[name] = struct{}{}
			stack[len(stack)-1].expectKey = false
			continue
		}
		completeValue()
	}
}

func ReadTemplateFile(path string) ([]byte, error) {
	file, info, err := openRegularNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open template: %w", err)
	}
	defer file.Close()
	const maximum = int64(1 << 20)
	if info.Size() > maximum {
		return nil, errors.New("template exceeds 1 MiB")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	if int64(len(contents)) != info.Size() {
		return nil, errors.New("template changed while it was read")
	}
	return contents, nil
}

func TemplateName(manifest []byte) (string, error) {
	var identity struct {
		Metadata manifestMeta `json:"metadata"`
	}
	if err := json.Unmarshal(manifest, &identity); err != nil || !dnsNamePattern.MatchString(identity.Metadata.Name) {
		return "", errors.New("template metadata.name is invalid")
	}
	return identity.Metadata.Name, nil
}
