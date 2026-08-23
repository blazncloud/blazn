package sandboxcontroller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

var workerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var quantityPattern = regexp.MustCompile(`^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti)?$`)
var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type Controller struct {
	store   Store
	backend Backend
	config  Config
	now     func() time.Time
}

func New(store Store, backend Backend, config Config) (*Controller, error) {
	if store == nil || backend == nil {
		return nil, fmt.Errorf("sandbox controller dependencies are required")
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Controller{store: store, backend: backend, config: config, now: time.Now}, nil
}

func validateConfig(config Config) error {
	effectiveLease := config.Lease.Truncate(time.Second)
	if !workerPattern.MatchString(config.WorkerID) || config.Lease < 5*time.Second || config.Lease > 300*time.Second || config.RenewEvery <= 0 || config.RenewEvery > effectiveLease-time.Second || config.PollEvery <= 0 || config.OperationTimeout < config.PollEvery || config.IdleDelay <= 0 || config.RetryDelay < time.Second || config.RetryDelay > time.Hour || config.ExpiryEvery <= 0 || config.ExpiryBatch < 1 || config.ExpiryBatch > 100 {
		return fmt.Errorf("sandbox controller configuration is invalid")
	}
	return nil
}

func (c *Controller) Run(ctx context.Context) error {
	if err := c.store.Health(ctx); err != nil {
		return fmt.Errorf("sandbox controller store health: %w", err)
	}
	if err := c.backend.Health(ctx); err != nil {
		return fmt.Errorf("sandbox controller backend health: %w", err)
	}
	expiry := time.NewTicker(c.config.ExpiryEvery)
	defer expiry.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		select {
		case <-expiry.C:
			if _, err := c.store.EnqueueExpired(ctx, c.config.ExpiryBatch); err != nil && ctx.Err() == nil {
				return fmt.Errorf("sandbox expiry enqueue: %w", err)
			}
		default:
		}
		item, err := c.store.Claim(ctx, c.config.WorkerID, int(c.config.Lease/time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("sandbox claim: %w", err)
		}
		if item == nil {
			if !wait(ctx, c.config.IdleDelay) {
				return nil
			}
			continue
		}
		if err := c.reconcile(ctx, *item); err != nil && ctx.Err() == nil {
			return err
		}
	}
}

func (c *Controller) reconcile(parent context.Context, item WorkItem) error {
	if err := validateWorkItem(item); err != nil {
		return c.finishFailure(parent, item, &Failure{Code: "invalid_work_item", SafeMessage: "controller work item is invalid", Ambiguous: true, Cause: err})
	}
	ctx, cancel := context.WithTimeout(parent, c.config.OperationTimeout)
	defer cancel()
	lost := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go c.heartbeat(ctx, cancel, item, lost, heartbeatDone)
	err := c.execute(ctx, item)
	cancel()
	<-heartbeatDone
	select {
	case <-lost:
		return nil
	default:
	}
	if err == nil || parent.Err() != nil {
		return nil
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		failure = &Failure{Code: "backend_failure", SafeMessage: "sandbox backend operation failed", Retryable: true, Cause: err}
	}
	return c.finishFailure(parent, item, failure)
}

func (c *Controller) heartbeat(ctx context.Context, cancel context.CancelFunc, item WorkItem, lost chan<- struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(c.config.RenewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, ok, err := c.store.Renew(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, int(c.config.Lease/time.Second))
			if err != nil || !ok {
				close(lost)
				cancel()
				return
			}
		}
	}
}

func (c *Controller) execute(ctx context.Context, item WorkItem) error {
	switch item.OperationType {
	case "create":
		return c.create(ctx, item)
	case "stop", "delete":
		return c.cleanup(ctx, item)
	default:
		return &Failure{Code: "unsupported_operation", SafeMessage: "sandbox operation is unsupported", Ambiguous: true}
	}
}

