package sandboxcontrol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
)

const (
	podAPIVersion = "v1"
	podKind       = "Pod"
	workloadKind  = "Workload"
)

// ObjectIdentity freezes the apiserver identity of one ownership-chain hop.
type ObjectIdentity struct {
	APIVersion      string `json:"apiVersion"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

// AdmissionObservation proves exactly one admitted Workload -> Pod -> Sandbox
// controller-owner chain. Pod identity is retained even though the terminal
// receipt exposes the transitive Sandbox owner through WorkloadIdentity.
type AdmissionObservation struct {
	Sandbox  ObjectIdentity   `json:"sandbox"`
	Pod      ObjectIdentity   `json:"pod"`
	Workload WorkloadIdentity `json:"workload"`
}

type kubeOwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type observedMetadata struct {
	Name            string               `json:"name"`
	Namespace       string               `json:"namespace"`
	UID             string               `json:"uid"`
	ResourceVersion string               `json:"resourceVersion"`
	Labels          map[string]string    `json:"labels"`
	OwnerReferences []kubeOwnerReference `json:"ownerReferences"`
}

type observedPod struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   observedMetadata `json:"metadata"`
	Spec       kubePodSpec      `json:"spec"`
}

type observedPodList struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Items      []observedPod `json:"items"`
}

type observedWorkload struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   observedMetadata `json:"metadata"`
	Status     struct {
		Admission *struct {
			ClusterQueue string `json:"clusterQueue"`
		} `json:"admission"`
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type observedWorkloadList struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Items      []observedWorkload `json:"items"`
}

// ObserveAdmission validates current API objects against the canonical create
// request and freezes the exact resourceVersions. Supplying expected detects a
// later UID, resourceVersion, or API substitution instead of silently rebinding.
func (a *Adapter) ObserveAdmission(ctx context.Context, request CreateRequest, record SandboxRecord, expected *AdmissionObservation) (AdmissionObservation, error) {
	if err := ValidateCreate(request, a.runtimes); err != nil {
		return AdmissionObservation{}, err
	}
	canonicalArtifacts, artifactDigest, err := CanonicalArtifactContract(request.Artifacts)
	if err != nil {
		return AdmissionObservation{}, err
	}
	request.Artifacts = canonicalArtifacts
	intentDigest, err := createIntentDigest(request)
	if err != nil {
		return AdmissionObservation{}, err
	}
	manifest := render(request, artifactDigest, intentDigest)
	var sandbox kubeSandbox
	if err := a.call(ctx, http.MethodGet, a.resourcePath(request.Name), nil, nil, &sandbox, ""); err != nil {
		return AdmissionObservation{}, err
	}
	live, _, err := a.finishEnsureCreated(ctx, request, artifactDigest, intentDigest, record.UID, manifest, sandbox, false)
	if err != nil {
		return AdmissionObservation{}, err
	}
	if record.CreateIntentDigest != "" && live.CreateIntentDigest != record.CreateIntentDigest {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "Sandbox create intent changed after creation", nil)
	}

	query := url.Values{"labelSelector": {admissionSelector(request.WorkspaceID, request.OwnerID, request.Name)}}
	var pods observedPodList
	if err := a.call(ctx, http.MethodGet, a.podCollectionPath(), query, nil, &pods, ""); err != nil {
		return AdmissionObservation{}, err
	}
	if pods.APIVersion != podAPIVersion || pods.Kind != "PodList" || len(pods.Items) != 1 {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission requires exactly one API-stable owned Pod", nil)
	}
	pod := pods.Items[0]
	if !validObservedIdentity(pod.APIVersion, pod.Kind, pod.Metadata) || pod.APIVersion != podAPIVersion || pod.Kind != podKind ||
		!hasExactControllerOwner(pod.Metadata.OwnerReferences, APIVersion, Kind, request.Name, record.UID) ||
		!sameJSON(pod.Spec, manifest.Spec.PodTemplate.Spec) || !hasAdmissionLabels(pod.Metadata.Labels, request.WorkspaceID, request.OwnerID, request.Name) {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Pod substituted the Sandbox owner or material spec", nil)
	}

	var workloads observedWorkloadList
	if err := a.call(ctx, http.MethodGet, a.workloadCollectionPath(), query, nil, &workloads, ""); err != nil {
		return AdmissionObservation{}, err
	}
	if workloads.APIVersion != AdmissionAPIVersion || workloads.Kind != "WorkloadList" || len(workloads.Items) != 1 {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission requires exactly one API-stable Workload", nil)
	}
	workload := workloads.Items[0]
	condition, admitted := exactAdmittedCondition(workload.Status.Conditions)
	if !validObservedIdentity(workload.APIVersion, workload.Kind, workload.Metadata) || workload.APIVersion != AdmissionAPIVersion || workload.Kind != workloadKind ||
		!hasExactControllerOwner(workload.Metadata.OwnerReferences, podAPIVersion, podKind, pod.Metadata.Name, pod.Metadata.UID) ||
		!hasAdmissionLabels(workload.Metadata.Labels, request.WorkspaceID, request.OwnerID, request.Name) || workload.Status.Admission == nil ||
		!dnsNamePattern.MatchString(workload.Status.Admission.ClusterQueue) || !admitted {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "Workload did not preserve the exact admitted Pod ownership chain", nil)
	}

	identity := WorkloadIdentity{
		APIVersion: workload.APIVersion, Namespace: workload.Metadata.Namespace, Name: workload.Metadata.Name,
		UID: workload.Metadata.UID, ResourceVersion: workload.Metadata.ResourceVersion,
		ClusterQueue: workload.Status.Admission.ClusterQueue,
		Owner:        SandboxOwnerReference{APIVersion: APIVersion, Kind: Kind, Name: request.Name, UID: record.UID, Controller: true},
		WorkspaceID:  request.WorkspaceID, SandboxID: request.Name, Admitted: true,
		Condition: AdmissionCondition{Type: condition.Type, Status: condition.Status},
	}
	identity.Digest = workloadIdentityDigest(identity)
	observation := AdmissionObservation{
		Sandbox:  objectIdentity(sandbox.APIVersion, sandbox.Kind, sandbox.Metadata.Name, sandbox.Metadata.Namespace, sandbox.Metadata.UID, sandbox.Metadata.ResourceVersion),
		Pod:      objectIdentity(pod.APIVersion, pod.Kind, pod.Metadata.Name, pod.Metadata.Namespace, pod.Metadata.UID, pod.Metadata.ResourceVersion),
		Workload: identity,
	}
	if expected != nil && !reflect.DeepEqual(observation, *expected) {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission UID, resourceVersion, or API identity drifted", nil)
	}
	return observation, nil
}

// ObserveAbsence proves that the frozen Sandbox and its exact Pod/Workload
// identities are absent. Namespace-wide reads intentionally ignore mutable
// labels so owner-reference orphans cannot hide by label substitution.
func (a *Adapter) ObserveAbsence(ctx context.Context, expected AdmissionObservation) error {
	if err := validateObservation(expected); err != nil {
		return err
	}
	var sandbox kubeSandbox
	err := a.call(ctx, http.MethodGet, a.resourcePath(expected.Sandbox.Name), nil, nil, &sandbox, "")
	if err == nil {
		return adapterError(ErrCleanupIncomplete, 409, "Sandbox identity or a same-name replacement remains", nil)
	}
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrNotFound {
		return err
	}

	var pods observedPodList
	if err := a.call(ctx, http.MethodGet, a.podCollectionPath(), nil, nil, &pods, ""); err != nil {
		return err
	}
	if pods.APIVersion != podAPIVersion || pods.Kind != "PodList" {
		return adapterError(ErrBackend, 502, "Pod absence observation API drifted", nil)
	}
	for _, pod := range pods.Items {
		if !validObservedIdentity(pod.APIVersion, pod.Kind, pod.Metadata) || pod.APIVersion != podAPIVersion || pod.Kind != podKind {
			return adapterError(ErrBackend, 502, "Pod absence observation contained an invalid identity", nil)
		}
		if pod.Metadata.UID == expected.Pod.UID || hasControllerUID(pod.Metadata.OwnerReferences, expected.Sandbox.UID) {
			return adapterError(ErrCleanupIncomplete, 409, "owned Sandbox Pod orphan remains", nil)
		}
	}

	var workloads observedWorkloadList
	if err := a.call(ctx, http.MethodGet, a.workloadCollectionPath(), nil, nil, &workloads, ""); err != nil {
		return err
	}
	if workloads.APIVersion != AdmissionAPIVersion || workloads.Kind != "WorkloadList" {
		return adapterError(ErrBackend, 502, "Workload absence observation API drifted", nil)
	}
	for _, workload := range workloads.Items {
		if !validObservedIdentity(workload.APIVersion, workload.Kind, workload.Metadata) || workload.APIVersion != AdmissionAPIVersion || workload.Kind != workloadKind {
			return adapterError(ErrBackend, 502, "Workload absence observation contained an invalid identity", nil)
		}
		if workload.Metadata.UID == expected.Workload.UID || hasControllerUID(workload.Metadata.OwnerReferences, expected.Pod.UID) {
			return adapterError(ErrCleanupIncomplete, 409, "owned Kueue Workload orphan remains", nil)
		}
	}
	return nil
}

func admissionSelector(workspaceID, ownerID, name string) string {
	return selector(workspaceID, ownerID) + "," + SandboxIDLabel + "=" + name
}

func hasAdmissionLabels(labels map[string]string, workspaceID, ownerID, name string) bool {
	return labels[ManagedLabel] == "true" && labels[WorkspaceLabel] == workspaceID && labels[OwnerLabel] == ownerID && labels[SandboxIDLabel] == name && labels[QueueLabel] == QueueName
}

func validObservedIdentity(apiVersion, kind string, metadata observedMetadata) bool {
	return apiVersion != "" && kind != "" && metadata.Namespace == Namespace && len(metadata.Name) <= 253 && dnsNamePattern.MatchString(metadata.Name) && objectIDPattern.MatchString(metadata.UID) && objectIDPattern.MatchString(metadata.ResourceVersion)
}

func hasExactControllerOwner(owners []kubeOwnerReference, apiVersion, kind, name, uid string) bool {
	controllers := 0
	matched := false
	for _, owner := range owners {
		if owner.Controller {
			controllers++
			matched = owner.APIVersion == apiVersion && owner.Kind == kind && owner.Name == name && owner.UID == uid
		}
	}
	return controllers == 1 && matched
}

func hasControllerUID(owners []kubeOwnerReference, uid string) bool {
	for _, owner := range owners {
		if owner.Controller && owner.UID == uid {
			return true
		}
	}
	return false
}

func exactAdmittedCondition(conditions []kubeCondition) (kubeCondition, bool) {
	var result kubeCondition
	count := 0
	for _, condition := range conditions {
		if condition.Type == "Admitted" {
			count++
			result = condition
		}
	}
	return result, count == 1 && result.Status == "True"
}

func objectIdentity(apiVersion, kind, name, namespace, uid, resourceVersion string) ObjectIdentity {
	return ObjectIdentity{APIVersion: apiVersion, Kind: kind, Namespace: namespace, Name: name, UID: uid, ResourceVersion: resourceVersion}
}

func validateObservation(value AdmissionObservation) error {
	if value.Sandbox.APIVersion != APIVersion || value.Sandbox.Kind != Kind || value.Pod.APIVersion != podAPIVersion || value.Pod.Kind != podKind ||
		value.Workload.APIVersion != AdmissionAPIVersion || value.Sandbox.Namespace != Namespace || value.Pod.Namespace != Namespace || value.Workload.Namespace != Namespace ||
		!objectIDPattern.MatchString(value.Sandbox.UID) || !objectIDPattern.MatchString(value.Pod.UID) || !objectIDPattern.MatchString(value.Workload.UID) ||
		!objectIDPattern.MatchString(value.Sandbox.ResourceVersion) || !objectIDPattern.MatchString(value.Pod.ResourceVersion) || !objectIDPattern.MatchString(value.Workload.ResourceVersion) ||
		value.Workload.Owner.UID != value.Sandbox.UID || value.Workload.Digest != workloadIdentityDigest(value.Workload) {
		return adapterError(ErrInvalidRequest, 400, "absence observation identity is invalid", nil)
	}
	return nil
}

func (a *Adapter) podCollectionPath() string {
	return "/api/v1/namespaces/" + Namespace + "/pods"
}

func (a *Adapter) workloadCollectionPath() string {
	return fmt.Sprintf("/apis/%s/namespaces/%s/workloads", AdmissionAPIVersion, Namespace)
}
