package sandboxcontrol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type ArtifactExporter interface {
	Export(context.Context, SandboxRecord, []ArtifactExport) ([]ArtifactReceipt, error)
}

type Config struct {
	BaseURL          string
	BearerToken      string
	HTTPClient       *http.Client
	RuntimeClasses   map[string]RuntimeCapability
	Exporter         ArtifactExporter
	Now              func() time.Time
	MaxResponseBytes int64
	WatchIdleTimeout time.Duration
}

type Adapter struct {
	base      *url.URL
	token     string
	client    *http.Client
	runtimes  map[string]RuntimeCapability
	exporter  ArtifactExporter
	now       func() time.Time
	maxBytes  int64
	watchIdle time.Duration
}

type kubeSandbox struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   kubeMetadata    `json:"metadata"`
	Spec       kubeSandboxSpec `json:"spec"`
	Status     kubeStatus      `json:"status,omitempty"`
}

type kubeMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
}

type kubeSandboxSpec struct {
	ShutdownPolicy string          `json:"shutdownPolicy"`
	PodTemplate    kubePodTemplate `json:"podTemplate"`
}

type kubePodTemplate struct {
	Metadata kubePodMetadata `json:"metadata"`
	Spec     kubePodSpec     `json:"spec"`
}

type kubePodMetadata struct {
	Labels map[string]string `json:"labels"`
}

type kubePodSpec struct {
	RuntimeClassName             string            `json:"runtimeClassName,omitempty"`
	ServiceAccountName           string            `json:"serviceAccountName"`
	AutomountServiceAccountToken bool              `json:"automountServiceAccountToken"`
	RestartPolicy                string            `json:"restartPolicy"`
	NodeSelector                 map[string]string `json:"nodeSelector"`
	SecurityContext              map[string]any    `json:"securityContext"`
	Containers                   []kubeContainer   `json:"containers"`
	Volumes                      []kubeVolume      `json:"volumes"`
}

type kubeContainer struct {
	Name            string                       `json:"name"`
	Image           string                       `json:"image"`
	Command         []string                     `json:"command"`
	SecurityContext map[string]any               `json:"securityContext"`
	Resources       map[string]map[string]string `json:"resources"`
	VolumeMounts    []map[string]any             `json:"volumeMounts"`
}

type kubeVolume struct {
	Name     string         `json:"name"`
	EmptyDir map[string]any `json:"emptyDir"`
}

type kubeStatus struct {
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Conditions         []kubeCondition `json:"conditions,omitempty"`
}

type kubeCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

type kubeList struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []kubeSandbox `json:"items"`
}

type kubeWatchEvent struct {
	Type   string      `json:"type"`
	Object kubeSandbox `json:"object"`
}

type kubeRuntimeClass struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Handler string `json:"handler"`
}

type ListResult struct {
	Sandboxes       []SandboxRecord
	ResourceVersion string
	Continue        string
}

func New(config Config) (*Adapter, error) {
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Scheme != "http" && base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("sandbox Kubernetes API base URL is invalid")
	}
	if config.HTTPClient == nil {
		return nil, fmt.Errorf("sandbox Kubernetes HTTP client is required")
	}
	if config.Exporter == nil {
		return nil, fmt.Errorf("sandbox artifact exporter is required")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	max := config.MaxResponseBytes
	if max == 0 {
		max = 4 << 20
	}
	if max < 1024 || max > 16<<20 {
		return nil, fmt.Errorf("sandbox response limit is invalid")
	}
	idle := config.WatchIdleTimeout
	if idle == 0 {
		idle = 30 * time.Second
	}
	if idle < time.Second || idle > 5*time.Minute {
		return nil, fmt.Errorf("sandbox watch idle timeout is invalid")
	}
	runtimes := make(map[string]RuntimeCapability, len(config.RuntimeClasses))
	for name, capability := range config.RuntimeClasses {
		capability.Architectures = append([]string(nil), capability.Architectures...)
		runtimes[name] = capability
	}
	return &Adapter{base: base, token: config.BearerToken, client: config.HTTPClient, runtimes: runtimes, exporter: config.Exporter, now: now, maxBytes: max, watchIdle: idle}, nil
}

