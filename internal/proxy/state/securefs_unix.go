//go:build darwin || linux

package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type FaultInjector interface {
	Check(point string) error
}

type FaultFunc func(string) error

func (f FaultFunc) Check(point string) error { return f(point) }

type noFaults struct{}

func (noFaults) Check(string) error { return nil }

type Paths struct {
	Root        string
	Lock        string
	Reservation string
	Journal     string
	Receipt     string
}

func AccountPaths(platform string, uid int) (Paths, error) {
	if uid < 0 || !oneOf(platform, "darwin", "linux") {
		return Paths{}, fmt.Errorf("%w: unsupported account identity", ErrInvalidState)
	}
	account, err := lookupAccount(strconv.Itoa(uid))
	if err != nil {
		return Paths{}, fmt.Errorf("lookup OS account %d: %w", uid, err)
	}
	var root string
	if platform == "darwin" {
		root = filepath.Join(account, "Library", "Application Support", "Blazn", "proxy")
	} else {
		root = filepath.Join(account, ".local", "share", "blazn", "proxy")
	}
	return pathsAt(root), nil
}

var lookupAccount = func(uid string) (string, error) {
	// os/user is deliberately wrapped so tests can prove invocation-controlled
	// HOME and XDG values are ignored without changing the process account.
	account, err := lookupUserID(uid)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", errors.New("OS account home is not absolute")
	}
	return account.HomeDir, nil
}

func pathsAt(root string) Paths {
	return Paths{
		Root:        root,
		Lock:        filepath.Join(root, "lifecycle.lock"),
		Reservation: filepath.Join(root, "reservation.json"),
		Journal:     filepath.Join(root, "activation-journal.json"),
		Receipt:     filepath.Join(root, "activation-receipt.json"),
	}
}

type userRecord struct{ HomeDir string }

var lookupUserID = func(uid string) (*userRecord, error) {
	account, err := user.LookupId(uid)
	if err != nil {
		return nil, err
	}
	return &userRecord{HomeDir: account.HomeDir}, nil
}

func randomSuffix() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func ensureSecureRoot(root string, uid int) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return fmt.Errorf("%w: state root must be a clean absolute path", ErrInvalidState)
	}
	current := string(filepath.Separator)
	parts := splitPath(root)
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: path component %s is not a real directory", ErrOwnershipAmbiguous, current)
		}
		if index == len(parts)-1 {
			if err := validateOwnerMode(info, uid, 0o700, true); err != nil {
				return fmt.Errorf("state root %s: %w", current, err)
			}
		}
	}
	return nil
}

func splitPath(path string) []string {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	clean = clean[len(volume):]
	var parts []string
	for clean != string(filepath.Separator) && clean != "." {
		dir, file := filepath.Split(clean)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		clean = filepath.Clean(dir)
	}
	return parts
}

func validateOwnerMode(info os.FileInfo, uid int, mode os.FileMode, directory bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid {
		return fmt.Errorf("%w: unexpected owner", ErrOwnershipAmbiguous)
	}
	if directory != info.IsDir() || info.Mode().Perm() != mode {
		return fmt.Errorf("%w: unexpected type or mode %o", ErrOwnershipAmbiguous, info.Mode().Perm())
	}
	if !directory && (info.Mode()&os.ModeType != 0 || stat.Nlink != 1) {
		return fmt.Errorf("%w: state file must be regular and single-link", ErrOwnershipAmbiguous)
	}
	return nil
}

func validateSecureFile(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: symlink state file", ErrOwnershipAmbiguous)
	}
	return validateOwnerMode(info, uid, 0o600, false)
}

func readSecureFile(path string, uid int, max int64) ([]byte, error) {
	if err := validateSecureFile(path, uid); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validateOwnerMode(info, uid, 0o600, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: state record exceeds %d bytes", ErrInvalidState, max)
	}
	return data, nil
}

func atomicWrite(path string, uid int, data []byte, faults FaultInjector) error {
	dir := filepath.Dir(path)
	if err := validateSecureFile(path, uid); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temp, err := os.OpenFile(filepath.Join(dir, ".tmp-"+filepath.Base(path)+"-"+suffix), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()
	if err := validateOwnerModeFile(temp, uid); err != nil {
		return err
	}
	if err := faults.Check("state.temp.opened"); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := faults.Check("state.temp.written"); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := faults.Check("state.temp.synced"); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	closed = true
	if err := faults.Check("state.temp.closed"); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if err := faults.Check("state.renamed"); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return faults.Check("state.parent.synced")
}

func validateOwnerModeFile(file *os.File, uid int) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return validateOwnerMode(info, uid, 0o600, false)
}

func removeSecureFile(path string, uid int, faults FaultInjector) error {
	if err := validateSecureFile(path, uid); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := faults.Check("state.removed"); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return faults.Check("state.remove.parent.synced")
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func lockFile(ctx context.Context, path string, uid int) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := validateOwnerModeFile(file, uid); err != nil {
		file.Close()
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func unlockFile(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
