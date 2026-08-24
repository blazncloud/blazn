package sandboxcontroller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

var workerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var quantityPattern = regexp.MustCompile(`^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti)?$`)
var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var immutableImagePattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}$`)
var sourceNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

const defaultLeaseSafetyMargin = time.Second

type Controller struct {
	store             Store
	backend           Backend
	config            Config
	leaseSafetyMargin time.Duration
}

type heartbeatKind uint8

const (
	heartbeatStopped heartbeatKind = iota
	heartbeatLeaseLost
	heartbeatStoreError
)

type heartbeatResult struct {
	kind heartbeatKind
	err  error
}

type renewResult struct {
	window LeaseWindow
	ok     bool
	err    error
}

func New(store Store, backend Backend, config Config) (*Controller, error) {
	if store == nil || backend == nil {
		return nil, fmt.Errorf("sandbox controller dependencies are required")
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Controller{store: store, backend: backend, config: config,
		leaseSafetyMargin: defaultLeaseSafetyMargin}, nil
}

func validateConfig(config Config) error {
	effectiveLease := config.Lease.Truncate(time.Second)
	if !workerPattern.MatchString(config.WorkerID) || config.Lease < 5*time.Second || config.Lease > 300*time.Second || config.RenewEvery <= 0 || config.RenewEvery >= effectiveLease-defaultLeaseSafetyMargin || config.PollEvery <= 0 || config.OperationTimeout < config.PollEvery || config.IdleDelay <= 0 || config.RetryDelay < time.Second || config.RetryDelay > time.Hour || config.ExpiryEvery <= 0 || config.ExpiryBatch < 1 || config.ExpiryBatch > 100 {
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
	if !c.leaseCoversNextRenew(item.LeaseDeadline) {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, c.config.OperationTimeout)
	defer cancel()
	heartbeatDone := make(chan heartbeatResult, 1)
	go c.heartbeat(ctx, cancel, item, heartbeatDone)
	err := c.execute(ctx, item)
	cancel()
	heartbeat := <-heartbeatDone
	switch heartbeat.kind {
	case heartbeatLeaseLost:
		return nil
	case heartbeatStoreError:
		return fmt.Errorf("sandbox lease renewal failed: %w", heartbeat.err)
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

func (c *Controller) heartbeat(ctx context.Context, cancel context.CancelFunc, item WorkItem, done chan<- heartbeatResult) {
	ticker := time.NewTicker(c.config.RenewEvery)
	defer ticker.Stop()
	watchdog := time.NewTimer(c.leaseSafetyDelay(item.LeaseDeadline))
	defer watchdog.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- heartbeatResult{kind: heartbeatStopped}
			return
		case <-watchdog.C:
			done <- heartbeatResult{kind: heartbeatLeaseLost}
			cancel()
			return
		case <-ticker.C:
			result := make(chan renewResult, 1)
			renewCtx, renewCancel := context.WithTimeout(ctx, c.leaseSafetyDelay(item.LeaseDeadline))
			go func() {
				window, ok, err := c.store.Renew(renewCtx, item.OperationID, c.config.WorkerID,
					item.LeaseToken, int(c.config.Lease/time.Second))
				result <- renewResult{window: window, ok: ok, err: err}
			}()
			select {
			case <-ctx.Done():
				renewCancel()
				done <- heartbeatResult{kind: heartbeatStopped}
				return
			case <-watchdog.C:
				renewCancel()
				done <- heartbeatResult{kind: heartbeatLeaseLost}
				cancel()
				return
			case renewed := <-result:
				renewContextErr := renewCtx.Err()
				renewCancel()
				if renewed.err != nil {
					if ctx.Err() != nil {
						done <- heartbeatResult{kind: heartbeatStopped}
						return
					}
					if renewContextErr != nil {
						done <- heartbeatResult{kind: heartbeatLeaseLost}
					} else {
						done <- heartbeatResult{kind: heartbeatStoreError, err: renewed.err}
					}
					cancel()
					return
				}
				if !renewed.ok || !c.leaseCoversNextRenew(renewed.window.Deadline) {
					done <- heartbeatResult{kind: heartbeatLeaseLost}
					cancel()
					return
				}
				item.LeaseExpiresAt = renewed.window.ExpiresAt
				item.LeaseRemaining = renewed.window.Remaining
				item.LeaseDeadline = renewed.window.Deadline
				resetTimer(watchdog, c.leaseSafetyDelay(item.LeaseDeadline))
			}
		}
	}
}

func (c *Controller) leaseCoversNextRenew(deadline time.Time) bool {
	return !deadline.IsZero() && c.leaseSafetyDelay(deadline) > c.config.RenewEvery
}

func (c *Controller) leaseSafetyDelay(deadline time.Time) time.Duration {
	return time.Until(deadline) - c.leaseSafetyMargin
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
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
	expectedObservation := item.AdmissionObservation
	if item.BackendUID == nil && item.SourceMaterialization != nil {
		expectedObservation = item.SourceBootstrapObservation
	}
	if item.BackendUID == nil && item.SourceMaterialization == nil {
		state, err = c.backend.EnsureCreated(ctx, item)
	} else {
		state, err = c.backend.Observe(ctx, item, expectedObservation)
	}
	if err != nil {
		return err
	}
	for state.AdmissionObservation == nil || len(item.Sources) == 0 && !state.Ready {
		if !wait(ctx, c.config.PollEvery) {
			return ctx.Err()
		}
		state, err = c.backend.Observe(ctx, item, item.AdmissionObservation)
		if err != nil {
			return err
		}
	}
	if err := validateObserved(item, state); err != nil {
		return &Failure{Code: "backend_identity_mismatch", SafeMessage: "backend identity does not match work item", Ambiguous: true, Cause: err}
	}
	if len(item.Sources) == 0 && item.BackendUID == nil {
		ok, err := c.store.BindBackend(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, *state.AdmissionObservation)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	} else if len(item.Sources) == 0 && item.BackendUID != nil {
		if err := validateExisting(item, state); err != nil {
			return &Failure{Code: "backend_identity_mismatch", SafeMessage: "backend identity does not match persisted identity", Ambiguous: true, Cause: err}
		}
	} else if item.BackendUID != nil {
		if err := validateExisting(item, state); err != nil {
			return &Failure{Code: "backend_identity_mismatch", SafeMessage: "backend identity does not match persisted identity", Ambiguous: true, Cause: err}
		}
	}
	if len(item.Sources) != 0 {
		sourceBackend, ok := c.backend.(SourceBackend)
		if !ok {
			return &Failure{Code: "sources_unsupported", SafeMessage: "source materialization runtime is unavailable", Ambiguous: true}
		}
		receipt := item.SourceMaterialization
		if receipt == nil {
			if state.Ready {
				return &Failure{Code: "source_receipt_missing", SafeMessage: "backend became ready without persisted source evidence", Ambiguous: true}
			}
			if err := sourceBackend.PrepareSourceBootstrap(ctx, item, *state.AdmissionObservation); err != nil {
				return err
			}
			materialized, err := sourceBackend.MaterializeSources(ctx, item, *state.AdmissionObservation)
			if err != nil {
				return err
			}
			recorded, err := c.store.RecordSources(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, *state.AdmissionObservation, materialized)
			if err != nil {
				return err
			}
			if !recorded {
				return nil
			}
			receipt = &materialized
			item.SourceBootstrapObservation = state.AdmissionObservation
		}
		if err := sourceBackend.RestrictSourceRuntime(ctx, item, *state.AdmissionObservation, *receipt); err != nil {
			return err
		}
		if !state.Ready {
			if err := sourceBackend.ReleaseSources(ctx, item, *state.AdmissionObservation, *receipt); err != nil {
				return err
			}
		}
		for !state.Ready {
			if !wait(ctx, c.config.PollEvery) {
				return ctx.Err()
			}
			state, err = c.backend.Observe(ctx, item, item.SourceBootstrapObservation)
			if err != nil {
				return err
			}
		}
	}
	if err := validateCreated(item, state); err != nil {
		return &Failure{Code: "backend_identity_mismatch", SafeMessage: "ready backend identity does not match work item", Ambiguous: true, Cause: err}
	}
	if len(item.Sources) != 0 && item.BackendUID == nil {
		ok, err := c.store.BindBackend(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, *state.AdmissionObservation)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	workloadDigest := state.AdmissionObservation.Workload.Digest
	observationDigest := state.AdmissionObservation.Digest
	uid, rv := state.Record.UID, state.Record.ResourceVersion
	ok, err := c.store.Complete(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, Completion{Status: "succeeded", ExpectedBackendUID: &uid, ExpectedBackendResourceVersion: &rv, ExpectedWorkloadDigest: &workloadDigest, ExpectedObservationDigest: &observationDigest, ArtifactIDs: []string{}, WarningCodes: []string{}})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return nil
}

func validateObserved(item WorkItem, state BackendState) error {
	if !state.Exists || state.Record.Name != item.SandboxID || state.Record.Namespace != sandboxcontrol.Namespace ||
		state.Record.WorkspaceID != item.WorkspaceID || state.Record.OwnerID != item.RequestedBy ||
		state.Record.UID == "" || state.Record.ResourceVersion == "" || state.AdmissionObservation == nil {
		return fmt.Errorf("observed identity incomplete")
	}
	if err := sandboxcontrol.ValidateAdmissionObservation(*state.AdmissionObservation); err != nil ||
		state.AdmissionObservation.Sandbox.UID != state.Record.UID ||
		state.AdmissionObservation.Sandbox.ResourceVersion != state.Record.ResourceVersion {
		return fmt.Errorf("observed admission identity is invalid")
	}
	return nil
}

func (c *Controller) cleanup(ctx context.Context, item WorkItem) error {
	if item.BackendUID == nil || item.BackendResourceVersion == nil || item.AdmissionObservation == nil {
		return &Failure{Code: "missing_backend_identity", SafeMessage: "cleanup lacks exact backend identity", Ambiguous: true}
	}
	state, err := c.backend.BeginDelete(ctx, item, item.AdmissionObservation)
	if err != nil {
		return err
	}
	if state.Exists {
		if err := validateDeleting(item, state); err != nil {
			return &Failure{Code: "backend_identity_mismatch", SafeMessage: "cleanup backend identity changed", Ambiguous: true, Cause: err}
		}
	}
	result, err := c.backend.Finalize(ctx, item, state, item.AdmissionObservation)
	if err != nil {
		return err
	}
	if !result.CleanupComplete || !result.ArtifactExportComplete || !result.GrantsRevoked || !result.BackendDestroyed {
		return &Failure{Code: "cleanup_incomplete", SafeMessage: "sandbox cleanup is incomplete", Ambiguous: true}
	}
	uid, rv := *item.BackendUID, *item.BackendResourceVersion
	workloadDigest, observationDigest := item.AdmissionObservation.Workload.Digest, item.AdmissionObservation.Digest
	ok, err := c.store.Complete(ctx, item.OperationID, c.config.WorkerID, item.LeaseToken, Completion{Status: "succeeded", ExpectedBackendUID: &uid, ExpectedBackendResourceVersion: &rv, ExpectedWorkloadDigest: &workloadDigest, ExpectedObservationDigest: &observationDigest, CleanupComplete: true, ArtifactExportComplete: true, GrantsRevoked: result.GrantsRevoked, BackendDestroyed: true, ArtifactIDs: append([]string(nil), result.ArtifactIDs...), WarningCodes: append([]string(nil), result.WarningCodes...)})
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
			if item.PersistedWorkloadDigest != nil {
				completion.ExpectedWorkloadDigest = item.PersistedWorkloadDigest
			}
			if item.AdmissionObservation != nil {
				digest := item.AdmissionObservation.Digest
				completion.ExpectedObservationDigest = &digest
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
		!immutableImagePattern.MatchString(item.ImageIndexDigest) || !immutableImagePattern.MatchString(item.ImageDigest) ||
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
	seenSourceNames, seenSourceDestinations := map[string]bool{}, map[string]bool{}
	for _, source := range item.Sources {
		if !validControllerSource(source) || seenSourceNames[source.Name] || seenSourceDestinations[source.Destination] {
			return fmt.Errorf("source identity is invalid")
		}
		seenSourceNames[source.Name], seenSourceDestinations[source.Destination] = true, true
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
	for _, value := range []*string{item.BackendUID, item.BackendResourceVersion, item.AdmissionID, item.PersistedWorkloadDigest} {
		if value != nil && *value != "" {
			backendParts++
		}
	}
	if backendParts != 0 && backendParts != 4 {
		return fmt.Errorf("persisted backend identity is incomplete")
	}
	if backendParts == 0 && item.AdmissionObservation != nil {
		return fmt.Errorf("persisted admission observation has no backend identity")
	}
	if backendParts == 4 && item.AdmissionObservation == nil {
		return fmt.Errorf("legacy admission observation is unavailable")
	}
	if item.AdmissionObservation != nil && (*item.AdmissionID != item.AdmissionObservation.Workload.UID ||
		*item.PersistedWorkloadDigest != item.AdmissionObservation.Workload.Digest ||
		*item.BackendUID != item.AdmissionObservation.Sandbox.UID ||
		*item.BackendResourceVersion != item.AdmissionObservation.Sandbox.ResourceVersion ||
		sandboxcontrol.ValidateAdmissionObservation(*item.AdmissionObservation) != nil) {
		return fmt.Errorf("persisted admission identity is inconsistent")
	}
	if item.SourceMaterialization != nil {
		manifest := sourceManifest(item.Sources)
		if len(item.Sources) == 0 || sandboxio.ValidateSourceMaterializationReceipt(*item.SourceMaterialization, &manifest) != nil ||
			item.BackendUID == nil && item.SourceBootstrapObservation == nil {
			return fmt.Errorf("persisted source materialization is inconsistent")
		}
	}
	if item.SourceBootstrapObservation != nil {
		if item.SourceMaterialization == nil || sandboxcontrol.ValidateAdmissionObservation(*item.SourceBootstrapObservation) != nil ||
			item.SourceBootstrapObservation.Sandbox.Name != item.SandboxID || item.SourceBootstrapObservation.Workload.WorkspaceID != item.WorkspaceID {
			return fmt.Errorf("persisted source bootstrap observation is inconsistent")
		}
	}
	return nil
}

func validControllerSource(source Source) bool {
	if !sourceNamePattern.MatchString(source.Name) || !commitPattern.MatchString(source.Commit) ||
		len(source.URL) > 2048 || strings.ContainsAny(source.URL, "\\\x00\r\n\t") ||
		len(source.Destination) > 512 || !strings.HasPrefix(source.Destination, "/workspace/src/") ||
		path.Clean(source.Destination) != source.Destination || strings.Contains(source.Destination, "//") || strings.Contains(source.Destination, `\`) {
		return false
	}
	parsed, err := url.Parse(source.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Host == "" || parsed.Path == "" || path.Clean(parsed.Path) != parsed.Path || strings.ContainsAny(parsed.Path, "\\\x00\r\n\t") {
		return false
	}
	hostname := parsed.Hostname()
	if hostname == "" || hostname != strings.ToLower(hostname) || strings.HasSuffix(hostname, ".") || !validSourceHostname(hostname) {
		return false
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return false
		}
	}
	authority := hostname
	if strings.Contains(hostname, ":") {
		authority = "[" + hostname + "]"
	}
	if port != "" {
		authority += ":" + port
	}
	return parsed.Host == authority && parsed.String() == source.URL
}

func validSourceHostname(hostname string) bool {
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.String() == hostname
	}
	if strings.Trim(hostname, "0123456789.") == "" {
		return false
	}
	if len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if !sourceNamePattern.MatchString(label) {
			return false
		}
	}
	return true
}
func validateCreated(item WorkItem, state BackendState) error {
	if !state.Ready {
		return fmt.Errorf("created backend is not ready")
	}
	if err := validateObserved(item, state); err != nil {
		return err
	}
	receipt, err := sandboxcontrol.NewReceipt("controller-"+item.OperationID, sandboxcontrol.OperationCreate, state.Record, nil, time.Unix(1, 0))
	if err != nil {
		return err
	}
	bound, err := sandboxcontrol.AttachAdmissionIdentity(receipt, state.AdmissionObservation.Workload)
	if err != nil {
		return err
	}
	return sandboxcontrol.ValidateTerminalCreateReceipt(bound)
}
func validateExisting(item WorkItem, state BackendState) error {
	if state.Record.UID != *item.BackendUID || state.Record.ResourceVersion != *item.BackendResourceVersion ||
		state.Record.Name != item.SandboxID || state.Record.Namespace != sandboxcontrol.Namespace ||
		state.Record.WorkspaceID != item.WorkspaceID || state.Record.OwnerID != item.RequestedBy ||
		state.Record.QueueName != sandboxcontrol.QueueName || state.AdmissionObservation == nil ||
		item.AdmissionObservation == nil || !reflect.DeepEqual(*state.AdmissionObservation, *item.AdmissionObservation) {
		return fmt.Errorf("existing backend tuple mismatch")
	}
	validationState := state
	validationState.Record.State = sandboxcontrol.StateReady
	if err := validateCreated(item, validationState); err != nil {
		return fmt.Errorf("existing backend admission identity is invalid: %w", err)
	}
	return nil
}

func validateDeleting(item WorkItem, state BackendState) error {
	if item.BackendUID == nil || item.BackendResourceVersion == nil || item.AdmissionObservation == nil ||
		!state.Exists || !state.Deleting || !state.Record.Deleting || !state.CleanupFinalizerPresent ||
		state.Record.UID != *item.BackendUID || state.Record.ResourceVersion == "" ||
		state.Record.ResourceVersion == *item.BackendResourceVersion || state.AdmissionObservation == nil ||
		!reflect.DeepEqual(*state.AdmissionObservation, *item.AdmissionObservation) {
		return fmt.Errorf("deleting backend tuple mismatch")
	}
	if err := sandboxcontrol.ValidateAdmissionObservation(*state.AdmissionObservation); err != nil ||
		state.Record.Name != item.SandboxID || state.Record.Namespace != sandboxcontrol.Namespace ||
		state.Record.WorkspaceID != item.WorkspaceID || state.Record.OwnerID != item.RequestedBy ||
		state.Record.QueueName != sandboxcontrol.QueueName {
		return fmt.Errorf("deleting backend identity is invalid: %w", err)
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
