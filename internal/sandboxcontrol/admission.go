package sandboxcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
)

const (
	podAPIVersion          = "v1"
	podKind                = "Pod"
	workloadKind           = "Workload"
	registryPullSecretName = "blazn-registry-pull"
	agentWorkloadLabel     = "frontro.io/agent-workloads"
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
	Digest   string           `json:"digest"`
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
	RawSpec    json.RawMessage  `json:"-"`
}

func (pod *observedPod) UnmarshalJSON(data []byte) error {
	type alias observedPod
	decoded := struct {
		*alias
		Spec json.RawMessage `json:"spec"`
	}{alias: (*alias)(pod)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.Spec) == 0 || json.Unmarshal(decoded.Spec, &pod.Spec) != nil {
		return fmt.Errorf("Pod spec is invalid")
	}
	pod.RawSpec = append(pod.RawSpec[:0], decoded.Spec...)
	return nil
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
	Spec struct {
		QueueName string `json:"queueName"`
	} `json:"spec"`
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

	var pods observedPodList
	if err := a.call(ctx, http.MethodGet, a.podCollectionPath(), nil, nil, &pods, ""); err != nil {
		return AdmissionObservation{}, err
	}
	if pods.APIVersion != podAPIVersion || pods.Kind != "PodList" {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Pod collection API drifted", nil)
	}
	podCandidates := make([]observedPod, 0, 1)
	relatedPods := 0
	for _, candidate := range pods.Items {
		if !validObservedIdentity(candidate.APIVersion, candidate.Kind, candidate.Metadata) || candidate.APIVersion != podAPIVersion || candidate.Kind != podKind {
			return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Pod collection contained an invalid identity", nil)
		}
		if hasAdmissionLabels(candidate.Metadata.Labels, request.WorkspaceID, request.OwnerID, request.Name) {
			relatedPods++
		}
		if hasControllerUID(candidate.Metadata.OwnerReferences, record.UID) {
			podCandidates = append(podCandidates, candidate)
		}
	}
	if len(podCandidates) == 0 {
		if relatedPods != 0 {
			return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Pod owner identity changed", nil)
		}
		return AdmissionObservation{}, adapterError(ErrAdmissionPending, 409, "admission Pod is pending", nil)
	}
	if len(podCandidates) != 1 {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission requires exactly one API-stable owned Pod", nil)
	}
	pod := podCandidates[0]
	if !hasExactControllerOwner(pod.Metadata.OwnerReferences, APIVersion, Kind, request.Name, record.UID) ||
		!sameObservedPodMaterialSpec(pod.RawSpec, manifest.Spec.PodTemplate.Spec) || !hasAdmissionLabels(pod.Metadata.Labels, request.WorkspaceID, request.OwnerID, request.Name) {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Pod substituted the Sandbox owner or material spec", nil)
	}

	var workloads observedWorkloadList
	if err := a.call(ctx, http.MethodGet, a.workloadCollectionPath(), nil, nil, &workloads, ""); err != nil {
		return AdmissionObservation{}, err
	}
	if workloads.APIVersion != AdmissionAPIVersion || workloads.Kind != "WorkloadList" {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Workload collection API drifted", nil)
	}
	workloadCandidates := make([]observedWorkload, 0, 1)
	relatedWorkloads := 0
	for _, candidate := range workloads.Items {
		if !validObservedIdentity(candidate.APIVersion, candidate.Kind, candidate.Metadata) || candidate.APIVersion != AdmissionAPIVersion || candidate.Kind != workloadKind {
			return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Workload collection contained an invalid identity", nil)
		}
		if hasAdmissionLabels(candidate.Metadata.Labels, request.WorkspaceID, request.OwnerID, request.Name) {
			relatedWorkloads++
		}
		if hasControllerUID(candidate.Metadata.OwnerReferences, pod.Metadata.UID) {
			workloadCandidates = append(workloadCandidates, candidate)
		}
	}
	if len(workloadCandidates) == 0 {
		if relatedWorkloads != 0 {
			return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission Workload owner identity changed", nil)
		}
		return AdmissionObservation{}, adapterError(ErrAdmissionPending, 409, "admission Workload is pending", nil)
	}
	if len(workloadCandidates) != 1 {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "admission requires exactly one API-stable Workload", nil)
	}
	workload := workloadCandidates[0]
	condition, admitted := exactAdmittedCondition(workload.Status.Conditions)
	if !hasExactControllerOwner(workload.Metadata.OwnerReferences, podAPIVersion, podKind, pod.Metadata.Name, pod.Metadata.UID) ||
		!hasAdmissionLabels(workload.Metadata.Labels, request.WorkspaceID, request.OwnerID, request.Name) || workload.Spec.QueueName != QueueName {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "Workload did not preserve the exact admitted Pod ownership chain", nil)
	}
	if workload.Status.Admission == nil || !admitted {
		admittedConditions := 0
		for _, value := range workload.Status.Conditions {
			if value.Type == "Admitted" {
				admittedConditions++
			}
		}
		if admittedConditions > 1 {
			return AdmissionObservation{}, adapterError(ErrConflict, 409, "Workload has ambiguous admission conditions", nil)
		}
		return AdmissionObservation{}, adapterError(ErrAdmissionPending, 409, "Workload admission is pending", nil)
	}
	if !dnsNamePattern.MatchString(workload.Status.Admission.ClusterQueue) {
		return AdmissionObservation{}, adapterError(ErrConflict, 409, "Workload admitted ClusterQueue is invalid", nil)
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
	observation.Digest = admissionObservationDigest(observation)
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

func sameObservedPodMaterialSpec(raw json.RawMessage, expected kubePodSpec) bool {
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return false
	}
	observedValue, observedOK := decodeMaterialJSON(raw)
	expectedValue, expectedOK := decodeMaterialJSON(expectedJSON)
	observed, observedIsObject := observedValue.(map[string]any)
	expectedObject, expectedIsObject := expectedValue.(map[string]any)
	if !observedOK || !expectedOK || !observedIsObject || !expectedIsObject {
		return false
	}
	serviceAccount, _ := expectedObject["serviceAccountName"].(string)
	for key, value := range map[string]any{
		"dnsPolicy":                     "ClusterFirst",
		"schedulerName":                 "default-scheduler",
		"terminationGracePeriodSeconds": json.Number("30"),
		"enableServiceLinks":            true,
		"preemptionPolicy":              "PreemptLowerPriority",
		"priority":                      json.Number("0"),
		"serviceAccount":                serviceAccount,
	} {
		if !removeExactDefault(observed, key, value) {
			return false
		}
	}
	if nodeName, exists := observed["nodeName"]; exists {
		name, ok := nodeName.(string)
		if !ok || len(name) > 253 || !dnsNamePattern.MatchString(name) {
			return false
		}
		delete(observed, "nodeName")
	}
	defaultTolerations := []any{
		map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": json.Number("300")},
		map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": json.Number("300")},
	}
	if !removeExactDefault(observed, "tolerations", defaultTolerations) {
		return false
	}
	if pullSecrets, exists := observed["imagePullSecrets"]; exists {
		expectedPullSecrets := []any{map[string]any{"name": registryPullSecretName}}
		if !reflect.DeepEqual(pullSecrets, expectedPullSecrets) {
			return false
		}
		delete(observed, "imagePullSecrets")
	}
	if selectors, exists := observed["nodeSelector"]; exists {
		selectorMap, ok := selectors.(map[string]any)
		if !ok {
			return false
		}
		if flavor, injected := selectorMap[agentWorkloadLabel]; injected {
			if flavor != "true" {
				return false
			}
			delete(selectorMap, agentWorkloadLabel)
		}
	}
	for _, field := range []string{"containers", "initContainers"} {
		containers, exists := observed[field]
		if !exists && field == "initContainers" {
			continue
		}
		values, ok := containers.([]any)
		if !ok {
			return false
		}
		for _, value := range values {
			container, ok := value.(map[string]any)
			if !ok || !removeExactDefault(container, "imagePullPolicy", "IfNotPresent") ||
				!removeExactDefault(container, "terminationMessagePath", "/dev/termination-log") ||
				!removeExactDefault(container, "terminationMessagePolicy", "File") {
				return false
			}
		}
	}
	return reflect.DeepEqual(observed, expectedObject)
}