func (a *Adapter) Create(ctx context.Context, request CreateRequest) (SandboxRecord, OperationReceipt, error) {
	if err := ValidateCreate(request, a.runtimes); err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	if err := a.verifyRuntimeClass(ctx, request); err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	manifest := render(request)
	var created kubeSandbox
	if err := a.call(ctx, http.MethodPost, a.collectionPath(), nil, manifest, &created, "application/json"); err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	record, err := a.record(created)
	if err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	if err := verifyManaged(record, request.WorkspaceID, request.OwnerID); err != nil {
		return SandboxRecord{}, OperationReceipt{}, a.rejectCreated(ctx, record, err)
	}
	if record.QueueName != QueueName {
		err := adapterError(ErrQueueRequired, 502, "backend did not preserve mandatory queue label", nil)
		return SandboxRecord{}, OperationReceipt{}, a.rejectCreated(ctx, record, err)
	}
	if record.RuntimeClassName != request.RuntimeClassName || !contains(record.Finalizers, CleanupFinalizer) {
		err := adapterError(ErrBackend, 502, "backend did not preserve runtime or cleanup boundary", nil)
		return SandboxRecord{}, OperationReceipt{}, a.rejectCreated(ctx, record, err)
	}
	receipt, err := NewReceipt(request.RequestID, OperationCreate, record, nil, a.now())
	return record, receipt, err
}

func (a *Adapter) rejectCreated(ctx context.Context, record SandboxRecord, original error) error {
	if record.Namespace != Namespace || !dnsLabelPattern.MatchString(record.Name) || record.UID == "" || record.ResourceVersion == "" {
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox identity cannot be cleaned safely", original)
	}
	options := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]string{"uid": record.UID, "resourceVersion": record.ResourceVersion}}
	if err := a.call(ctx, http.MethodDelete, a.resourcePath(record.Name), nil, options, nil, "application/json"); err != nil {
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox deletion failed", err)
	}
	var deleting kubeSandbox
	if err := a.call(ctx, http.MethodGet, a.resourcePath(record.Name), nil, nil, &deleting, ""); err != nil {
		var adapterErr *AdapterError
		if errors.As(err, &adapterErr) && adapterErr.Code == ErrNotFound {
			return original
		}
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox cleanup state is unavailable", err)
	}
	finalizers := make([]string, 0, len(deleting.Metadata.Finalizers))
	for _, finalizer := range deleting.Metadata.Finalizers {
		if finalizer != CleanupFinalizer {
			finalizers = append(finalizers, finalizer)
		}
	}
	if contains(deleting.Metadata.Finalizers, CleanupFinalizer) {
		patch := map[string]any{"metadata": map[string]any{"resourceVersion": deleting.Metadata.ResourceVersion, "finalizers": finalizers}}
		if err := a.call(ctx, http.MethodPatch, a.resourcePath(record.Name), nil, patch, nil, "application/merge-patch+json"); err != nil {
			return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox finalizer cleanup failed", err)
		}
	}
	return original
}

func (a *Adapter) verifyRuntimeClass(ctx context.Context, request CreateRequest) error {
	if request.RuntimeClassName == "" {
		return nil
	}
	capability := a.runtimes[request.RuntimeClassName]
	var runtimeClass kubeRuntimeClass
	path := "/apis/node.k8s.io/v1/runtimeclasses/" + url.PathEscape(request.RuntimeClassName)
	if err := a.call(ctx, http.MethodGet, path, nil, nil, &runtimeClass, ""); err != nil {
		return adapterError(ErrRuntimeUntrusted, 403, "qualified RuntimeClass is unavailable", err)
	}
	if runtimeClass.APIVersion != "node.k8s.io/v1" || runtimeClass.Kind != "RuntimeClass" || runtimeClass.Metadata.Name != request.RuntimeClassName || runtimeClass.Handler != capability.Handler {
		return adapterError(ErrRuntimeUntrusted, 403, "live RuntimeClass differs from qualified capability", nil)
	}
	return nil
}

