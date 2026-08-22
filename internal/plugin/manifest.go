package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const maxManifestSize = 64 * 1024

var safeIdentifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var semanticVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

type Manifest struct {
	SchemaVersion      int      `json:"schemaVersion"`
	Name               string   `json:"name"`
	Version            string   `json:"version"`
	ProtocolVersion    int      `json:"protocolVersion"`
	MinimumCoreVersion string   `json:"minimumCoreVersion"`
	Executable         string   `json:"executable"`
	Commands           []string `json:"commands"`
	Capabilities       []string `json:"capabilities,omitempty"`
}

func DecodeManifest(reader io.Reader) (Manifest, error) {
	encoded, err := io.ReadAll(io.LimitReader(reader, maxManifestSize+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(encoded) > maxManifestSize {
		return Manifest{}, errors.New("plugin manifest exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, errors.New("plugin manifest contains trailing data")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("unsupported plugin manifest schema version %d", m.SchemaVersion)
	}
	if !safeIdentifier.MatchString(m.Name) {
		return errors.New("plugin name is invalid")
	}
	if _, err := parseVersion(m.Version); err != nil {
		return fmt.Errorf("plugin version is invalid: %w", err)
	}
	if m.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("unsupported plugin protocol version %d", m.ProtocolVersion)
	}
	if _, err := parseVersion(m.MinimumCoreVersion); err != nil {
		return fmt.Errorf("minimum core version is invalid: %w", err)
	}
	if m.Executable == "" || strings.ContainsAny(m.Executable, `/\\`) || m.Executable == "." || m.Executable == ".." {
		return errors.New("plugin executable is invalid")
	}
	if len(m.Commands) == 0 || len(m.Commands) > 64 {
		return errors.New("plugin commands must contain between 1 and 64 entries")
	}
	seen := map[string]bool{}
	for _, command := range m.Commands {
		if !safeIdentifier.MatchString(command) || seen[command] {
			return errors.New("plugin commands contain an invalid or duplicate entry")
		}
		seen[command] = true
	}
	return nil
}

func Compatible(coreVersion string, manifest Manifest) error {
	core, err := parseVersion(coreVersion)
	if err != nil {
		return fmt.Errorf("core version %q cannot be checked for plugin compatibility", coreVersion)
	}
	minimum, _ := parseVersion(manifest.MinimumCoreVersion)
	if compareVersion(core, minimum) < 0 {
		return fmt.Errorf("plugin requires blazn %s or newer", manifest.MinimumCoreVersion)
	}
	return nil
}

type version struct {
	major      int
	minor      int
	patch      int
	prerelease []versionIdentifier
}

type versionIdentifier struct {
	value   string
	number  int
	numeric bool
}

func parseVersion(value string) (version, error) {
	if !semanticVersion.MatchString(value) || strings.Contains(value, "..") {
		return version{}, errors.New("expected semantic version")
	}
	value = strings.TrimPrefix(value, "v")
	value = strings.SplitN(value, "+", 2)[0]
	core := value
	prerelease := ""
	if separator := strings.IndexByte(value, '-'); separator >= 0 {
		core, prerelease = value[:separator], value[separator+1:]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, errors.New("expected semantic version")
	}
	numbers := [3]int{}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version{}, errors.New("expected semantic version")
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return version{}, errors.New("expected semantic version")
		}
		numbers[i] = number
	}
	result := version{major: numbers[0], minor: numbers[1], patch: numbers[2]}
	if prerelease != "" {
		for _, identifier := range strings.Split(prerelease, ".") {
			if identifier == "" {
				return version{}, errors.New("expected semantic version")
			}
			parsed := versionIdentifier{value: identifier}
			if number, err := strconv.Atoi(identifier); err == nil {
				if len(identifier) > 1 && identifier[0] == '0' {
					return version{}, errors.New("numeric prerelease identifiers cannot contain leading zeroes")
				}
				parsed.number, parsed.numeric = number, true
			}
			result.prerelease = append(result.prerelease, parsed)
		}
	}
	return result, nil
}

func compareVersion(a, b version) int {
	left := [3]int{a.major, a.minor, a.patch}
	right := [3]int{b.major, b.minor, b.patch}
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) == 0 {
		return 0
	}
	if len(a.prerelease) == 0 {
		return 1
	}
	if len(b.prerelease) == 0 {
		return -1
	}
	limit := len(a.prerelease)
	if len(b.prerelease) < limit {
		limit = len(b.prerelease)
	}
	for i := 0; i < limit; i++ {
		leftIdentifier, rightIdentifier := a.prerelease[i], b.prerelease[i]
		switch {
		case leftIdentifier.numeric && rightIdentifier.numeric:
			if leftIdentifier.number < rightIdentifier.number {
				return -1
			}
			if leftIdentifier.number > rightIdentifier.number {
				return 1
			}
		case leftIdentifier.numeric:
			return -1
		case rightIdentifier.numeric:
			return 1
		default:
			if leftIdentifier.value < rightIdentifier.value {
				return -1
			}
			if leftIdentifier.value > rightIdentifier.value {
				return 1
			}
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1
	}
	return 0
}
