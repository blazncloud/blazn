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

	"github.com/blazncloud/blazn/internal/client"
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
	AuthorizeBootstrap(context.Context, BootstrapAuthorization) error
	Preflight(context.Context, client.NodeInstallPlan) error
	ServiceState(context.Context, client.NodeInstallService) (ServicePriorState, error)
	Capture(context.Context, client.NodeInstallMutation, string) (PriorState, error)
	Apply(context.Context, client.NodeInstallMutation) error
	Rollback(context.Context, client.NodeInstallMutation, PriorState) error
	Verify(context.Context, client.NodeInstallPlan) error
}

func (i *Installer) FinalizeServiceState(ctx context.Context, plan client.NodeInstallPlan) error {
	finalizer, ok := i.platform.(interface {
		FinalizeServiceState(context.Context, client.NodeInstallPlan) error
	})
	if !ok {
		return errors.New("platform service-state finalizer is unavailable")
	}
	return finalizer.FinalizeServiceState(ctx, plan)
}

func (i *Installer) AuthorizeBootstrap(ctx context.Context, authorization BootstrapAuthorization) error {
	if i.platform == nil {
		return errors.New("privileged platform is unavailable")
	}
	if err := authorization.Validate(); err != nil {
		return err
	}
	if err := i.platform.AuthorizeBootstrap(ctx, authorization); err != nil {
		return fmt.Errorf("authorize privileged bootstrap: %w", err)
	}
	return nil
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
	if binder, ok := i.state.(interface{ BindPlan(client.NodeInstallPlan) }); ok {
		binder.BindPlan(plan)
	}
	if binder, ok := i.state.(interface{ BindContext(context.Context) }); ok {
		binder.BindContext(ctx)
		defer binder.BindContext(nil)
	}
	fingerprint, err := identity.Fingerprint()
	issuedAt, issuedErr := time.Parse(time.RFC3339, identityMeta.IssuedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339, identityMeta.ExpiresAt)
	if err != nil || identityMeta.Generation < 1 || identityMeta.SigningKeyID == "" || identityMeta.PublicKeyFingerprint != fingerprint || identityMeta.IssuedAt != plan.IssuedAt || issuedErr != nil || expiresErr != nil || !expiresAt.After(issuedAt) {
		return client.NodeInstallReceipt{}, errors.New("installer identity does not match the enrolled identity")
	}
	if existing, loadErr := i.state.LoadReceipt(); loadErr == nil {
		trust := client.NodeInstallReceiptTrust{PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Signer: client.NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: identityMeta.SigningKeyID, Generation: identityMeta.Generation, Fingerprint: fingerprint, PublicKey: identity.PublicKey}, BackupRoot: plan.Rollback.BackupRoot, VerifyNoSymlinkTraversal: i.verifyNoSymlink}
		if existing.State != "active" && existing.State != "removed" {
			return client.NodeInstallReceipt{}, errors.New("prior node install receipt requires explicit recovery before reinstall")
		}
		if err := client.VerifyNodeInstallReceipt(existing, trust); err != nil {
			return client.NodeInstallReceipt{}, fmt.Errorf("existing node install receipt is untrusted: %w", err)
		}
		if existing.State == "active" {
			if err := i.platform.Verify(ctx, plan); err != nil {
				return client.NodeInstallReceipt{}, fmt.Errorf("live node state drifted from the active receipt: %w", err)
			}
			return existing, nil
		}
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
	wal := InstallWAL{SchemaVersion: 1, Lifecycle: "install", ReceiptID: receiptID, Generation: 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "preflight", Owner: owner, ServicePrior: servicePrior, Mutations: []client.NodeReceiptMutation{}, CreatedAt: nowString(created), UpdatedAt: nowString(created)}
	if err := i.state.CreateWAL(wal); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if checkpoints, ok := i.platform.(interface{ SetInstallCheckpoint(func(string) error) }); ok {
		checkpoints.SetInstallCheckpoint(func(value string) error {
			wal.Checkpoint = value
			wal.UpdatedAt = nowString(i.now())
			return i.state.SaveWAL(wal)
		})
		defer checkpoints.SetInstallCheckpoint(nil)
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
	wal.Checkpoint = "receipt"
	wal.UpdatedAt = nowString(i.now())
	if err := i.state.SaveWAL(wal); err != nil {
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
	if binder, ok := i.state.(interface{ BindPlan(client.NodeInstallPlan) }); ok {
		binder.BindPlan(plan)
	}
	if binder, ok := i.state.(interface{ BindContext(context.Context) }); ok {
		binder.BindContext(ctx)
		defer binder.BindContext(nil)
	}
	release, err := i.state.AcquireInstallLock()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	defer release()
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
	if err := i.platform.Preflight(ctx, plan); err != nil {
		return client.NodeInstallReceipt{}, fmt.Errorf("recovery platform preflight: %w", err)
	}
	if reconciler, ok := i.platform.(interface {
		ReconcileRecovery(context.Context, client.NodeInstallPlan) error
	}); ok {
		if err := reconciler.ReconcileRecovery(ctx, plan); err != nil {
			return client.NodeInstallReceipt{}, fmt.Errorf("reconcile recovery checkpoint: %w", err)
		}
	}
	if wal.Lifecycle == "repair" {
		return i.recoverRepair(ctx, plan, identityMeta, identity, &wal)
	}
	if wal.Lifecycle == "uninstall" && wal.TerminalReceipt != nil {
		return *wal.TerminalReceipt, nil
	}
	servicePrior := wal.ServicePrior
	forward := wal.Stage == "complete" || ((wal.Checkpoint == "broker_consume" || wal.Checkpoint == "broker_consumed" || wal.Checkpoint == "verify" || wal.Checkpoint == "receipt") && allMutationStatuses(wal.Mutations, "applied"))
	if forward {
		if wal.Stage != "complete" {
			if err := i.platform.Verify(ctx, plan); err != nil {
				return client.NodeInstallReceipt{}, fmt.Errorf("resume install verification: %w", err)
			}
			wal.Stage = "complete"
			wal.Checkpoint = "receipt"
			wal.UpdatedAt = nowString(i.now())
			if err := i.state.SaveWAL(wal); err != nil {
				return client.NodeInstallReceipt{}, err
			}
		}
		receipt, err := i.receipt(plan, identityMeta, identity, servicePrior, wal, "active", nil)
		if err != nil {
			return receipt, err
		}
		if err := i.state.SaveReceipt(receipt); err != nil {
			return receipt, err
		}
		if err := i.FinalizeServiceState(ctx, plan); err != nil {
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
	if state == "removed" {
		wal.Checkpoint = "cleanup_pending"
		wal.TerminalReceipt = &receipt
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(wal); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	if err := i.state.SaveReceipt(receipt); err != nil {
		return receipt, err
	}
	return receipt, errors.New("install recovery left receipt-bound residues")
}

func allMutationStatuses(mutations []client.NodeReceiptMutation, status string) bool {
	for _, mutation := range mutations {
		if mutation.Status != status {
			return false
		}
	}
	return true
}

func (i *Installer) recoverRepair(ctx context.Context, plan client.NodeInstallPlan, meta client.NodeEnrollmentIdentity, identity Identity, wal *InstallWAL) (client.NodeInstallReceipt, error) {
	if wal.OriginalReceipt == nil || i.verifyReceiptValue(plan, meta, identity, *wal.OriginalReceipt, "active") != nil {
		return client.NodeInstallReceipt{}, errors.New("repair WAL original active receipt is untrusted")
	}
	original := *wal.OriginalReceipt
	if wal.Stage == "complete" {
		receiptWAL := *wal
		receiptWAL.Mutations = append([]client.NodeReceiptMutation(nil), original.Mutations...)
		for index := range receiptWAL.Mutations {
			receiptWAL.Mutations[index].Status = "applied"
		}
		receipt, err := i.receipt(plan, meta, identity, wal.ServicePrior, receiptWAL, "active", nil)
		if err != nil {
			return receipt, err
		}
		if err := i.state.SaveReceipt(receipt); err != nil {
			return receipt, err
		}
		if err := i.FinalizeServiceState(ctx, plan); err != nil {
			return receipt, err
		}
		if err := i.state.RemoveWAL(); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	residues, err := i.rollback(ctx, plan, wal)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if len(residues) > 0 {
		wal.Checkpoint = "repair_recovery_required"
		wal.Residues = append([]client.NodeReceiptResidue(nil), residues...)
		wal.UpdatedAt = nowString(i.now())
		if saveErr := i.state.SaveWAL(*wal); saveErr != nil {
			return original, saveErr
		}
		return original, errors.New("repair recovery left pre-repair restoration residues")
	}
	if err := i.state.SaveReceipt(original); err != nil {
		return original, err
	}
	if err := i.FinalizeServiceState(ctx, plan); err != nil {
		return original, err
	}
	if err := i.state.RemoveWAL(); err != nil {
		return original, err
	}
	return original, nil
}

func (i *Installer) Repair(ctx context.Context, plan client.NodeInstallPlan, identityMeta client.NodeEnrollmentIdentity, identity Identity) (client.NodeInstallReceipt, error) {
	if i.platform == nil || i.state == nil {
		return client.NodeInstallReceipt{}, errors.New("repair dependencies are incomplete")
	}
	release, err := i.state.AcquireInstallLock()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	defer release()
	if binder, ok := i.state.(interface{ BindPlan(client.NodeInstallPlan) }); ok {
		binder.BindPlan(plan)
	}
	if binder, ok := i.state.(interface{ BindContext(context.Context) }); ok {
		binder.BindContext(ctx)
		defer binder.BindContext(nil)
	}
	existing, err := i.trustedActiveReceipt(plan, identityMeta, identity)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	owner, err := i.owner()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	wal := InstallWAL{SchemaVersion: 1, Lifecycle: "repair", OriginalReceipt: &existing, ReceiptID: existing.ReceiptID, Generation: existing.Generation + 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "preflight", Checkpoint: "repair", Owner: owner, ServicePrior: ServicePriorState{Enabled: existing.Service.PriorEnabled, Active: existing.Service.PriorActive}, Mutations: []client.NodeReceiptMutation{}, CreatedAt: existing.CreatedAt, UpdatedAt: nowString(i.now())}
	if err := i.state.CreateWAL(wal); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if err := i.platform.Preflight(ctx, plan); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	mutations := append([]client.NodeInstallMutation(nil), plan.Mutations...)
	sort.Slice(mutations, func(a, b int) bool { return mutations[a].Ordinal < mutations[b].Ordinal })
	for _, mutation := range mutations {
		prior, captureErr := i.platform.Capture(ctx, mutation, plan.Rollback.BackupRoot)
		if captureErr != nil {
			return i.failRepairAndRollback(ctx, plan, identityMeta, identity, &wal, captureErr)
		}
		if err := validatePriorState(prior); err != nil {
			return i.failRepairAndRollback(ctx, plan, identityMeta, identity, &wal, err)
		}
		wal.Mutations = append(wal.Mutations, client.NodeReceiptMutation{Ordinal: mutation.Ordinal, Kind: mutation.Kind, Target: mutation.Target, PriorState: prior.State, RollbackMaterial: prior.Material, DesiredDigest: mutation.DesiredDigest, Status: "pending"})
		wal.Checkpoint = fmt.Sprintf("repair_mutation_%d", mutation.Ordinal)
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(wal); err != nil {
			return client.NodeInstallReceipt{}, err
		}
		if err := i.platform.Apply(ctx, mutation); err != nil {
			return i.failRepairAndRollback(ctx, plan, identityMeta, identity, &wal, err)
		}
		wal.Mutations[len(wal.Mutations)-1].Status = "applied"
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(wal); err != nil {
			return client.NodeInstallReceipt{}, err
		}
	}
	if err := i.platform.Verify(ctx, plan); err != nil {
		return i.failRepairAndRollback(ctx, plan, identityMeta, identity, &wal, err)
	}
	wal.Stage, wal.Checkpoint, wal.UpdatedAt = "complete", "receipt", nowString(i.now())
	if err := i.state.SaveWAL(wal); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	receiptWAL := wal
	receiptWAL.Mutations = append([]client.NodeReceiptMutation(nil), existing.Mutations...)
	for index := range receiptWAL.Mutations {
		receiptWAL.Mutations[index].Status = "applied"
	}
	receipt, err := i.receipt(plan, identityMeta, identity, wal.ServicePrior, receiptWAL, "active", nil)
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

func (i *Installer) Uninstall(ctx context.Context, plan client.NodeInstallPlan, identityMeta client.NodeEnrollmentIdentity, identity Identity, removeManagedRuntime bool) (client.NodeInstallReceipt, error) {
	if i.platform == nil || i.state == nil {
		return client.NodeInstallReceipt{}, errors.New("uninstall dependencies are incomplete")
	}
	release, err := i.state.AcquireInstallLock()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	defer release()
	if binder, ok := i.state.(interface{ BindPlan(client.NodeInstallPlan) }); ok {
		binder.BindPlan(plan)
	}
	if binder, ok := i.state.(interface{ BindContext(context.Context) }); ok {
		binder.BindContext(ctx)
		defer binder.BindContext(nil)
	}
	if existingWAL, loadErr := i.state.LoadWAL(); loadErr == nil && existingWAL.Lifecycle == "uninstall" {
		if existingWAL.TerminalReceipt != nil {
			return *existingWAL.TerminalReceipt, nil
		}
		if existingWAL.PlanID != plan.PlanID || existingWAL.PlanDigest != plan.Digest || validateWALOwner(existingWAL.Owner) != nil {
			return client.NodeInstallReceipt{}, errors.New("existing uninstall WAL differs from verified plan")
		}
		if err := i.platform.Preflight(ctx, plan); err != nil {
			return client.NodeInstallReceipt{}, err
		}
		residues, err := i.rollback(ctx, plan, &existingWAL)
		if err != nil {
			return client.NodeInstallReceipt{}, err
		}
		state := "removed"
		if len(residues) > 0 {
			state = "recovery_required"
		}
		receipt, err := i.receipt(plan, identityMeta, identity, existingWAL.ServicePrior, existingWAL, state, residues)
		if err != nil {
			return receipt, err
		}
		if state != "removed" {
			if err := i.state.SaveReceipt(receipt); err != nil {
				return receipt, err
			}
			return receipt, errors.New("node uninstall left receipt-bound residues")
		}
		existingWAL.Checkpoint = "cleanup_pending"
		existingWAL.TerminalReceipt = &receipt
		existingWAL.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(existingWAL); err != nil {
			return receipt, err
		}
		return receipt, nil
	} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return client.NodeInstallReceipt{}, loadErr
	}
	existing, err := i.trustedActiveReceipt(plan, identityMeta, identity)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	owner, err := i.owner()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	wal := InstallWAL{SchemaVersion: 1, Lifecycle: "uninstall", OriginalReceipt: &existing, ReceiptID: existing.ReceiptID, Generation: existing.Generation + 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "configure", Checkpoint: "uninstall", Owner: owner, ServicePrior: ServicePriorState{Enabled: existing.Service.PriorEnabled, Active: existing.Service.PriorActive}, Mutations: append([]client.NodeReceiptMutation(nil), existing.Mutations...), CreatedAt: existing.CreatedAt, UpdatedAt: nowString(i.now())}
	if !removeManagedRuntime {
		for index := range wal.Mutations {
			if wal.Mutations[index].Kind == "package" || wal.Mutations[index].Kind == "image" {
				wal.Mutations[index].Status = "restored"
			}
		}
	}
	if err := i.state.CreateWAL(wal); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if err := i.platform.Preflight(ctx, plan); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	residues, rollbackErr := i.rollback(ctx, plan, &wal)
	if rollbackErr != nil {
		return client.NodeInstallReceipt{}, rollbackErr
	}
	state := "removed"
	if len(residues) > 0 {
		state = "recovery_required"
	}
	receipt, err := i.receipt(plan, identityMeta, identity, wal.ServicePrior, wal, state, residues)
	if err != nil {
		return receipt, err
	}
	if state == "removed" {
		wal.Checkpoint = "cleanup_pending"
		wal.TerminalReceipt = &receipt
		wal.UpdatedAt = nowString(i.now())
		if err := i.state.SaveWAL(wal); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	if err := i.state.SaveReceipt(receipt); err != nil {
		return receipt, err
	}
	return receipt, errors.New("node uninstall left receipt-bound residues")
}

func (i *Installer) trustedActiveReceipt(plan client.NodeInstallPlan, meta client.NodeEnrollmentIdentity, identity Identity) (client.NodeInstallReceipt, error) {
	receipt, err := i.state.LoadReceipt()
	if err != nil {
		return receipt, err
	}
	if i.verifyReceiptValue(plan, meta, identity, receipt, "active") != nil {
		return receipt, errors.New("active node install receipt is untrusted")
	}
	return receipt, nil
}
func (i *Installer) verifyReceiptValue(plan client.NodeInstallPlan, meta client.NodeEnrollmentIdentity, identity Identity, receipt client.NodeInstallReceipt, state string) error {
	fingerprint, err := identity.Fingerprint()
	if err != nil {
		return err
	}
	trust := client.NodeInstallReceiptTrust{PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Signer: client.NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: meta.SigningKeyID, Generation: meta.Generation, Fingerprint: fingerprint, PublicKey: identity.PublicKey}, BackupRoot: plan.Rollback.BackupRoot, VerifyNoSymlinkTraversal: i.verifyNoSymlink}
	if receipt.State != state {
		return errors.New("receipt state differs")
	}
	return client.VerifyNodeInstallReceipt(receipt, trust)
}

func (i *Installer) failRepairAndRollback(ctx context.Context, plan client.NodeInstallPlan, meta client.NodeEnrollmentIdentity, identity Identity, wal *InstallWAL, cause error) (client.NodeInstallReceipt, error) {
	if wal.OriginalReceipt == nil {
		return client.NodeInstallReceipt{}, cause
	}
	original := *wal.OriginalReceipt
	residues, rollbackErr := i.rollback(ctx, plan, wal)
	if rollbackErr != nil {
		return original, fmt.Errorf("%v; persist repair rollback WAL: %w", cause, rollbackErr)
	}
	if len(residues) > 0 {
		wal.Checkpoint = "repair_recovery_required"
		wal.Residues = append([]client.NodeReceiptResidue(nil), residues...)
		wal.UpdatedAt = nowString(i.now())
		if saveErr := i.state.SaveWAL(*wal); saveErr != nil {
			return original, fmt.Errorf("%v; persist repair recovery requirement: %w", cause, saveErr)
		}
		return original, fmt.Errorf("%v; repair rollback left restoration residues", cause)
	}
	if err := i.state.SaveReceipt(original); err != nil {
		return original, fmt.Errorf("%v; restore original active receipt: %w", cause, err)
	}
	if err := i.FinalizeServiceState(ctx, plan); err != nil {
		return original, fmt.Errorf("%v; finalize repaired service state: %w", cause, err)
	}
	if err := i.state.RemoveWAL(); err != nil {
		return original, fmt.Errorf("%v; remove repair WAL: %w", cause, err)
	}
	return original, cause
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
	residues := append([]client.NodeReceiptResidue(nil), wal.Residues...)
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
			residues = appendUniqueResidue(residues, client.NodeReceiptResidue{Target: entry.Target, ReasonCode: "mutation_missing", SafeMessage: "verified plan no longer contains the applied mutation"})
			wal.Residues = append([]client.NodeReceiptResidue(nil), residues...)
			wal.UpdatedAt = nowString(i.now())
			if err := i.state.SaveWAL(*wal); err != nil {
				return residues, err
			}
			continue
		}
		prior := PriorState{State: entry.PriorState, Material: entry.RollbackMaterial}
		if err := i.platform.Rollback(ctx, mutation, prior); err != nil {
			entry.Status = "residue"
			residues = appendUniqueResidue(residues, client.NodeReceiptResidue{Target: entry.Target, ReasonCode: "rollback_failed", SafeMessage: "platform rollback failed; manual recovery is required"})
		} else if entry.PriorState == "absent" {
			entry.Status = "removed"
		} else {
			entry.Status = "restored"
		}
		wal.UpdatedAt = nowString(i.now())
		wal.Residues = append([]client.NodeReceiptResidue(nil), residues...)
		if err := i.state.SaveWAL(*wal); err != nil {
			return residues, err
		}
	}
	if wal.Lifecycle == "" || wal.Lifecycle == "install" {
		if aborter, ok := i.platform.(interface {
			AbortIncompleteJoin(context.Context, client.NodeInstallPlan) error
		}); ok {
			if err := aborter.AbortIncompleteJoin(ctx, plan); err != nil {
				residues = appendUniqueResidue(residues, client.NodeReceiptResidue{Target: plan.Hostname, ReasonCode: "join_intent_recovery_required", SafeMessage: "an incomplete worker join intent could not be safely cleared"})
			}
		}
	}
	wal.Residues = append([]client.NodeReceiptResidue(nil), residues...)
	wal.UpdatedAt = nowString(i.now())
	if err := i.state.SaveWAL(*wal); err != nil {
		return residues, err
	}
	return residues, nil
}
func appendUniqueResidue(values []client.NodeReceiptResidue, value client.NodeReceiptResidue) []client.NodeReceiptResidue {
	for _, existing := range values {
		if sameJSON(existing, value) {
			return values
		}
	}
	return append(values, value)
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