func (a *Adapter) Get(ctx context.Context, workspaceID, ownerID, name string) (SandboxRecord, error) {
	if err := validateIdentity(workspaceID, ownerID, name); err != nil {
		return SandboxRecord{}, err
	}
	var object kubeSandbox
	if err := a.call(ctx, http.MethodGet, a.resourcePath(name), nil, nil, &object, ""); err != nil {
		return SandboxRecord{}, err
	}
	record, err := a.record(object)
	if err != nil {
		return SandboxRecord{}, err
	}
	if err := verifyManaged(record, workspaceID, ownerID); err != nil {
		return SandboxRecord{}, err
	}
	return record, nil
}

func (a *Adapter) Status(ctx context.Context, workspaceID, ownerID, name string) (SandboxStatus, error) {
	if err := validateIdentity(workspaceID, ownerID, name); err != nil {
		return SandboxStatus{}, err
	}
	var object kubeSandbox
	if err := a.call(ctx, http.MethodGet, a.resourcePath(name)+"/status", nil, nil, &object, ""); err != nil {
		return SandboxStatus{}, err
	}
	record, err := a.record(object)
	if err != nil {
		return SandboxStatus{}, err
	}
	if err := verifyManaged(record, workspaceID, ownerID); err != nil {
		return SandboxStatus{}, err
	}
	state, reason, message, ready := stateFrom(object)
	return SandboxStatus{State: state, Reason: reason, Message: message, Ready: ready, ResourceVersion: object.Metadata.ResourceVersion, ObservedGeneration: object.Status.ObservedGeneration}, nil
}

func (a *Adapter) List(ctx context.Context, workspaceID, ownerID, continueToken string, limit int) (ListResult, error) {
	if err := validateIdentity(workspaceID, ownerID, "sandbox"); err != nil {
		return ListResult{}, err
	}
	if limit < 1 || limit > 500 || len(continueToken) > 2048 {
		return ListResult{}, adapterError(ErrInvalidRequest, 400, "list pagination is invalid", nil)
	}
	query := url.Values{"labelSelector": {selector(workspaceID, ownerID)}, "limit": {strconv.Itoa(limit)}}
	if continueToken != "" {
		query.Set("continue", continueToken)
	}
	var list kubeList
	if err := a.call(ctx, http.MethodGet, a.collectionPath(), query, nil, &list, ""); err != nil {
		return ListResult{}, err
	}
	result := ListResult{ResourceVersion: list.Metadata.ResourceVersion, Continue: list.Metadata.Continue, Sandboxes: make([]SandboxRecord, 0, len(list.Items))}
	for _, item := range list.Items {
		record, err := a.record(item)
		if err != nil {
			return ListResult{}, err
		}
		if err := verifyManaged(record, workspaceID, ownerID); err != nil {
			return ListResult{}, err
		}
		result.Sandboxes = append(result.Sandboxes, record)
	}
	return result, nil
}