func (c *Controller) create(ctx context.Context, item WorkItem) error {
	var state BackendState
	var err error
	if item.BackendUID == nil {
		state, err = c.backend.EnsureCreated(ctx, item)
	} else {
		state, err = c.backend.Observe(ctx, item)
	}
	if err != nil {
		return err
	}
	for !state.Ready || state.Admission == nil {
		if !wait(ctx, c.config.PollEvery) {
			return ctx.Err()
		}
		state, err = c.backend.Observe(ctx, item)
		if err != nil {
			return err
		}
	}
	if err := validateCreated(item, state); err != nil {
		return &Failure{Code: "backend_identity_mismatch", SafeMessage: "backend identity does not match work item", Ambiguous: true, Cause: err}
	}
	if item.BackendUID == nil {
		ok, err := c.store.BindBackend(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, state.Record, *state.Admission)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	} else if err := validateExisting(item, state); err != nil {
		return &Failure{Code: "backend_identity_mismatch", SafeMessage: "backend identity does not match persisted identity", Ambiguous: true, Cause: err}
	}
	digest := state.Admission.Digest
	uid, rv := state.Record.UID, state.Record.ResourceVersion
	ok, err := c.store.Complete(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, Completion{Status: "succeeded", ExpectedBackendUID: &uid, ExpectedBackendResourceVersion: &rv, ExpectedAdmissionDigest: &digest, ArtifactIDs: []string{}, WarningCodes: []string{}})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return nil
}

func (c *Controller) cleanup(ctx context.Context, item WorkItem) error {
	if item.BackendUID == nil || item.BackendResourceVersion == nil || item.Admission == nil {
		return &Failure{Code: "missing_backend_identity", SafeMessage: "cleanup lacks exact backend identity", Ambiguous: true}
	}
	state, err := c.backend.BeginDelete(ctx, item)
	if err != nil {
		return err
	}
	if state.Exists {
		if err := validateExisting(item, state); err != nil {
			return &Failure{Code: "backend_identity_mismatch", SafeMessage: "cleanup backend identity changed", Ambiguous: true, Cause: err}
		}
	}
	result, err := c.backend.Finalize(ctx, item, state)
	if err != nil {
		return err
	}
	if !result.CleanupComplete || !result.ArtifactExportComplete || !result.GrantsRevoked || !result.BackendDestroyed {
		return &Failure{Code: "cleanup_incomplete", SafeMessage: "sandbox cleanup is incomplete", Ambiguous: true}
	}
	uid, rv, digest := *item.BackendUID, *item.BackendResourceVersion, item.Admission.Digest
	ok, err := c.store.Complete(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, Completion{Status: "succeeded", ExpectedBackendUID: &uid, ExpectedBackendResourceVersion: &rv, ExpectedAdmissionDigest: &digest, CleanupComplete: true, ArtifactExportComplete: true, GrantsRevoked: result.GrantsRevoked, BackendDestroyed: true, ArtifactIDs: append([]string(nil), result.ArtifactIDs...), WarningCodes: append([]string(nil), result.WarningCodes...)})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return nil
}

func (c *Controller) finishFailure(ctx context.Context, item WorkItem, failure *Failure) error {
	safe := SafeError{Code: safeCode(failure.Code), Message: safeMessage(failure.SafeMessage), RequestID: newRequestID()}
	if failure.Ambiguous {
		completion := Completion{Status: "recovery_required", CleanupComplete: false, ArtifactExportComplete: false, GrantsRevoked: false, BackendDestroyed: false, ArtifactIDs: []string{}, WarningCodes: []string{}, Error: &safe}
		if item.BackendUID != nil {
			completion.ExpectedBackendUID = item.BackendUID
			completion.ExpectedBackendResourceVersion = item.BackendResourceVersion
			if item.Admission != nil {
				digest := item.Admission.Digest
				completion.ExpectedAdmissionDigest = &digest
			}
		}
		_, err := c.store.Complete(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, completion)
		return err
	}
	if failure.Retryable {
		_, err := c.store.Retry(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, int(c.config.RetryDelay/time.Second), safe)
		return err
	}
	completion := Completion{Status: "failed", ArtifactIDs: []string{}, WarningCodes: []string{}, Error: &safe}
	_, err := c.store.Complete(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, completion)
	return err
}

