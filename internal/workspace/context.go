package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"time"
)

var ErrNoContext = errors.New("no workspace is selected")

type Selection struct {
	SchemaVersion int       `json:"schemaVersion"`
	APIOrigin     string    `json:"apiOrigin"`
	UserID        string    `json:"userId"`
	WorkspaceID   string    `json:"workspaceId"`
	SelectedAt    time.Time `json:"selectedAt"`
}

type ContextStore interface {
	Load(origin, userID string) (Selection, error)
	Save(Selection) error
}

type FileContextStore struct{ home string }

func NewFileContextStore() (*FileContextStore, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve current user for workspace context: %w", err)
	}
	return NewFileContextStoreAtHome(current.HomeDir)
}

func NewFileContextStoreAtHome(home string) (*FileContextStore, error) {
	if !filepath.IsAbs(home) {
		return nil, errors.New("workspace context home must be absolute")
	}
	return &FileContextStore{home: home}, nil
}

func (s *FileContextStore) Load(origin, userID string) (Selection, error) {
	path := s.path(origin, userID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Selection{}, ErrNoContext
	}
	if err != nil {
		return Selection{}, err
	}
	if err := validatePrivateFile(info); err != nil {
		return Selection{}, fmt.Errorf("workspace context is unsafe: %w", err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Selection{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if opened, err := file.Stat(); err != nil {
		return Selection{}, err
	} else if err := validatePrivateFile(opened); err != nil {
		return Selection{}, fmt.Errorf("opened workspace context is unsafe: %w", err)
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(encoded) > 4096 {
		return Selection{}, errors.New("workspace context cannot be read safely")
	}
	var selection Selection
	if err := json.Unmarshal(encoded, &selection); err != nil {
		return Selection{}, errors.New("workspace context is invalid")
	}
	if selection.SchemaVersion != 1 || selection.APIOrigin != origin || selection.UserID != userID || selection.WorkspaceID == "" || selection.SelectedAt.IsZero() {
		return Selection{}, errors.New("workspace context does not match the current origin and user")
	}
	return selection, nil
}

func (s *FileContextStore) Save(selection Selection) error {
	if selection.APIOrigin == "" || selection.UserID == "" || selection.WorkspaceID == "" {
		return errors.New("workspace context is incomplete")
	}
	selection.SchemaVersion = 1
	if selection.SelectedAt.IsZero() {
		selection.SelectedAt = time.Now().UTC()
	}
	path := s.path(selection.APIOrigin, selection.UserID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("workspace context directory is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("workspace context directory is not owned by the current user")
	}
	encoded, err := json.Marshal(selection)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".context.tmp.*")
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
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	committed = true
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *FileContextStore) path(origin, userID string) string {
	originHash := sha256.Sum256([]byte(origin))
	userHash := sha256.Sum256([]byte(userID))
	return filepath.Join(s.home, ".local", "share", "blazn", "workspace-contexts", hex.EncodeToString(originHash[:16]), hex.EncodeToString(userHash[:16])+".json")
}

func validatePrivateFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("expected a private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return errors.New("file is not solely owned by the current user")
	}
	return nil
}