func (a *Adapter) Watch(ctx context.Context, workspaceID, ownerID, resourceVersion string) (<-chan WatchEvent, <-chan error, error) {
	if err := validateIdentity(workspaceID, ownerID, "sandbox"); err != nil {
		return nil, nil, err
	}
	if resourceVersion == "" || len(resourceVersion) > 128 {
		return nil, nil, adapterError(ErrInvalidRequest, 400, "watch resourceVersion is required", nil)
	}
	query := url.Values{"watch": {"true"}, "allowWatchBookmarks": {"true"}, "resourceVersion": {resourceVersion}, "labelSelector": {selector(workspaceID, ownerID)}, "timeoutSeconds": {strconv.Itoa(int(a.watchIdle.Seconds()))}}
	request, err := a.request(ctx, http.MethodGet, a.collectionPath(), query, nil, "")
	if err != nil {
		return nil, nil, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, nil, adapterError(ErrBackend, 502, "watch connection failed", err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, nil, decodeStatus(response)
	}
	events := make(chan WatchEvent)
	errors := make(chan error, 1)
	go func() {
		defer response.Body.Close()
		defer close(events)
		defer close(errors)
		decoder := json.NewDecoder(bufio.NewReader(io.LimitReader(response.Body, a.maxBytes)))
		for {
			var event kubeWatchEvent
			if err := decoder.Decode(&event); err != nil {
				if err != io.EOF {
					errors <- adapterError(ErrBackend, 502, "watch stream is invalid", err)
				}
				return
			}
			if event.Type == "BOOKMARK" {
				continue
			}
			if event.Type != "ADDED" && event.Type != "MODIFIED" && event.Type != "DELETED" {
				errors <- adapterError(ErrBackend, 502, "watch event type is invalid", nil)
				return
			}
			record, err := a.record(event.Object)
			if err == nil {
				err = verifyManaged(record, workspaceID, ownerID)
			}
			if err != nil {
				errors <- err
				return
			}
			select {
			case events <- WatchEvent{Type: event.Type, Sandbox: record}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, errors, nil
}

func (a *Adapter) Delete(ctx context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion string) (OperationReceipt, error) {
	if !requestPattern.MatchString(requestID) || uid == "" || resourceVersion == "" {
		return OperationReceipt{}, adapterError(ErrInvalidRequest, 400, "delete precondition is invalid", nil)
	}
	record, err := a.Get(ctx, workspaceID, ownerID, name)
	if err != nil {
		return OperationReceipt{}, err
	}
	if record.UID != uid || record.ResourceVersion != resourceVersion {
		return OperationReceipt{}, adapterError(ErrResourceVersionStale, 409, "delete precondition does not match", nil)
	}
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]string{"uid": uid, "resourceVersion": resourceVersion}}
	if err := a.call(ctx, http.MethodDelete, a.resourcePath(name), nil, body, nil, "application/json"); err != nil {
		return OperationReceipt{}, err
	}
	record.State = StateStopping
	record.Deleting = true
	return NewReceipt(requestID, OperationDelete, record, nil, a.now())
}

func (a *Adapter) Finalize(ctx context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion string) (OperationReceipt, error) {
	if !requestPattern.MatchString(requestID) {
		return OperationReceipt{}, adapterError(ErrInvalidRequest, 400, "finalize request ID is invalid", nil)
	}
	record, err := a.Get(ctx, workspaceID, ownerID, name)
	if err != nil {
		return OperationReceipt{}, err
	}
	if record.UID != uid || record.ResourceVersion != resourceVersion || !record.Deleting || !contains(record.Finalizers, CleanupFinalizer) {
		return OperationReceipt{}, adapterError(ErrCleanupIncomplete, 409, "sandbox is not ready for receipted cleanup", nil)
	}
	artifacts, err := a.exporter.Export(ctx, record, record.Artifacts)
	if err != nil {
		return OperationReceipt{}, adapterError(ErrArtifactExport, 502, "artifact export did not complete", err)
	}
	if err := validateArtifactCompletion(record, artifacts); err != nil {
		return OperationReceipt{}, err
	}
	finalizers := make([]string, 0, len(record.Finalizers)-1)
	for _, finalizer := range record.Finalizers {
		if finalizer != CleanupFinalizer {
			finalizers = append(finalizers, finalizer)
		}
	}
	patch := map[string]any{"metadata": map[string]any{"resourceVersion": resourceVersion, "finalizers": finalizers}}
	var updated kubeSandbox
	if err := a.call(ctx, http.MethodPatch, a.resourcePath(name), nil, patch, &updated, "application/merge-patch+json"); err != nil {
		return OperationReceipt{}, err
	}
	updatedRecord, err := a.record(updated)
	if err != nil {
		return OperationReceipt{}, err
	}
	if err := verifyManaged(updatedRecord, workspaceID, ownerID); err != nil {
		return OperationReceipt{}, err
	}
	updatedRecord.State = StateDeleted
	return NewReceipt(requestID, OperationFinalize, updatedRecord, artifacts, a.now())
}

func render(request CreateRequest) kubeSandbox {
	labels := map[string]string{ManagedLabel: "true", WorkspaceLabel: request.WorkspaceID, OwnerLabel: request.OwnerID, SandboxIDLabel: request.Name}
	podLabels := cloneMap(labels)
	podLabels[QueueLabel] = QueueName
	annotations := map[string]string{"sandboxes.blazn.dev/trust-level": string(request.TrustLevel), "sandboxes.blazn.dev/expires-at": request.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	if encoded, err := json.Marshal(request.Artifacts); err == nil {
		annotations["sandboxes.blazn.dev/artifact-exports"] = string(encoded)
	}
	return kubeSandbox{
		APIVersion: APIVersion, Kind: Kind,
		Metadata: kubeMetadata{Name: request.Name, Namespace: Namespace, Labels: labels, Annotations: annotations, Finalizers: []string{CleanupFinalizer}},
		Spec: kubeSandboxSpec{ShutdownPolicy: "Delete", PodTemplate: kubePodTemplate{Metadata: kubePodMetadata{Labels: podLabels}, Spec: kubePodSpec{
			RuntimeClassName: request.RuntimeClassName, ServiceAccountName: ServiceAccountName, AutomountServiceAccountToken: false,
			RestartPolicy: "Never", NodeSelector: map[string]string{"kubernetes.io/arch": request.Architecture, "blazn.dev/sandbox-eligible": "true"},
			SecurityContext: map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532), "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			Containers: []kubeContainer{{Name: "main", Image: request.Image, Command: append([]string(nil), request.Command...),
				SecurityContext: map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string][]string{"drop": {"ALL"}}},
				Resources:       map[string]map[string]string{"requests": {"cpu": request.CPURequest, "memory": request.MemoryRequest}, "limits": {"cpu": request.CPULimit, "memory": request.MemoryLimit}},
				VolumeMounts:    []map[string]any{{"name": "workspace", "mountPath": "/workspace"}}}},
			Volumes: []kubeVolume{{Name: "workspace", EmptyDir: map[string]any{"sizeLimit": "6Gi"}}},
		}}},
	}
}

