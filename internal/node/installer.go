package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type PriorState struct {
	State    string
	Material client.NodeRollbackMaterial
}

type ServicePriorState struct {
	Enabled bool
	Active  bool
}

type Platform interface {
	Preflight(context.Context, client.NodeInstallPlan) error
	ServiceState(context.Context, client.NodeInstallService) (ServicePriorState, error)
	Capture(context.Context, client.NodeInstallMutation, string) (PriorState, error)
	Apply(context.Context, client.NodeInstallMutation) error
	Rollback(context.Context, client.NodeInstallMutation, PriorState) error
	Verify(context.Context, client.NodeInstallPlan) error
}

type Installer struct {
	platform        Platform
	state           StateStore
	now             func() time.Time
	uid             func() int64
	processIdentity func() string
	verifyNoSymlink func(string) error
}

func NewInstaller(platform Platform, state StateStore) *Installer {
	return &Installer{platform: platform, state: state, now: time.Now, uid: currentUID, processIdentity: func() string { return fmt.Sprintf("pid-%d-start-%d", os.Getpid(), time.Now().UnixNano()) }, verifyNoSymlink: verifyNoSymlinkTraversal}
}

func (i *Installer) Install(ctx context.Context, plan client.NodeInstallPlan, identityMeta client.NodeEnrollmentIdentity, identity Identity) (client.NodeInstallReceipt, error) {
	if i.platform == nil || i.state == nil {
		return client.NodeInstallReceipt{}, errors.New("installer dependencies are incomplete")
	}
	release, err := i.state.AcquireInstallLock()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	defer release()
	if i.uid() != 0 {
		return client.NodeInstallReceipt{}, errors.New("privileged node install requires UID 0")
	}
	fingerprint, err := identity.Fingerprint()
	issuedAt, issuedErr := time.Parse(time.RFC3339, identityMeta.IssuedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339, identityMeta.ExpiresAt)
	if err != nil || identityMeta.Generation < 1 || identityMeta.SigningKeyID == "" || identityMeta.PublicKeyFingerprint != fingerprint || identityMeta.IssuedAt != plan.IssuedAt || issuedErr != nil || expiresErr != nil || !expiresAt.After(issuedAt) {
		return client.NodeInstallReceipt{}, errors.New("installer identity does not match the enrolled identity")
	}
	if existing, loadErr := i.state.LoadReceipt(); loadErr == nil {
		trust := client.NodeInstallReceiptTrust{PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Signer: client.NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: identityMeta.SigningKeyID, Generation: identityMeta.Generation, Fingerprint: fingerprint, PublicKey: identity.PublicKey}, BackupRoot: plan.Rollback.BackupRoot, VerifyNoSymlinkTraversal: i.verifyNoSymlink}
		if existing.State != "active" {
			return client.NodeInstallReceipt{}, errors.New("prior node install receipt requires explicit recovery before reinstall")
		}
		if err := client.VerifyNodeInstallReceipt(existing, trust); err != nil {
			return client.NodeInstallReceipt{}, fmt.Errorf("existing node install receipt is untrusted: %w", err)
		}
		if err := i.platform.Verify(ctx, plan); err != nil {
			return client.NodeInstallReceipt{}, fmt.Errorf("live node state drifted from the active receipt: %w", err)
		}
		return existing, nil
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return client.NodeInstallReceipt{}, loadErr
	}
	if err := i.platform.Preflight(ctx, plan); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	servicePrior, err := i.platform.ServiceState(ctx, plan.NodeService)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	owner, err := i.owner()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	receiptID, err := newUUID()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	created := i.now().UTC()
	wal := InstallWAL{SchemaVersion: 1, ReceiptID: receiptID, Generation: 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "preflight", Owner: owner, ServicePrior: servicePrior, Mutations: []client.NodeReceiptMutation{}, CreatedAt: nowString(created), UpdatedAt: nowString(created)}
	if err := i.state.CreateWAL(wal); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	mutations := append([]client.NodeInstallMutation(nil), plan.Mutations...)
	sort.Slice(mutations, func(a, b int) bool { return mutations[a].Ordinal < mutations[b].Ordinal })
	for _, mutation := range mutations {
		prior, err := i.platform.Capture(ctx, mutation, plan.Rollback.BackupRoot)
		if err != nil {
			return i.failAndRollback(ctx, plan, identityMeta, identity, servicePrior, &wal, fmt.Errorf("capture mutation %d: %w", mutation.Ordinal, err))
		}
		if err := validatePriorState(prior); err != nil {
			return i.failAndRollback(ctx, plan, identityMeta, identity, servicePrior, &wal, fmt.Errorf("capture mutation %d returned unsafe rollback state: %w", mutation.Ordinal, err))
		}
		entry := client.NodeReceiptMutation{Ordinal: mutation.Ordinal, Kind: mutation.Kind, Target: mutation.Target, PriorState: prior.State, RollbackMaterial: prior.Material, DesiredDigest: mutation.DesiredDigest, Status: "pending"}
		wal.Stage = "install"
		wal.Mutations = append(wal.Mutations, entry)
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(wal); err != nil {
			return client.NodeInstallReceipt{}, err
		}
		if err := i.platform.Apply(ctx, mutation); err != nil {
			return i.failAndRollback(ctx, plan, identityMeta, identity, servicePrior, &wal, fmt.Errorf("apply mutation %d: %w", mutation.Ordinal, err))
		}
		wal.Mutations[len(wal.Mutations)-1].Status = "applied"
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(wal); err != nil {
			return client.NodeInstallReceipt{}, err
		}
	}
	if err := i.platform.Verify(ctx, plan); err != nil {
		return i.failAndRollback(ctx, plan, identityMeta, identity, servicePrior, &wal, fmt.Errorf("verify install: %w", err))
	}
	wal.Stage = "complete"
	wal.UpdatedAt = nowString(i.now())
	if err := i.state.SaveWAL(wal); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	receipt, err := i.receipt(plan, identityMeta, identity, servicePrior, wal, "active", nil)
	if err != nil {
		return receipt, err
	}
	if err := i.state.SaveReceipt(receipt); err != nil {
		return receipt, err
	}
	if err := i.state.RemoveWAL(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (i *Installer) Recover(ctx context.Context, plan client.NodeInstallPlan, identityMeta client.NodeEnrollmentIdentity, identity Identity) (client.NodeInstallReceipt, error) {
	release, err := i.state.AcquireInstallLock()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	defer release()
	if i.uid() != 0 {
		return client.NodeInstallReceipt{}, errors.New("privileged node recovery requires UID 0")
	}
	wal, err := i.state.LoadWAL()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if wal.PlanID != plan.PlanID || wal.PlanDigest != plan.Digest || wal.NodeID != plan.NodeID {
		return client.NodeInstallReceipt{}, errors.New("install WAL does not match the verified plan")
	}
	if err := validateWALOwner(wal.Owner); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	servicePrior := wal.ServicePrior
	if wal.Stage == "complete" {
		receipt, err := i.receipt(plan, identityMeta, identity, servicePrior, wal, "active", nil)
		if err != nil {
			return receipt, err
		}
		if err := i.state.SaveReceipt(receipt); err != nil {
			return receipt, err
		}
		if err := i.state.RemoveWAL(); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	residues, rollbackErr := i.rollback(ctx, plan, &wal)
	if rollbackErr != nil {
		return client.NodeInstallReceipt{}, rollbackErr
	}
	state := "removed"
	if len(residues) > 0 {
		state = "recovery_required"
	}
	receipt, err := i.receipt(plan, identityMeta, identity, servicePrior, wal, state, residues)
	if err != nil {
		return receipt, err
	}
	if err := i.state.SaveReceipt(receipt); err != nil {
		return receipt, err
	}
	if state == "removed" {
		if err := i.state.RemoveWAL(); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	return receipt, errors.New("install recovery left receipt-bound residues")
}

func (i *Installer) failAndRollback(ctx context.Context, plan client.NodeInstallPlan, meta client.NodeEnrollmentIdentity, identity Identity, servicePrior ServicePriorState, wal *InstallWAL, cause error) (client.NodeInstallReceipt, error) {
	residues, rollbackErr := i.rollback(ctx, plan, wal)
	if rollbackErr != nil {
		return client.NodeInstallReceipt{}, fmt.Errorf("%v; persist rollback WAL: %w", cause, rollbackErr)
	}
	state := "removed"
	if len(residues) > 0 {
		state = "recovery_required"
	}
	receipt, receiptErr := i.receipt(plan, meta, identity, servicePrior, *wal, state, residues)
	if receiptErr == nil {
		receiptErr = i.state.SaveReceipt(receipt)
	}
	if state == "removed" && receiptErr == nil {
		receiptErr = i.state.RemoveWAL()
	}
	if receiptErr != nil {
		return receipt, fmt.Errorf("%v; persist rollback receipt: %w", cause, receiptErr)
	}
	return receipt, cause
}

func (i *Installer) rollback(ctx context.Context, plan client.NodeInstallPlan, wal *InstallWAL) ([]client.NodeReceiptResidue, error) {
	byOrdinal := map[int64]client.NodeInstallMutation{}
	for _, mutation := range plan.Mutations {
		byOrdinal[mutation.Ordinal] = mutation
	}
	residues := []client.NodeReceiptResidue{}
	wal.Stage = "configure"
	wal.UpdatedAt = nowString(i.now())
	if err := i.state.SaveWAL(*wal); err != nil {
		return residues, err
	}
	for index := len(wal.Mutations) - 1; index >= 0; index-- {
		entry := &wal.Mutations[index]
		if entry.Status != "applied" && entry.Status != "pending" {
			continue
		}
		mutation, ok := byOrdinal[entry.Ordinal]
		if !ok {
			entry.Status = "residue"
			residues = append(residues, client.NodeReceiptResidue{Target: entry.Target, ReasonCode: "mutation_missing", SafeMessage: "verified plan no longer contains the applied mutation"})
			continue
		}
		prior := PriorState{State: entry.PriorState, Material: entry.RollbackMaterial}
		if err := i.platform.Rollback(ctx, mutation, prior); err != nil {
			entry.Status = "residue"
			residues = append(residues, client.NodeReceiptResidue{Target: entry.Target, ReasonCode: "rollback_failed", SafeMessage: "platform rollback failed; manual recovery is required"})
		} else if entry.PriorState == "absent" {
			entry.Status = "removed"
		} else {
			entry.Status = "restored"
		}
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(*wal); err != nil {
			return residues, err
		}
	}
	return residues, nil
}

func (i *Installer) receipt(plan client.NodeInstallPlan, meta client.NodeEnrollmentIdentity, identity Identity, servicePrior ServicePriorState, wal InstallWAL, state string, residues []client.NodeReceiptResidue) (client.NodeInstallReceipt, error) {
	if residues == nil {
		residues = []client.NodeReceiptResidue{}
	}
	fingerprint, err := identity.Fingerprint()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if wal.ReceiptID == "" || wal.Generation < 1 {
		return client.NodeInstallReceipt{}, errors.New("install WAL receipt identity is invalid")
	}
	receipt := client.NodeInstallReceipt{SchemaVersion: client.NodeSchemaVersion, ReceiptID: wal.ReceiptID, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Generation: wal.Generation, NodeIdentityGeneration: meta.Generation, SignerKind: "node_identity", SignerFingerprint: fingerprint, State: state, CurrentStage: "complete", Owner: wal.Owner, Binary: client.NodeReceiptBinary{Path: plan.NodeService.BinaryPath, Digest: binaryDigest(plan)}, Service: client.NodeReceiptService{Manager: plan.NodeService.Manager, Name: plan.NodeService.UnitName, DefinitionDigest: "sha256:" + plan.NodeService.DefinitionSHA256, PriorEnabled: servicePrior.Enabled, PriorActive: servicePrior.Active}, Mutations: wal.Mutations, Residues: residues, CreatedAt: wal.CreatedAt, UpdatedAt: nowString(i.now()), SigningKeyID: meta.SigningKeyID}
	digest, err := client.NodeInstallReceiptDigest(receipt)
	if err != nil {
		return receipt, err
	}
	receipt.Digest = digest
	receipt.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(identity.PrivateKey, []byte("blazn-node-install-receipt-v1\n"+digest)))
	if err := client.ValidateNodeInstallReceipt(receipt); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (i *Installer) owner() (client.NodeReceiptOwner, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return client.NodeReceiptOwner{}, err
	}
	return client.NodeReceiptOwner{UID: i.uid(), PID: int64(os.Getpid()), ProcessStartIdentity: i.processIdentity(), Nonce: base64.RawURLEncoding.EncodeToString(nonce)}, nil
}
func binaryDigest(plan client.NodeInstallPlan) string {
	for _, component := range plan.Components {
		if component.ArtifactType == "binary" && component.SourceClass == "current_binary" {
			return "sha256:" + component.SHA256
		}
	}
	return ""
}
func validatePriorState(prior PriorState) error {
	switch prior.State {
	case "absent":
		if prior.Material.Kind != "absent" || prior.Material.Locator != "" || prior.Material.Digest != "" || prior.Material.Mode != nil || prior.Material.UID != nil || prior.Material.GID != nil {
			return errors.New("absent prior state contains rollback material")
		}
	case "owned", "preexisting_exact":
		if prior.Material.Kind == "" || prior.Material.Kind == "absent" || prior.Material.Locator == "" || prior.Material.Digest == "" || prior.Material.Mode == nil || prior.Material.UID == nil || prior.Material.GID == nil {
			return errors.New("pre-existing prior state lacks complete rollback material")
		}
	default:
		return errors.New("prior state is invalid")
	}
	return nil
}
func validateWALOwner(owner client.NodeReceiptOwner) error {
	nonce, err := base64.RawURLEncoding.DecodeString(owner.Nonce)
	if err != nil || len(nonce) < 24 || owner.UID != 0 || owner.PID < 1 || owner.ProcessStartIdentity == "" {
		return errors.New("install WAL owner fencing is invalid")
	}
	return nil
}
func newUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
