package sandboxcontroller

import (
	"context"
	"errors"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type SourceNetworkController interface {
	Prepare(context.Context, WorkItem, sandboxcontrol.AdmissionObservation) error
	Restrict(context.Context, WorkItem, sandboxcontrol.AdmissionObservation, sandboxio.SourceMaterializationReceipt) error
}

type KubernetesSourceRuntimeConfig struct {
	Network SourceNetworkController
	IO      *sandboxio.Controller
}

type KubernetesSourceRuntime struct {
	network SourceNetworkController
	io      *sandboxio.Controller
}

func NewKubernetesSourceRuntime(config KubernetesSourceRuntimeConfig) (*KubernetesSourceRuntime, error) {
	if config.Network == nil || config.IO == nil {
		return nil, errors.New("Kubernetes source runtime dependencies are required")
	}
	return &KubernetesSourceRuntime{network: config.Network, io: config.IO}, nil
}

func (r *KubernetesSourceRuntime) Prepare(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if err := validateSourceObservation(item, observation); err != nil {
		return backendFailure("source_identity_mismatch", "source bootstrap identity is invalid", false, true, err)
	}
	if err := r.network.Prepare(ctx, item, observation); err != nil {
		return backendFailure("source_network_unavailable", "source bootstrap network policy is unavailable", true, false, err)
	}
	return nil
}

func (r *KubernetesSourceRuntime) Materialize(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation) (sandboxio.SourceMaterializationReceipt, error) {
	if err := validateSourceObservation(item, observation); err != nil {
		return sandboxio.SourceMaterializationReceipt{}, backendFailure("source_identity_mismatch", "source bootstrap identity is invalid", false, true, err)
	}
	manifest := sourceManifest(item.Sources)
	receipt, err := r.io.Bootstrap(ctx, sourceTarget(observation), manifest)
	if err != nil {
		return sandboxio.SourceMaterializationReceipt{}, backendFailure("source_materialization_failed", "source materialization failed", true, false, err)
	}
	if err := sandboxio.ValidateSourceMaterializationReceipt(receipt, &manifest); err != nil {
		return sandboxio.SourceMaterializationReceipt{}, backendFailure("source_receipt_mismatch", "source materialization receipt changed", false, true, err)
	}
	return receipt, nil
}

func (r *KubernetesSourceRuntime) Restrict(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) error {
	if err := validateSourceReceipt(item, observation, receipt); err != nil {
		return backendFailure("source_receipt_mismatch", "persisted source materialization receipt changed", false, true, err)
	}
	if err := r.network.Restrict(ctx, item, observation, receipt); err != nil {
		return backendFailure("runtime_network_unavailable", "runtime default-deny policy is unavailable", true, false, err)
	}
	return nil
}

func (r *KubernetesSourceRuntime) Release(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) error {
	if err := validateSourceReceipt(item, observation, receipt); err != nil {
		return backendFailure("source_receipt_mismatch", "persisted source materialization receipt changed", false, true, err)
	}
	if err := r.io.Release(ctx, sourceTarget(observation), receipt.Digest); err != nil {
		return backendFailure("source_release_failed", "source bootstrap release failed", true, false, err)
	}
	return nil
}

func validateSourceReceipt(item WorkItem, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) error {
	if err := validateSourceObservation(item, observation); err != nil {
		return err
	}
	manifest := sourceManifest(item.Sources)
	return sandboxio.ValidateSourceMaterializationReceipt(receipt, &manifest)
}

func validateSourceObservation(item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if len(item.Sources) == 0 || sandboxcontrol.ValidateAdmissionObservation(observation) != nil ||
		observation.Sandbox.Name != item.SandboxID || observation.Sandbox.Namespace != sandboxcontrol.Namespace ||
		observation.Workload.WorkspaceID != item.WorkspaceID || observation.Workload.SandboxID != item.SandboxID ||
		observation.Pod.Namespace != sandboxcontrol.Namespace {
		return errors.New("source observation does not match the work item")
	}
	return nil
}

func sourceTarget(observation sandboxcontrol.AdmissionObservation) sandboxio.FrozenPodTarget {
	return sandboxio.FrozenPodTarget{Namespace: observation.Pod.Namespace, PodName: observation.Pod.Name,
		PodUID: observation.Pod.UID, SandboxUID: observation.Sandbox.UID, Container: sandboxio.BootstrapContainer}
}
