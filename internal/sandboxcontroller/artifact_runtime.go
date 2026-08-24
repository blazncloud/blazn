package sandboxcontroller

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type ArtifactObjectStore interface {
	Put(context.Context, ArtifactObjectSpec, []byte) (bool, error)
	Head(context.Context, ArtifactObjectSpec) (ArtifactObjectHead, bool, error)
}

type KubernetesArtifactRuntimeConfig struct {
	IO      *sandboxio.Controller
	Objects ArtifactObjectStore
}

type ArtifactExportResult struct {
	Artifacts    []PersistedArtifact
	WarningCodes []string
}

type KubernetesArtifactRuntime struct {
	io      *sandboxio.Controller
	objects ArtifactObjectStore
}

func NewKubernetesArtifactRuntime(config KubernetesArtifactRuntimeConfig) (*KubernetesArtifactRuntime, error) {
	if config.IO == nil || config.Objects == nil {
		return nil, errors.New("Kubernetes artifact runtime dependencies are required")
	}
	return &KubernetesArtifactRuntime{io: config.IO, objects: config.Objects}, nil
}

func (r *KubernetesArtifactRuntime) Export(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation) (ArtifactExportResult, error) {
	if err := validateArtifactObservation(item, observation); err != nil {
		return ArtifactExportResult{}, backendFailure("artifact_identity_mismatch", "artifact export identity is invalid", false, true, err)
	}
	existing := make(map[string]PersistedArtifact, len(item.PersistedArtifacts))
	for _, artifact := range item.PersistedArtifacts {
		if _, duplicate := existing[artifact.Name]; duplicate {
			return ArtifactExportResult{}, backendFailure("artifact_identity_mismatch", "persisted artifact identity is duplicated", false, true, nil)
		}
		existing[artifact.Name] = artifact
	}
	contracts := append([]Artifact(nil), item.Artifacts...)
	sort.Slice(contracts, func(i, j int) bool { return contracts[i].Name < contracts[j].Name })
	result := ArtifactExportResult{Artifacts: make([]PersistedArtifact, 0, len(contracts)), WarningCodes: []string{}}
	for _, contract := range contracts {
		if persisted, ok := existing[contract.Name]; ok {
			if err := validatePersistedArtifact(item, contract, persisted); err != nil {
				return ArtifactExportResult{}, backendFailure("artifact_identity_mismatch", "persisted artifact differs from its contract", false, true, err)
			}
			spec := artifactObjectSpec(item, persisted)
			head, found, err := r.objects.Head(ctx, spec)
			if err != nil || !found || !sameArtifactHead(spec, head) {
				return ArtifactExportResult{}, backendFailure("artifact_object_unavailable", "persisted artifact object cannot be verified", true, !found, err)
			}
			result.Artifacts = append(result.Artifacts, persisted)
			continue
		}
		target := sandboxio.FrozenPodTarget{Namespace: observation.Pod.Namespace, PodName: observation.Pod.Name,
			PodUID: observation.Pod.UID, SandboxUID: observation.Sandbox.UID, Container: sandboxio.ArtifactContainer}
		artifact, err := r.io.ReadArtifact(ctx, target, contract.Path)
		if err != nil {
			if sandboxio.IsProtocolError(err, "artifact_not_found") && !contract.Required {
				result.WarningCodes = append(result.WarningCodes, "optional_artifact_missing:"+contract.Name)
				continue
			}
			return ArtifactExportResult{}, backendFailure("artifact_read_failed", "Sandbox artifact cannot be read safely", true, false, err)
		}
		key, err := ArtifactObjectKey(item.WorkspaceID, item.SandboxID, contract.Name)
		if err != nil {
			return ArtifactExportResult{}, backendFailure("artifact_identity_mismatch", "artifact object identity is invalid", false, true, err)
		}
		persisted := PersistedArtifact{Name: contract.Name, Path: contract.Path, MediaType: contract.MediaType,
			Digest: artifact.SHA256, Size: artifact.Size, ObjectKey: key}
		spec := artifactObjectSpec(item, persisted)
		if _, err := r.objects.Put(ctx, spec, artifact.Body); err != nil {
			return ArtifactExportResult{}, backendFailure("artifact_object_write_failed", "artifact object cannot be stored", true, false, err)
		}
		head, found, err := r.objects.Head(ctx, spec)
		if err != nil || !found || !sameArtifactHead(spec, head) {
			return ArtifactExportResult{}, backendFailure("artifact_object_mismatch", "artifact object verification failed", true, !found, err)
		}
		result.Artifacts = append(result.Artifacts, persisted)
	}
	return result, nil
}

func validateArtifactObservation(item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if item.BackendUID == nil || item.BackendResourceVersion == nil || item.AdmissionObservation == nil ||
		sandboxcontrol.ValidateAdmissionObservation(observation) != nil || observation.Sandbox.UID != *item.BackendUID ||
		observation.Sandbox.ResourceVersion != *item.BackendResourceVersion || observation.Digest != item.AdmissionObservation.Digest ||
		observation.Pod.UID != item.AdmissionObservation.Pod.UID || observation.Workload.Digest != item.AdmissionObservation.Workload.Digest {
		return errors.New("artifact observation differs from persisted admission")
	}
	return nil
}

func validatePersistedArtifact(item WorkItem, contract Artifact, artifact PersistedArtifact) error {
	key, err := ArtifactObjectKey(item.WorkspaceID, item.SandboxID, contract.Name)
	if err != nil || !canonicalUUID(artifact.ID) || artifact.Name != contract.Name || artifact.Path != contract.Path ||
		artifact.MediaType != contract.MediaType || !sha256Pattern.MatchString(artifact.Digest) || artifact.Size < 0 ||
		artifact.Size > maxArtifactBytes || artifact.ObjectKey != key || !validExportedAt(artifact.ExportedAt) {
		return errors.New("persisted artifact identity is invalid")
	}
	return nil
}

func validExportedAt(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.UTC().Format(time.RFC3339Nano) == value
}

func artifactObjectSpec(item WorkItem, artifact PersistedArtifact) ArtifactObjectSpec {
	return ArtifactObjectSpec{Key: artifact.ObjectKey, WorkspaceID: item.WorkspaceID, SandboxID: item.SandboxID,
		Name: artifact.Name, MediaType: artifact.MediaType, Digest: artifact.Digest, Size: artifact.Size}
}

func sameArtifactHead(spec ArtifactObjectSpec, head ArtifactObjectHead) bool {
	return head.Size == spec.Size && head.DigestSize == spec.Size && head.MediaType == spec.MediaType && head.Digest == spec.Digest &&
		head.WorkspaceID == spec.WorkspaceID && head.SandboxID == spec.SandboxID && head.Name == spec.Name
}
