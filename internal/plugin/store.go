package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type Receipt struct {
	SchemaVersion   int      `json:"schemaVersion"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	BinarySHA256    string   `json:"binarySha256"`
	Executable      string   `json:"executable"`
	PreviousVersion string   `json:"previousVersion,omitempty"`
	Manifest        Manifest `json:"manifest"`
}

type Installed struct {
	Receipt Receipt
	Path    string
}

type Store struct{ root string }

func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve current user home: %w", err)
	}
	return NewStoreAt(filepath.Join(home, ".local", "share", "blazn", "plugins"))
}

func NewStoreAt(root string) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return nil, errors.New("plugin store path must be a safe absolute path")
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Current(name string) (Installed, error) {
	if !safeIdentifier.MatchString(name) {
		return Installed{}, errors.New("plugin name is invalid")
	}
	pluginDir := filepath.Join(s.root, name)
	if err := validatePrivateDirectory(pluginDir); err != nil {
		return Installed{}, err
	}
	receiptPath := filepath.Join(pluginDir, "current.json")
	receipt, err := readReceipt(receiptPath)
	if err != nil {
		return Installed{}, err
	}
	if receipt.Name != name || receipt.SchemaVersion != 1 || receipt.Executable != receipt.Manifest.Executable || receipt.Version != receipt.Manifest.Version {
		return Installed{}, errors.New("plugin receipt is inconsistent")
	}
	path := filepath.Join(pluginDir, "versions", receipt.Version, receipt.Executable)
	if err := validateOwnedRegular(path, 0o022); err != nil {
		return Installed{}, fmt.Errorf("installed plugin is unsafe: %w", err)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return Installed{}, err
	}
	if digest != receipt.BinarySHA256 {
		return Installed{}, errors.New("installed plugin differs from its receipt")
	}
	return Installed{Receipt: receipt, Path: path}, nil
}

func (s *Store) Activate(definition Definition, manifest Manifest, binary string) (Receipt, error) {
	if err := manifest.Validate(); err != nil {
		return Receipt{}, err
	}
	if manifest.Name != definition.Name || manifest.Executable != definition.Executable {
		return Receipt{}, errors.New("plugin manifest does not match the catalog")
	}
	commands := map[string]bool{definition.CanonicalCommand: true}
	for _, alias := range definition.Aliases {
		commands[alias] = true
	}
	for command := range commands {
		found := false
		for _, claimed := range manifest.Commands {
			if claimed == command {
				found = true
				break
			}
		}
		if !found {
			return Receipt{}, fmt.Errorf("plugin manifest does not claim catalog command %q", command)
		}
	}
	if err := s.ensureRoot(); err != nil {
		return Receipt{}, err
	}
	releaseLock, err := s.acquireLock(definition.Name)
	if err != nil {
		return Receipt{}, err
	}
	defer releaseLock()
	pluginDir := filepath.Join(s.root, definition.Name)
	if err := ensurePrivateDirectory(pluginDir); err != nil {
		return Receipt{}, err
	}
	versions := filepath.Join(pluginDir, "versions")
	if err := ensurePrivateDirectory(versions); err != nil {
		return Receipt{}, err
	}
	digest, err := fileSHA256(binary)
	if err != nil {
		return Receipt{}, err
	}
	versionReceipt := Receipt{SchemaVersion: 1, Name: definition.Name, Version: manifest.Version, BinarySHA256: digest, Executable: manifest.Executable, Manifest: manifest}
	previous := ""
	if installed, err := s.Current(definition.Name); err == nil {
		previous = installed.Receipt.Version
	} else if !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, err
	}
	versionDir := filepath.Join(versions, manifest.Version)
	if _, err := os.Lstat(versionDir); errors.Is(err, os.ErrNotExist) {
		stage, err := os.MkdirTemp(pluginDir, ".install-*")
		if err != nil {
			return Receipt{}, err
		}
		committed := false
		defer func() {
			if !committed {
				_ = os.RemoveAll(stage)
			}
		}()
		if err := os.Chmod(stage, 0o700); err != nil {
			return Receipt{}, err
		}
		destination := filepath.Join(stage, manifest.Executable)
		if err := copyExecutable(binary, destination); err != nil {
			return Receipt{}, err
		}
		if err := writeJSONAtomic(stage, "manifest.json", manifest); err != nil {
			return Receipt{}, err
		}
		if err := writeJSONAtomic(stage, "version.json", versionReceipt); err != nil {
			return Receipt{}, err
		}
		if err := os.Rename(stage, versionDir); err != nil {
			return Receipt{}, err
		}
		committed = true
	} else if err != nil {
		return Receipt{}, err
	} else {
		existingDigest, err := fileSHA256(filepath.Join(versionDir, manifest.Executable))
		if err != nil || existingDigest != digest {
			return Receipt{}, errors.New("plugin version already exists with different contents")
		}
		existingReceipt, err := readReceipt(filepath.Join(versionDir, "version.json"))
		if err != nil || existingReceipt.Name != manifest.Name || existingReceipt.Version != manifest.Version || existingReceipt.BinarySHA256 != digest {
			return Receipt{}, errors.New("plugin version already exists with invalid metadata")
		}
	}
	receipt := Receipt{SchemaVersion: 1, Name: definition.Name, Version: manifest.Version, BinarySHA256: digest, Executable: manifest.Executable, PreviousVersion: previous, Manifest: manifest}
	if previous == manifest.Version {
		receipt.PreviousVersion = ""
	}
	if err := writeJSONAtomic(pluginDir, "current.json", receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (s *Store) Rollback(name string) (Receipt, error) {
	if err := s.ensureRoot(); err != nil {
		return Receipt{}, err
	}
	releaseLock, err := s.acquireLock(name)
	if err != nil {
		return Receipt{}, err
	}
	defer releaseLock()
	installed, err := s.Current(name)
	if err != nil {
		return Receipt{}, err
	}
	previous := installed.Receipt.PreviousVersion
	if previous == "" {
		return Receipt{}, errors.New("plugin has no rollback version")
	}
	previousDir := filepath.Join(s.root, name, "versions", previous)
	previousReceipt, err := readReceipt(filepath.Join(previousDir, "version.json"))
	if err != nil {
		return Receipt{}, errors.New("plugin rollback metadata is unavailable")
	}
	if previousReceipt.Name != name || previousReceipt.Version != previous || previousReceipt.PreviousVersion != "" {
		return Receipt{}, errors.New("plugin rollback metadata is inconsistent")
	}
	previousPath := filepath.Join(previousDir, previousReceipt.Executable)
	digest, err := fileSHA256(previousPath)
	if err != nil {
		return Receipt{}, errors.New("plugin rollback version is unavailable")
	}
	if digest != previousReceipt.BinarySHA256 {
		return Receipt{}, errors.New("plugin rollback version differs from its immutable receipt")
	}
	receipt := previousReceipt
	receipt.PreviousVersion = installed.Receipt.Version
	if err := writeJSONAtomic(filepath.Join(s.root, name), "current.json", receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (s *Store) Remove(name string) error {
	if err := s.ensureRoot(); err != nil {
		return err
	}
	releaseLock, err := s.acquireLock(name)
	if err != nil {
		return err
	}
	defer releaseLock()
	if _, err := s.Current(name); err != nil {
		return err
	}
	pluginDir := filepath.Join(s.root, name)
	removed := filepath.Join(s.root, ".remove-"+name)
	if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
		return errors.New("stale plugin removal exists")
	}
	if err := os.Rename(pluginDir, removed); err != nil {
		return err
	}
	return os.RemoveAll(removed)
}

func (s *Store) ensureRoot() error { return ensurePrivateDirectory(s.root) }

func (s *Store) acquireLock(name string) (func(), error) {
	if !safeIdentifier.MatchString(name) {
		return nil, errors.New("plugin name is invalid")
	}
	path := filepath.Join(s.root, "."+name+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("plugin lock file is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("plugin operation is already in progress")
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return validatePrivateDirectory(path)
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("plugin directory is not private")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("plugin directory is not owned by the current user")
	}
	return nil
}

func validateOwnedRegular(path string, forbidden os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&forbidden != 0 {
		return errors.New("expected an owner-controlled regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return errors.New("file ownership or link count is unsafe")
	}
	return nil
}

func readReceipt(path string) (Receipt, error) {
	if err := validateOwnedRegular(path, 0o077); err != nil {
		return Receipt{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Receipt{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxManifestSize+1))
	decoder.DisallowUnknownFields()
	var receipt Receipt
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, errors.New("plugin receipt is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Receipt{}, errors.New("plugin receipt contains trailing data")
	}
	if err := receipt.Manifest.Validate(); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func readManifest(path string) (Manifest, error) {
	if err := validateOwnedRegular(path, 0o077); err != nil {
		return Manifest{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()
	return DecodeManifest(file)
}

func writeJSONAtomic(directory, name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".receipt-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, filepath.Join(directory, name)); err != nil {
		return err
	}
	committed = true
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func copyExecutable(source, destination string) error {
	if err := validateOwnedRegular(source, 0o022); err != nil {
		return fmt.Errorf("plugin candidate is unsafe: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
