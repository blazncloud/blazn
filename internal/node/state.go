package node

import (
	"encoding/json"
	"errors"
	"io"
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
	SchemaVersion      int                                   `json:"schemaVersion"`
	ControlPlaneOrigin string                                `json:"controlPlaneOrigin"`
	Pin                EnrollmentPin                         `json:"pin"`
	Exchange           client.ExchangeNodeEnrollmentResponse `json:"exchange"`
	KubernetesBinding  *client.KubernetesBinding             `json:"kubernetesBinding,omitempty"`
	ActivationGrant    *client.NodeActivationGrant           `json:"activationGrant,omitempty"`
	PendingHeartbeat   *client.NodeHeartbeat                 `json:"pendingHeartbeat,omitempty"`
	CapabilityVersion  int64                                 `json:"capabilityVersion,omitempty"`
	PendingJoin        *PendingJoinState                     `json:"pendingJoin,omitempty"`
	UpdatedAt          string                                `json:"updatedAt"`
}
type PendingJoinState struct {
	PlanID     string `json:"planId"`
	IssuanceID string `json:"issuanceId"`
}

type InstallWAL struct {
	SchemaVersion   int                          `json:"schemaVersion"`
	Lifecycle       string                       `json:"lifecycle"`
	ReceiptID       string                       `json:"receiptId"`
	Generation      int64                        `json:"generation"`
	PlanID          string                       `json:"planId"`
	PlanDigest      string                       `json:"planDigest"`
	NodeID          string                       `json:"nodeId"`
	Stage           string                       `json:"stage"`
	Checkpoint      string                       `json:"checkpoint"`
	OriginalReceipt *client.NodeInstallReceipt   `json:"originalReceipt,omitempty"`
	TerminalReceipt *client.NodeInstallReceipt   `json:"terminalReceipt,omitempty"`
	Owner           client.NodeReceiptOwner      `json:"owner"`
	ServicePrior    ServicePriorState            `json:"servicePrior"`
	Mutations       []client.NodeReceiptMutation `json:"mutations"`
	Residues        []client.NodeReceiptResidue  `json:"residues,omitempty"`
	CreatedAt       string                       `json:"createdAt"`
	UpdatedAt       string                       `json:"updatedAt"`
}
type UninstallCleanupJournal struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Plan          client.NodeInstallPlan    `json:"plan"`
	Receipt       client.NodeInstallReceipt `json:"receipt"`
	CreatedAt     string                    `json:"createdAt"`
}

type StateStore interface {
	AcquireInstallLock() (func(), error)
	AcquireRuntimeLock() (func(), error)
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
func (s FileStateStore) AcquireRuntimeLock() (func(), error) {
	if err := ensurePrivateDirectory(s.Root, currentUID()); err != nil {
		return nil, err
	}
	return lockInstallFile(filepath.Join(s.Root, ".runtime.lock"))
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
	if err == nil && (v.SchemaVersion != 1 || v.Pin.SchemaVersion != 1 || !validControlPlaneOrigin(v.ControlPlaneOrigin) || v.CapabilityVersion < 0 || v.CapabilityVersion > maxSafeCapabilityVersion || (v.PendingHeartbeat != nil && v.PendingHeartbeat.Capability.Version != v.CapabilityVersion+1) || (v.PendingJoin != nil && (v.PendingJoin.PlanID == "" || v.PendingJoin.IssuanceID == ""))) {
		err = errors.New("node runtime state schema is unsupported")
	}
	return v, err
}
func (s FileStateStore) RetireEnrollmentState(expected RuntimeState) error {
	if !filepath.IsAbs(s.Root) {
		return errors.New("node state root must be absolute")
	}
	current, err := s.LoadRuntime()
	if err != nil {
		return err
	}
	if !sameJSON(current, expected) || client.ValidateExchangeNodeEnrollmentResponse(current.Exchange) != nil || current.Exchange.Plan.PlanID == "" || !samePin(current.Pin, expected.Pin) {
		return errors.New("node runtime changed before retirement")
	}
	pin, pinErr := s.LoadPin()
	if pinErr == nil && !samePin(pin, current.Pin) {
		return errors.New("node enrollment pin changed before retirement")
	}
	if pinErr != nil && !errors.Is(pinErr, os.ErrNotExist) {
		return pinErr
	}
	planID := current.Exchange.Plan.PlanID
	runtimeBytes, err := json.Marshal(current)
	if err != nil {
		return err
	}
	pinBytes, err := json.Marshal(current.Pin)
	if err != nil {
		return err
	}
	if err := archivePrivateExact(filepath.Join(s.Root, "retired-"+planID+"-runtime.json"), runtimeBytes); err != nil {
		return err
	}
	if err := archivePrivateExact(filepath.Join(s.Root, "retired-"+planID+"-enrollment-pin.json"), pinBytes); err != nil {
		return err
	}
	if pinErr == nil {
		if err := os.Remove(filepath.Join(s.Root, "enrollment-pin.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(s.Root, "runtime.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
	err := s.read("install-wal.json", 1<<20, &v)
	if err == nil && (v.SchemaVersion != 1 || (v.Lifecycle != "" && v.Lifecycle != "install" && v.Lifecycle != "repair" && v.Lifecycle != "uninstall") || (v.Lifecycle == "repair" && v.OriginalReceipt == nil)) {
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
func (s FileStateStore) SaveUninstallCleanup(v UninstallCleanupJournal) error {
	return s.write("uninstall-cleanup.json", v)
}
func (s FileStateStore) CreateUninstallCleanup(v UninstallCleanupJournal) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	err = writePrivateCreate(filepath.Join(s.Root, "uninstall-cleanup.json"), encoded)
	if errors.Is(err, os.ErrExist) {
		existing, loadErr := s.LoadUninstallCleanup()
		if loadErr == nil && sameJSON(existing, v) {
			return nil
		}
	}
	return err
}
func (s FileStateStore) LoadUninstallCleanup() (UninstallCleanupJournal, error) {
	var v UninstallCleanupJournal
	err := s.read("uninstall-cleanup.json", 1<<20, &v)
	validPlatform := v.Plan.Target.Platform == client.NodePlatformLinux || v.Plan.Target.Platform == client.NodePlatformMacOS
	if err == nil && (v.SchemaVersion != 1 || !validPlatform || client.ValidateNodeInstallReceipt(v.Receipt) != nil || v.Receipt.State != "removed" || v.Receipt.PlanID != v.Plan.PlanID || v.Receipt.PlanDigest != v.Plan.Digest) {
		err = errors.New("uninstall cleanup journal is invalid")
	}
	return v, err
}
func (s FileStateStore) RemoveUninstallCleanup() error {
	err := os.Remove(filepath.Join(s.Root, "uninstall-cleanup.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("node state contains trailing JSON")
	}
	return nil
}

func nowString(now time.Time) string { return now.UTC().Format(time.RFC3339Nano) }
func samePin(a, b EnrollmentPin) bool {
	return a.SchemaVersion == b.SchemaVersion && a.WorkspaceID == b.WorkspaceID && a.EnrollmentID == b.EnrollmentID && a.IdempotencyKey == b.IdempotencyKey && a.Hostname == b.Hostname && a.MachineFingerprint == b.MachineFingerprint && a.ProfileID == b.ProfileID && a.ProfilePath == b.ProfilePath && a.PlanSigningKey == b.PlanSigningKey
}
