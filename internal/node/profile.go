package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/KingJammin/blazn/internal/client"
)

type TrustedProfileFile struct {
	SchemaVersion               int                            `json:"schemaVersion"`
	ID                          string                         `json:"id"`
	AllowedClusterOrigins       []string                       `json:"allowedClusterOrigins"`
	AllowedDownloadOrigins      []string                       `json:"allowedDownloadOrigins"`
	AllowedDownloadHostSuffixes []string                       `json:"allowedDownloadHostSuffixes"`
	AllowedRegistryOrigins      []string                       `json:"allowedRegistryOrigins"`
	AllowedMutationRoots        []string                       `json:"allowedMutationRoots"`
	EmbeddedComponentSHA256     map[string]string              `json:"embeddedComponentSha256"`
	LimaBinding                 *client.NodeTrustedLimaBinding `json:"limaBinding,omitempty"`
}

func LoadTrustedProfile(path, currentBinaryPath, currentVersion string) (client.NodeTrustedInstallProfile, error) {
	if !filepath.IsAbs(path) || !filepath.IsAbs(currentBinaryPath) || currentVersion == "" {
		return client.NodeTrustedInstallProfile{}, errors.New("trusted profile and current binary inputs must be absolute and versioned")
	}
	encoded, err := readPrivateFile(path, 64<<10)
	if err != nil {
		return client.NodeTrustedInstallProfile{}, err
	}
	var stored TrustedProfileFile
	decoder := json.NewDecoder(bytesReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return client.NodeTrustedInstallProfile{}, errors.New("trusted install profile is invalid")
	}
	if stored.SchemaVersion != 1 || stored.ID == "" {
		return client.NodeTrustedInstallProfile{}, errors.New("trusted install profile schema or ID is invalid")
	}
	file, err := openNoFollow(currentBinaryPath)
	if err != nil {
		return client.NodeTrustedInstallProfile{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return client.NodeTrustedInstallProfile{}, err
	}
	profile := client.NodeTrustedInstallProfile{ID: stored.ID, AllowedClusterOrigins: stored.AllowedClusterOrigins, AllowedDownloadOrigins: stored.AllowedDownloadOrigins, AllowedDownloadHostSuffixes: stored.AllowedDownloadHostSuffixes, AllowedRegistryOrigins: stored.AllowedRegistryOrigins, AllowedMutationRoots: stored.AllowedMutationRoots, CurrentBinaryVersion: currentVersion, CurrentBinarySHA256: hex.EncodeToString(hash.Sum(nil)), EmbeddedComponentSHA256: stored.EmbeddedComponentSHA256, LimaBinding: stored.LimaBinding, VerifyNoSymlinkTraversal: verifyNoSymlinkTraversal}
	return profile, nil
}

func verifyNoSymlinkTraversal(target string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("target path is not canonical")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(target, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("target traverses a symbolic link")
		}
	}
	return nil
}