func (a *Adapter) record(object kubeSandbox) (SandboxRecord, error) {
	if object.APIVersion != APIVersion || object.Kind != Kind || object.Metadata.Namespace != Namespace || !dnsLabelPattern.MatchString(object.Metadata.Name) || object.Metadata.UID == "" || object.Metadata.ResourceVersion == "" {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend Sandbox identity is invalid", nil)
	}
	artifacts := []ArtifactExport{}
	if value := object.Metadata.Annotations["sandboxes.blazn.dev/artifact-exports"]; value != "" {
		if len(value) > 32768 || json.Unmarshal([]byte(value), &artifacts) != nil {
			return SandboxRecord{}, adapterError(ErrBackend, 502, "backend artifact contract is invalid", nil)
		}
	}
	state, _, _, _ := stateFrom(object)
	return SandboxRecord{
		Name: object.Metadata.Name, Namespace: object.Metadata.Namespace, UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion,
		Generation: object.Metadata.Generation, WorkspaceID: object.Metadata.Labels[WorkspaceLabel], OwnerID: object.Metadata.Labels[OwnerLabel],
		QueueName: object.Spec.PodTemplate.Metadata.Labels[QueueLabel], RuntimeClassName: object.Spec.PodTemplate.Spec.RuntimeClassName,
		TrustLevel: TrustLevel(object.Metadata.Annotations["sandboxes.blazn.dev/trust-level"]), State: state,
		Deleting: object.Metadata.DeletionTimestamp != "", Finalizers: append([]string(nil), object.Metadata.Finalizers...), Artifacts: artifacts, Labels: cloneMap(object.Metadata.Labels),
	}, nil
}

func stateFrom(object kubeSandbox) (SandboxState, string, string, bool) {
	if object.Metadata.DeletionTimestamp != "" {
		return StateStopping, "Deleting", "sandbox deletion is in progress", false
	}
	for _, condition := range object.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return StateReady, condition.Reason, condition.Message, true
		}
		if condition.Status == "False" && (strings.Contains(strings.ToLower(condition.Reason), "fail") || strings.Contains(strings.ToLower(condition.Reason), "error")) {
			return StateFailed, condition.Reason, condition.Message, false
		}
		if condition.Status == "False" && (strings.Contains(strings.ToLower(condition.Reason), "queue") || strings.Contains(strings.ToLower(condition.Reason), "admission")) {
			return StateQueued, condition.Reason, condition.Message, false
		}
	}
	if object.Status.ObservedGeneration > 0 {
		return StateStarting, "Reconciling", "sandbox dependencies are reconciling", false
	}
	return StatePending, "Pending", "sandbox has not been observed", false
}

func (a *Adapter) collectionPath() string {
	return "/apis/agents.x-k8s.io/v1beta1/namespaces/" + Namespace + "/sandboxes"
}