func validateWorkItem(item WorkItem) error {
	if item.AllocationMode != "direct" || item.OperationID == "" || item.WorkspaceID == "" || item.SandboxID == "" || item.RequestedBy == "" || item.LeaseToken == "" || item.QueueName != sandboxcontrol.QueueName || item.ExpiresAt.IsZero() || item.ExpectedSandboxVersion < 1 || item.Attempt < 1 || len(item.Command) == 0 || len(item.Command) > 32 {
		return fmt.Errorf("missing immutable work item fields")
	}
	if item.TemplateVersionID == "" || !sha256Pattern.MatchString(item.TemplateDigest) ||
		!sha256Pattern.MatchString(item.ImageIndexDigest) || !sha256Pattern.MatchString(item.ImageDigest) ||
		item.VariantName == "" {
		return fmt.Errorf("immutable image identity is invalid")
	}
	if item.Architecture == "amd64" && item.PlacementProfile != "poc-linux-amd64-v1" ||
		item.Architecture == "arm64" && item.PlacementProfile != "poc-mac-arm64-v1" ||
		item.Architecture != "amd64" && item.Architecture != "arm64" {
		return fmt.Errorf("placement identity is invalid")
	}
	for _, argument := range item.Command {
		if argument == "" || len(argument) > 1024 || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("command argument is invalid")
		}
	}
	for _, quantity := range []string{item.Resources.CPURequest, item.Resources.MemoryRequest,
		item.Resources.EphemeralRequest, item.Resources.CPULimit, item.Resources.MemoryLimit,
		item.Resources.EphemeralLimit} {
		if !quantityPattern.MatchString(quantity) {
			return fmt.Errorf("resource quantity is invalid")
		}
	}
	for _, source := range item.Sources {
		if source.Name == "" || source.URL == "" || source.Destination == "" || !commitPattern.MatchString(source.Commit) {
			return fmt.Errorf("source identity is invalid")
		}
	}
	for _, artifact := range item.Artifacts {
		if artifact.Name == "" || artifact.Path == "" || artifact.MediaType == "" {
			return fmt.Errorf("artifact contract is invalid")
		}
	}
	if item.OperationType == "create" && item.DesiredState != "ready" || item.OperationType == "stop" && item.DesiredState != "stopped" || item.OperationType == "delete" && item.DesiredState != "deleted" {
		return fmt.Errorf("operation desired state mismatch")
	}
	backendParts := 0
	for _, value := range []*string{item.BackendUID, item.BackendResourceVersion} {
		if value != nil && *value != "" {
			backendParts++
		}
	}
	if item.Admission != nil {
		backendParts++
	}
	if backendParts != 0 && backendParts != 3 {
		return fmt.Errorf("persisted backend identity is incomplete")
	}
	if (item.AdmissionID == nil) != (item.Admission == nil) || item.Admission != nil && *item.AdmissionID != item.Admission.UID {
		return fmt.Errorf("persisted admission identity is inconsistent")
	}
	return nil
}
func validateCreated(item WorkItem, state BackendState) error {
	if !state.Exists || state.Record.Name != item.SandboxID || state.Record.Namespace != sandboxcontrol.Namespace || state.Record.WorkspaceID != item.WorkspaceID || state.Record.OwnerID != item.RequestedBy || state.Record.UID == "" || state.Record.ResourceVersion == "" || state.Admission == nil {
		return fmt.Errorf("created identity incomplete")
	}
	receipt, err := sandboxcontrol.NewReceipt("controller-"+item.OperationID, sandboxcontrol.OperationCreate, state.Record, nil, time.Unix(1, 0))
	if err != nil {
		return err
	}
	bound, err := sandboxcontrol.AttachAdmissionIdentity(receipt, *state.Admission)
	if err != nil {
		return err
	}
	return sandboxcontrol.ValidateTerminalCreateReceipt(bound)
}
func validateExisting(item WorkItem, state BackendState) error {
	if state.Record.UID != *item.BackendUID || state.Record.ResourceVersion != *item.BackendResourceVersion ||
		state.Record.Name != item.SandboxID || state.Record.Namespace != sandboxcontrol.Namespace ||
		state.Record.WorkspaceID != item.WorkspaceID || state.Record.OwnerID != item.RequestedBy ||
		state.Record.QueueName != sandboxcontrol.QueueName || state.Admission == nil ||
		state.Admission.Digest != item.Admission.Digest {
		return fmt.Errorf("existing backend tuple mismatch")
	}
	validationState := state
	validationState.Record.State = sandboxcontrol.StateReady
	if err := validateCreated(item, validationState); err != nil {
		return fmt.Errorf("existing backend admission identity is invalid: %w", err)
	}
	return nil
}
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func safeCode(value string) string {
	if regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`).MatchString(value) {
		return value
	}
	return "backend_failure"
}
func safeMessage(value string) string {
	if len(value) > 0 && len(value) <= 512 {
		return value
	}
	return "sandbox backend operation failed"
}
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
