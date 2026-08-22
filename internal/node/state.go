package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type EnrollmentPin struct {
	SchemaVersion      int                       `json:"schemaVersion"`
	WorkspaceID        string                    `json:"workspaceId"`
	EnrollmentID       string                    `json:"enrollmentId"`
	IdempotencyKey     string                    `json:"idempotencyKey"`
	Hostname           string                    `json:"hostname"`
	MachineFingerprint string                    `json:"machineFingerprint"`
	ProfileID          string                    `json:"profileId"`
	ProfilePath        string                    `json:"profilePath"`
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
	ReceiptID     string                       `json:"receiptId"`
	Generation    int64                        `json:"generation"`
	PlanID        string                       `json:"planId"`
	PlanDigest    string                       `json:"planDigest"`
	NodeID        string                       `json:"nodeId"`
	Stage         string                       `json:"stage"`
	Owner         client.NodeReceiptOwner      `json:"owner"`
	ServicePrior  ServicePriorState            `json:"servicePrior"`
	Mutations     []client.NodeReceiptMutation `json:"mutations"`
	CreatedAt     string                       `json:"createdAt"`
	UpdatedAt     string                       `json:"updatedAt"`
}

type StateStore interface {
	AcquireInstallLock() (func(), error)
	Pin(EnrollmentPin) error
	LoadPin() (EnrollmentPin, error)
	SaveRuntime(RuntimeState) error
	LoadRuntime() (RuntimeState, error)
	SaveWAL(InstallWAL) error
	CreateWAL(InstallWAL) error
	LoadWAL() (InstallWAL, error)
	RemoveWAL() error
	SaveReceipt(client.NodeInstallReceipt) error
	LoadReceipt() (client.NodeInstallReceipt, error)
}

type FileStateStore struct{ Root string }

func (s FileStateStore) AcquireInstallLock() (func(), error) {
	if err := ensurePrivateDirectory(s.Root, currentUID()); err != nil {
		return nil, err
	}
	return lockInstallFile(filepath.Join(s.Root, ".install.lock"))
}

func (s FileStateStore) Pin(v EnrollmentPin) error {
	existing, err := s.LoadPin()
	if err == nil {
		if !samePin(existing, v) {
			return errors.New("node enrollment is already pinned to different trust")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !filepath.IsAbs(s.Root) {
		return errors.New("node state root must be absolute")
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := writePrivateCreate(filepath.Join(s.Root, "enrollment-pin.json"), encoded); err != nil {
		if existing, readErr := s.LoadPin(); readErr == nil && samePin(existing, v) {
			return nil
		}
		return err
	}
	return nil
}
func (s FileStateStore) LoadPin() (EnrollmentPin, error) {
	var v EnrollmentPin
	err := s.read("enrollment-pin.json", 32<<10, &v)
	if err == nil && (v.SchemaVersion != 1 || v.EnrollmentID == "" || v.PlanSigningKey.KeyID == "") {
		err = errors.New("node enrollment pin is invalid")
	}
	return v, err
}
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
func (s FileStateStore) CreateWAL(v InstallWAL) error {
	if !filepath.IsAbs(s.Root) {
		return errors.New("node state root must be absolute")
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writePrivateCreate(filepath.Join(s.Root, "install-wal.json"), encoded)
}
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
func (s FileStateStore) LoadReceipt() (client.NodeInstallReceipt, error) {
	var v client.NodeInstallReceipt
	err := s.read("install-receipt.json", 256<<10, &v)
	if err == nil {
		err = client.ValidateNodeInstallReceipt(v)
	}
	return v, err
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
func samePin(a, b EnrollmentPin) bool {
	return a.SchemaVersion == b.SchemaVersion && a.WorkspaceID == b.WorkspaceID && a.EnrollmentID == b.EnrollmentID && a.IdempotencyKey == b.IdempotencyKey && a.Hostname == b.Hostname && a.MachineFingerprint == b.MachineFingerprint && a.ProfileID == b.ProfileID && a.ProfilePath == b.ProfilePath && a.PlanSigningKey == b.PlanSigningKey
}
