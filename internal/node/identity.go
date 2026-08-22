package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type Identity struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
}

func (i Identity) PublicBase64() string         { return base64.RawURLEncoding.EncodeToString(i.PublicKey) }
func (i Identity) Fingerprint() (string, error) { return client.NodePublicKeyFingerprint(i.PublicKey) }

type IdentityStore interface{ LoadOrCreate() (Identity, error) }

type FileIdentityStore struct {
	Path string
	Now  func() time.Time
}

type identityFile struct {
	SchemaVersion int    `json:"schemaVersion"`
	PrivateKey    string `json:"privateKey"`
	PublicKey     string `json:"publicKey"`
	CreatedAt     string `json:"createdAt"`
}

func (s FileIdentityStore) LoadOrCreate() (Identity, error) {
	if !filepath.IsAbs(s.Path) {
		return Identity{}, errors.New("identity path must be absolute")
	}
	value, err := readPrivateFile(s.Path, 4096)
	if err == nil {
		return decodeIdentity(value)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	encoded, err := json.Marshal(identityFile{SchemaVersion: 1, PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey), PublicKey: base64.RawURLEncoding.EncodeToString(publicKey), CreatedAt: now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return Identity{}, err
	}
	if err := writePrivateCreate(s.Path, encoded); err != nil {
		if value, readErr := readPrivateFile(s.Path, 4096); readErr == nil {
			return decodeIdentity(value)
		}
		return Identity{}, err
	}
	return Identity{PrivateKey: privateKey, PublicKey: publicKey}, nil
}

func decodeIdentity(value []byte) (Identity, error) {
	var stored identityFile
	decoder := json.NewDecoder(bytesReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return Identity{}, errors.New("node identity file is invalid")
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(stored.PrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return Identity{}, errors.New("node identity private key is invalid")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(stored.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.PublicKey(privateKey[32:]).Equal(ed25519.PublicKey(publicKey)) {
		return Identity{}, errors.New("node identity keypair is inconsistent")
	}
	if stored.SchemaVersion != 1 {
		return Identity{}, errors.New("node identity schema is unsupported")
	}
	if _, err := time.Parse(time.RFC3339Nano, stored.CreatedAt); err != nil {
		return Identity{}, errors.New("node identity timestamp is invalid")
	}
	return Identity{PrivateKey: ed25519.PrivateKey(privateKey), PublicKey: ed25519.PublicKey(publicKey)}, nil
}

func writePrivateAtomic(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0700 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("identity directory is unsafe")
	}
	tmp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func writePrivateCreate(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0700 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private state directory is unsafe")
	}
	tmp, err := os.CreateTemp(dir, ".create-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func readPrivateFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("private state file is unsafe")
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		return nil, errors.New("opened private state file is unsafe")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		return nil, fmt.Errorf("private state exceeds %d bytes", limit)
	}
	return value, nil
}

type byteReader struct {
	value  []byte
	offset int
}

func bytesReader(value []byte) *byteReader { return &byteReader{value: value} }
func (r *byteReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.value) {
		return 0, io.EOF
	}
	n := copy(p, r.value[r.offset:])
	r.offset += n
	return n, nil
}
