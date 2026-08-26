package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/blazncloud/blazn/internal/client"
)

type TrustedProfileFile struct {
	SchemaVersion               int                            `json:"schemaVersion"`
	ID                          string                         `json:"id"`
	ControlPlaneOrigin          string                         `json:"controlPlaneOrigin"`
	AllowedClusterOrigins       []string                       `json:"allowedClusterOrigins"`
	AllowedDownloadOrigins      []string                       `json:"allowedDownloadOrigins"`
	AllowedDownloadHostSuffixes []string                       `json:"allowedDownloadHostSuffixes"`
	AllowedRegistryOrigins      []string                       `json:"allowedRegistryOrigins"`
	AllowedMutationRoots        []string                       `json:"allowedMutationRoots"`
	EmbeddedComponentSHA256     map[string]string              `json:"embeddedComponentSha256"`
	LimaBinding                 *client.NodeTrustedLimaBinding `json:"limaBinding,omitempty"`
}

func LoadTrustedProfile(path, currentBinaryPath, currentVersion string) (client.NodeTrustedInstallProfile, error) {
	profile, _, err := loadTrustedProfileForOwner(path, currentBinaryPath, currentVersion, currentUID())
	return profile, err
}

func loadTrustedProfileForOwner(path, currentBinaryPath, currentVersion string, profileOwner int64) (client.NodeTrustedInstallProfile, []byte, error) {
	if !filepath.IsAbs(path) || !filepath.IsAbs(currentBinaryPath) || currentVersion == "" || profileOwner < 0 {
		return client.NodeTrustedInstallProfile{}, nil, errors.New("trusted profile and current binary inputs must be absolute and versioned")
	}
	if err := verifyNoSymlinkTraversal(currentBinaryPath); err != nil {
		return client.NodeTrustedInstallProfile{}, nil, fmt.Errorf("current binary path is unsafe: %w", err)
	}
	encoded, err := readTrustedProfileForOwner(path, profileOwner)
	if err != nil {
		return client.NodeTrustedInstallProfile{}, nil, err
	}
	var stored TrustedProfileFile
	decoder := json.NewDecoder(bytesReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return client.NodeTrustedInstallProfile{}, nil, errors.New("trusted install profile is invalid")
	}
	if stored.SchemaVersion != 1 || stored.ID == "" || !validControlPlaneOrigin(stored.ControlPlaneOrigin) {
		return client.NodeTrustedInstallProfile{}, nil, errors.New("trusted install profile schema or ID is invalid")
	}
	binaryInfo, err := os.Lstat(currentBinaryPath)
	if err != nil {
		return client.NodeTrustedInstallProfile{}, nil, err
	}
	owner, nlink, ok := fileOwner(binaryInfo)
	if !ok || owner != currentUID() || nlink != 1 || !binaryInfo.Mode().IsRegular() || binaryInfo.Mode()&os.ModeSymlink != 0 || binaryInfo.Mode().Perm()&0022 != 0 || binaryInfo.Mode().Perm()&0111 == 0 {
		return client.NodeTrustedInstallProfile{}, nil, errors.New("current binary ownership or mode is unsafe")
	}
	file, err := openNoFollow(currentBinaryPath)
	if err != nil {
		return client.NodeTrustedInstallProfile{}, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(binaryInfo, opened) {
		return client.NodeTrustedInstallProfile{}, nil, errors.New("current binary changed while opening")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return client.NodeTrustedInstallProfile{}, nil, err
	}
	profile := client.NodeTrustedInstallProfile{ID: stored.ID, ControlPlaneOrigin: stored.ControlPlaneOrigin, AllowedClusterOrigins: stored.AllowedClusterOrigins, AllowedDownloadOrigins: stored.AllowedDownloadOrigins, AllowedDownloadHostSuffixes: stored.AllowedDownloadHostSuffixes, AllowedRegistryOrigins: stored.AllowedRegistryOrigins, AllowedMutationRoots: stored.AllowedMutationRoots, CurrentBinaryVersion: currentVersion, CurrentBinarySHA256: hex.EncodeToString(hash.Sum(nil)), EmbeddedComponentSHA256: stored.EmbeddedComponentSHA256, LimaBinding: stored.LimaBinding, VerifyNoSymlinkTraversal: verifyProfileTargetTraversal}
	return profile, encoded, nil
}

func readTrustedProfile(path string) ([]byte, error) {
	return readTrustedProfileForOwner(path, currentUID())
}

func readTrustedProfileForOwner(path string, expectedOwner int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	owner, nlink, ok := fileOwner(info)
	mode := info.Mode().Perm()
	if !ok || !trustedProfileAccess(owner, expectedOwner, mode) || nlink != 1 || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("trusted profile ownership or mode is unsafe")
	}
	if err := ensurePrivateDirectory(filepath.Dir(path), expectedOwner); err != nil {
		return nil, err
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("trusted profile changed while opening")
	}
	value, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(value) > 64<<10 {
		return nil, errors.New("trusted profile cannot be read safely")
	}
	return value, nil
}

func trustedProfileAccess(owner, expectedOwner int64, mode os.FileMode) bool {
	return expectedOwner >= 0 && owner == expectedOwner && (mode == 0400 || mode == 0600)
}

func ensureTrustedProfileRoot(path string, expectedOwner int64) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, _, ok := fileOwner(info)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted profile directory is unsafe")
	}
	if owner == expectedOwner && info.Mode().Perm() == 0700 {
		return ensurePrivateDirectory(path, expectedOwner)
	}
	return errors.New("trusted profile directory ownership or mode is unsafe")
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

func verifyProfileTargetTraversal(target string) error {
	err := verifyNoSymlinkTraversal(target)
	if err != nil && currentUID() != 0 && errors.Is(err, os.ErrPermission) {
		// The caller cannot inspect below a root-only mutation root. The root
		// helper reloads this profile and repeats the traversal check as UID 0
		// before authorizing or applying any mutation.
		return nil
	}
	return err
}