func removeExactDefault(object map[string]any, key string, expected any) bool {
	value, exists := object[key]
	if !exists {
		return true
	}
	if !reflect.DeepEqual(value, expected) {
		return false
	}
	delete(object, key)
	return true
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
	if !validFrozenObjectIdentity(value.Sandbox, APIVersion, Kind, true) ||
		!validFrozenObjectIdentity(value.Pod, podAPIVersion, podKind, false) ||
		value.Workload.APIVersion != AdmissionAPIVersion || value.Workload.Namespace != Namespace ||
		value.Sandbox.Name != value.Workload.SandboxID || value.Sandbox.Name != value.Workload.Owner.Name ||
		value.Workload.Owner.UID != value.Sandbox.UID || !digestPattern.MatchString(value.Digest) ||
		value.Digest != admissionObservationDigest(value) {
		return adapterError(ErrInvalidRequest, 400, "absence observation identity is invalid", nil)
	}
	receipt := OperationReceipt{Namespace: Namespace, Name: value.Sandbox.Name, UID: value.Sandbox.UID, WorkspaceID: value.Workload.WorkspaceID}
	if err := validateWorkloadIdentity(value.Workload, receipt, true); err != nil {
		return adapterError(ErrInvalidRequest, 400, "absence observation identity is invalid", err)
	}
	return nil
}

// ValidateAdmissionObservation verifies the complete frozen Sandbox, Pod, and
// Workload identity chain, including both canonical digests. Callers that
// retain an observation for later absence proof must validate it before it
// crosses their persistence or cache boundary.
func ValidateAdmissionObservation(value AdmissionObservation) error {
	return validateObservation(value)
}

// AdmissionObservationDigest returns the canonical digest for an admission
// observation. The Workload identity must already carry its own canonical
// digest; ValidateAdmissionObservation performs the complete validation.
func AdmissionObservationDigest(value AdmissionObservation) string {
	return admissionObservationDigest(value)
}

func validFrozenObjectIdentity(value ObjectIdentity, apiVersion, kind string, sandboxName bool) bool {
	validName := len(value.Name) <= 253 && dnsNamePattern.MatchString(value.Name)
	if sandboxName {
		validName = dnsLabelPattern.MatchString(value.Name)
	}
	return value.APIVersion == apiVersion && value.Kind == kind && value.Namespace == Namespace && validName &&
		objectIDPattern.MatchString(value.UID) && objectIDPattern.MatchString(value.ResourceVersion)
}

func (a *Adapter) podCollectionPath() string {
	return "/api/v1/namespaces/" + Namespace + "/pods"
}

func (a *Adapter) workloadCollectionPath() string {
	return fmt.Sprintf("/apis/%s/namespaces/%s/workloads", AdmissionAPIVersion, Namespace)
}
