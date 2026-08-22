package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type EnrollmentPin struct {
	SchemaVersion      int                       `json:"schemaVersion"`
	WorkspaceID        string                    `json:"workspaceId"`
	EnrollmentID       string                    `json:"enrollmentId"`
	IdempotencyKey     string                    `json:"idempotencyKey"`
	Hostname           string                    `json:"hostname"`
	MachineFingerprint string                    `json:"machineFingerprint"`
	ProfileID          string                    `json:"profileId"`
	PlanSigningKey     client.NodePlanSigningKey `json:"planSigningKey"`
	PinnedAt           string                    `json:"pinnedAt"`
}

type RuntimeState struct {
	SchemaVersion int                                   `json:"schemaVersion"`
	Pin           EnrollmentPin                         `json:"pin"`
	Exchange      client.ExchangeNodeEnrollmentResponse `json:"exchange"`
	UpdatedAt     string                                `json:"updatedAt"`
}

type InstallWAL struct {
	SchemaVersion int                          `json:"schemaVersion"`
	PlanID        string                       `json:"planId"`
	PlanDigest    string                       `json:"planDigest"`
	NodeID        string                       `json:"nodeId"`
	Stage         string                       `json:"stage"`
	Owner         client.NodeReceiptOwner      `json:"owner"`
	Mutations     []client.NodeReceiptMutation `json:"mutations"`
	CreatedAt     string                       `json:"createdAt"`
	UpdatedAt     string                       `json:"updatedAt"`
}

type StateStore interface {
	Pin(EnrollmentPin) error
	SaveRuntime(RuntimeState) error
	LoadRuntime() (RuntimeState, error)
	SaveWAL(InstallWAL) error
	LoadWAL() (InstallWAL, error)
	RemoveWAL() error
	SaveReceipt(client.NodeInstallReceipt) error
}

type FileStateStore struct{ Root string }

func (s FileStateStore) Pin(v EnrollmentPin) error        { return s.write("enrollment-pin.json", v) }
func (s FileStateStore) SaveRuntime(v RuntimeState) error { return s.write("runtime.json", v) }
func (s FileStateStore) LoadRuntime() (RuntimeState, error) {
	var v RuntimeState
	err := s.read("runtime.json", 64<<10, &v)
	if err == nil && (v.SchemaVersion != 1 || v.Pin.SchemaVersion != 1) {
		err = errors.New("node runtime state schema is unsupported")
	}
	return v, err
}
func (s FileStateStore) SaveWAL(v InstallWAL) error { return s.write("install-wal.json", v) }
func (s FileStateStore) LoadWAL() (InstallWAL, error) {
	var v InstallWAL
	err := s.read("install-wal.json", 256<<10, &v)
	if err == nil && v.SchemaVersion != 1 {
		err = errors.New("install WAL schema is unsupported")
	}
	return v, err
}
func (s FileStateStore) RemoveWAL() error {
	err := os.Remove(filepath.Join(s.Root, "install-wal.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (s FileStateStore) SaveReceipt(v client.NodeInstallReceipt) error {
	return s.write("install-receipt.json", v)
}

func (s FileStateStore) write(name string, v any) error {
	if !filepath.IsAbs(s.Root) {
		return errors.New("node state root must be absolute")
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writePrivateAtomic(filepath.Join(s.Root, name), encoded)
}
func (s FileStateStore) read(name string, limit int64, v any) error {
	encoded, err := readPrivateFile(filepath.Join(s.Root, name), limit)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytesReader(encoded))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

func nowString(now time.Time) string { return now.UTC().Format(time.RFC3339Nano) }