func (a *Adapter) resourcePath(name string) string {
	return a.collectionPath() + "/" + url.PathEscape(name)
}

func (a *Adapter) call(ctx context.Context, method, path string, query url.Values, input, output any, contentType string) error {
	request, err := a.request(ctx, method, path, query, input, contentType)
	if err != nil {
		return err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return adapterError(ErrBackend, 502, "Kubernetes API request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeStatus(response)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, a.maxBytes))
	if err := decoder.Decode(output); err != nil {
		return adapterError(ErrBackend, 502, "Kubernetes API response is invalid", err)
	}
	return nil
}

func (a *Adapter) request(ctx context.Context, method, requestPath string, query url.Values, input any, contentType string) (*http.Request, error) {
	endpoint := *a.base
	endpoint.Path = strings.TrimSuffix(a.base.Path, "/") + requestPath
	endpoint.RawQuery = query.Encode()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, adapterError(ErrInvalidRequest, 400, "request encoding failed", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, adapterError(ErrInvalidRequest, 400, "request construction failed", err)
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if a.token != "" {
		request.Header.Set("Authorization", "Bearer "+a.token)
	}
	request.Header.Set("User-Agent", "blazn-sandbox-control-adapter/v1")
	return request, nil
}

func decodeStatus(response *http.Response) error {
	var status struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status)
	code, httpStatus := ErrBackend, response.StatusCode
	switch response.StatusCode {
	case http.StatusNotFound:
		code = ErrNotFound
	case http.StatusConflict:
		code = ErrConflict
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		code = ErrInvalidRequest
	}
	detail := status.Reason
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	return adapterError(code, httpStatus, detail, nil)
}

func verifyManaged(record SandboxRecord, workspaceID, ownerID string) error {
	if record.Namespace != Namespace || record.Labels[ManagedLabel] != "true" || record.Labels[SandboxIDLabel] != record.Name || record.WorkspaceID != workspaceID || record.OwnerID != ownerID {
		return adapterError(ErrIdentityBoundary, 404, "Sandbox is outside the authenticated identity boundary", nil)
	}
	return nil
}

func validateIdentity(workspaceID, ownerID, name string) error {
	if !dnsLabelPattern.MatchString(workspaceID) || !dnsLabelPattern.MatchString(ownerID) || !dnsLabelPattern.MatchString(name) {
		return adapterError(ErrIdentityBoundary, 400, "identity selector is invalid", nil)
	}
	return nil
}

func validateArtifactCompletion(sandbox SandboxRecord, receipts []ArtifactReceipt) error {
	specs := sandbox.Artifacts
	requested := make(map[string]ArtifactExport, len(specs))
	for _, spec := range specs {
		requested[spec.Name] = spec
	}
	prefix := "workspaces/" + sandbox.WorkspaceID + "/sandboxes/" + sandbox.Name + "/"
	byName := map[string]ArtifactReceipt{}
	for _, receipt := range receipts {
		_, wasRequested := requested[receipt.Name]
		if _, exists := byName[receipt.Name]; exists || !wasRequested || receipt.SchemaVersion != ArtifactSchema || !digestPattern.MatchString(receipt.SHA256) || !strings.HasPrefix(receipt.ObjectKey, prefix) || path.Clean(receipt.ObjectKey) != receipt.ObjectKey || strings.Contains(receipt.ObjectKey, "..") || receipt.Size < 0 {
			return adapterError(ErrArtifactExport, 502, "artifact receipt is invalid", nil)
		}
		if _, err := time.Parse(time.RFC3339Nano, receipt.ExportedAt); err != nil {
			return adapterError(ErrArtifactExport, 502, "artifact receipt timestamp is invalid", nil)
		}
		byName[receipt.Name] = receipt
	}
	for _, spec := range specs {
		if spec.Required {
			if _, exists := byName[spec.Name]; !exists {
				return adapterError(ErrArtifactExport, 502, "required artifact was not exported", nil)
			}
		}
	}
	return nil
}

func selector(workspaceID, ownerID string) string {
	return ManagedLabel + "=true," + WorkspaceLabel + "=" + workspaceID + "," + OwnerLabel + "=" + ownerID
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cloneMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
