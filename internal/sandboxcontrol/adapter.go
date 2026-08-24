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
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxio"
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
	RawSpec    json.RawMessage `json:"-"`
}

func (object *kubeSandbox) UnmarshalJSON(data []byte) error {
	type alias kubeSandbox
	decoded := struct {
		*alias
		Spec json.RawMessage `json:"spec"`
	}{alias: (*alias)(object)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.Spec) == 0 {
		object.RawSpec = nil
		return nil
	}
	if json.Unmarshal(decoded.Spec, &object.Spec) != nil {
		return fmt.Errorf("Sandbox spec is invalid")
	}
	object.RawSpec = append(object.RawSpec[:0], decoded.Spec...)
	return nil
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
	OperatingMode  string          `json:"operatingMode"`
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
	InitContainers               []kubeContainer   `json:"initContainers,omitempty"`
	Volumes                      []kubeVolume      `json:"volumes,omitempty"`
}

type kubeContainer struct {
	Name            string                       `json:"name"`
	Image           string                       `json:"image"`
	Command         []string                     `json:"command"`
	RestartPolicy   string                       `json:"restartPolicy,omitempty"`
	SecurityContext map[string]any               `json:"securityContext"`
	Resources       map[string]map[string]string `json:"resources"`
	VolumeMounts    []kubeVolumeMount            `json:"volumeMounts,omitempty"`
}

type kubeVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
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
	return a.EnsureCreated(ctx, request, "")
}

// EnsureCreated creates a Sandbox once or adopts only the exact UID requested
// by a recovering caller. A response-lost POST may be recovered without a
// prior UID, but only after a preflight NotFound and exact spec/intent checks.
// An object that existed before the POST is never adopted by name or labels.
func (a *Adapter) EnsureCreated(ctx context.Context, request CreateRequest, expectedUID string) (SandboxRecord, OperationReceipt, error) {
	if err := ValidateCreate(request, a.runtimes); err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	if err := a.verifyRuntimeClass(ctx, request); err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	canonicalArtifacts, artifactDigest, err := CanonicalArtifactContract(request.Artifacts)
	if err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	request.Artifacts = canonicalArtifacts
	canonicalSourceMounts, err := canonicalSources(request.Sources)
	if err != nil {
		return SandboxRecord{}, OperationReceipt{}, err
	}
	request.Sources = canonicalSourceMounts
	intentDigest, err := createIntentDigest(request)
	if err != nil {
		return SandboxRecord{}, OperationReceipt{}, adapterError(ErrInvalidRequest, 400, "create intent cannot be canonicalized", err)
	}
	manifest := render(request, artifactDigest, intentDigest)
	var existing kubeSandbox
	preflightErr := a.call(ctx, http.MethodGet, a.resourcePath(request.Name), nil, nil, &existing, "")
	if preflightErr == nil {
		if expectedUID == "" {
			return SandboxRecord{}, OperationReceipt{}, adapterError(ErrConflict, 409, "Sandbox name is already occupied without an exact UID precondition", nil)
		}
		return a.finishEnsureCreated(ctx, request, artifactDigest, intentDigest, expectedUID, manifest, existing, false)
	}
	var preflightAdapterErr *AdapterError
	if !errors.As(preflightErr, &preflightAdapterErr) || preflightAdapterErr.Code != ErrNotFound {
		return SandboxRecord{}, OperationReceipt{}, preflightErr
	}
	if expectedUID != "" {
		return SandboxRecord{}, OperationReceipt{}, adapterError(ErrConflict, 409, "expected Sandbox UID is absent; refusing replacement creation", nil)
	}
	var created kubeSandbox
	postErr := a.call(ctx, http.MethodPost, a.collectionPath(), nil, manifest, &created, "application/json")
	if postErr != nil {
		if !ambiguousCreateError(postErr) {
			return SandboxRecord{}, OperationReceipt{}, postErr
		}
		// POST failures can be ambiguous (transport loss, timeout, apiserver
		// retryable failure after accepting the object). Resolve once by exact
		// persisted UID/spec/intent, never after an authoritative rejection or
		// conflict and never by the deterministic name or labels alone.
		if err := a.call(ctx, http.MethodGet, a.resourcePath(request.Name), nil, nil, &created, ""); err != nil {
			return SandboxRecord{}, OperationReceipt{}, postErr
		}
		return a.finishEnsureCreated(ctx, request, artifactDigest, intentDigest, "", manifest, created, false)
	}
	return a.finishEnsureCreated(ctx, request, artifactDigest, intentDigest, "", manifest, created, true)
}

type kubeHTTPStatusError struct {
	statusCode int
}

func (e *kubeHTTPStatusError) Error() string {
	return fmt.Sprintf("Kubernetes API returned HTTP %d", e.statusCode)
}

func ambiguousCreateError(err error) bool {
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != ErrBackend || adapterErr.Cause == nil {
		return false
	}
	var statusErr *kubeHTTPStatusError
	if !errors.As(adapterErr.Cause, &statusErr) {
		// A client/transport error does not prove whether the apiserver committed
		// the request.
		return true
	}
	switch statusErr.statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (a *Adapter) finishEnsureCreated(ctx context.Context, request CreateRequest, artifactDigest, intentDigest, expectedUID string, manifest, created kubeSandbox, cleanup bool) (SandboxRecord, OperationReceipt, error) {
	record, err := a.record(created)
	if err != nil {
		if cleanup {
			return SandboxRecord{}, OperationReceipt{}, a.rejectCreatedMetadata(ctx, request.Name, created.Metadata, err)
		}
		return SandboxRecord{}, OperationReceipt{}, err
	}
	reject := func(err error) error {
		if cleanup {
			return a.rejectCreated(ctx, record, err)
		}
		return err
	}
	if expectedUID != "" && record.UID != expectedUID {
		return SandboxRecord{}, OperationReceipt{}, reject(adapterError(ErrConflict, 409, "persisted Sandbox UID differs from the exact adoption precondition", nil))
	}
	if err := verifyManaged(record, request.WorkspaceID, request.OwnerID); err != nil {
		return SandboxRecord{}, OperationReceipt{}, reject(err)
	}
	if record.QueueName != QueueName {
		err := adapterError(ErrQueueRequired, 502, "backend did not preserve mandatory queue label", nil)
		return SandboxRecord{}, OperationReceipt{}, reject(err)
	}
	if record.RuntimeClassName != request.RuntimeClassName || !contains(record.Finalizers, CleanupFinalizer) {
		err := adapterError(ErrBackend, 502, "backend did not preserve runtime or cleanup boundary", nil)
		return SandboxRecord{}, OperationReceipt{}, reject(err)
	}
	if record.ArtifactContractDigest != artifactDigest || !sameArtifactExports(record.Artifacts, request.Artifacts) {
		err := adapterError(ErrBackend, 502, "backend did not preserve artifact contract", nil)
		return SandboxRecord{}, OperationReceipt{}, reject(err)
	}
	if created.Metadata.Annotations[CreateIntentAnnotation] != intentDigest || !sameCreateSpec(created, manifest) {
		err := adapterError(ErrConflict, 409, "persisted Sandbox differs from the exact create intent", nil)
		return SandboxRecord{}, OperationReceipt{}, reject(err)
	}
	receipt, err := NewReceipt(request.RequestID, OperationCreate, record, nil, a.now())
	return record, receipt, err
}

func sameCreateSpec(observed, expected kubeSandbox) bool {
	if observed.APIVersion != expected.APIVersion || observed.Kind != expected.Kind ||
		observed.Metadata.Name != expected.Metadata.Name || observed.Metadata.Namespace != expected.Metadata.Namespace ||
		!objectIDPattern.MatchString(observed.Metadata.UID) || !objectIDPattern.MatchString(observed.Metadata.ResourceVersion) ||
		!sameMaterialSpec(observed.RawSpec, expected.Spec) || !sameJSON(observed.Metadata.Finalizers, expected.Metadata.Finalizers) {
		return false
	}
	for key, value := range expected.Metadata.Labels {
		if observed.Metadata.Labels[key] != value {
			return false
		}
	}
	for key, value := range expected.Metadata.Annotations {
		if observed.Metadata.Annotations[key] != value {
			return false
		}
	}
	return true
}

func sameJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func sameMaterialSpec(observed json.RawMessage, expected any) bool {
	if len(observed) == 0 {
		return false
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return false
	}
	left, leftOK := decodeMaterialJSON(observed)
	right, rightOK := decodeMaterialJSON(expectedJSON)
	if !leftOK || !rightOK {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func decodeMaterialJSON(data []byte) (any, bool) {
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return nil, false
	}
	return decoded, true
}

func (a *Adapter) rejectCreated(ctx context.Context, record SandboxRecord, original error) error {
	metadata := kubeMetadata{Name: record.Name, Namespace: record.Namespace, UID: record.UID, ResourceVersion: record.ResourceVersion}
	return a.rejectCreatedMetadata(ctx, record.Name, metadata, original)
}

func (a *Adapter) rejectCreatedMetadata(ctx context.Context, expectedName string, metadata kubeMetadata, original error) error {
	if metadata.Namespace != Namespace || metadata.Name != expectedName || !dnsLabelPattern.MatchString(metadata.Name) ||
		!objectIDPattern.MatchString(metadata.UID) || !objectIDPattern.MatchString(metadata.ResourceVersion) {
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox identity cannot be cleaned safely", original)
	}
	options := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]string{"uid": metadata.UID, "resourceVersion": metadata.ResourceVersion}}
	if err := a.call(ctx, http.MethodDelete, a.resourcePath(metadata.Name), nil, options, nil, "application/json"); err != nil {
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox deletion failed", err)
	}
	var deleting kubeSandbox
	if err := a.call(ctx, http.MethodGet, a.resourcePath(metadata.Name), nil, nil, &deleting, ""); err != nil {
		var adapterErr *AdapterError
		if errors.As(err, &adapterErr) && adapterErr.Code == ErrNotFound {
			return original
		}
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox cleanup state is unavailable", err)
	}
	if deleting.Metadata.Namespace != Namespace || deleting.Metadata.Name != metadata.Name || deleting.Metadata.UID != metadata.UID ||
		!objectIDPattern.MatchString(deleting.Metadata.ResourceVersion) || deleting.Metadata.DeletionTimestamp == "" {
		return adapterError(ErrCleanupIncomplete, 502, "rejected Sandbox cleanup identity drifted", original)
	}
	finalizers := make([]string, 0, len(deleting.Metadata.Finalizers))
	for _, finalizer := range deleting.Metadata.Finalizers {
		if finalizer != CleanupFinalizer {
			finalizers = append(finalizers, finalizer)
		}
	}
	if contains(deleting.Metadata.Finalizers, CleanupFinalizer) {
		patch := map[string]any{"metadata": map[string]any{"resourceVersion": deleting.Metadata.ResourceVersion, "finalizers": finalizers}}
		if err := a.call(ctx, http.MethodPatch, a.resourcePath(metadata.Name), nil, patch, nil, "application/merge-patch+json"); err != nil {
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
	notice := ""
	if record.TrustLevel == TrustApprovedPOC {
		notice = OrchestrationNotice
	}
	return SandboxStatus{State: state, Reason: reason, Message: message, IsolationNotice: notice, Ready: ready, ResourceVersion: object.Metadata.ResourceVersion, ObservedGeneration: object.Status.ObservedGeneration}, nil
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

func (a *Adapter) Delete(ctx context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion, artifactContractDigest string) (OperationReceipt, error) {
	if !requestPattern.MatchString(requestID) || uid == "" || resourceVersion == "" || !digestPattern.MatchString(artifactContractDigest) {
		return OperationReceipt{}, adapterError(ErrInvalidRequest, 400, "delete precondition is invalid", nil)
	}
	record, err := a.Get(ctx, workspaceID, ownerID, name)
	if err != nil {
		return OperationReceipt{}, err
	}
	if record.UID != uid || record.ResourceVersion != resourceVersion {
		return OperationReceipt{}, adapterError(ErrResourceVersionStale, 409, "delete precondition does not match", nil)
	}
	if record.ArtifactContractDigest != artifactContractDigest {
		return OperationReceipt{}, adapterError(ErrConflict, 409, "artifact contract precondition does not match", nil)
	}
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]string{"uid": uid, "resourceVersion": resourceVersion}}
	if err := a.call(ctx, http.MethodDelete, a.resourcePath(name), nil, body, nil, "application/json"); err != nil {
		return OperationReceipt{}, err
	}
	record.State = StateStopping
	record.Deleting = true
	return NewReceipt(requestID, OperationDelete, record, nil, a.now())
}

func (a *Adapter) Finalize(ctx context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion string, expectedArtifacts []ArtifactExport, artifactContractDigest string) (OperationReceipt, error) {
	return a.finalize(ctx, requestID, workspaceID, ownerID, name, uid, resourceVersion, expectedArtifacts, nil, false, artifactContractDigest)
}

func (a *Adapter) FinalizePreExported(ctx context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion string,
	expectedArtifacts []ArtifactExport, exported []ArtifactReceipt, artifactContractDigest string) (OperationReceipt, error) {
	return a.finalize(ctx, requestID, workspaceID, ownerID, name, uid, resourceVersion, expectedArtifacts, exported, true, artifactContractDigest)
}

func (a *Adapter) finalize(ctx context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion string,
	expectedArtifacts []ArtifactExport, artifacts []ArtifactReceipt, preExported bool, artifactContractDigest string) (OperationReceipt, error) {
	if !requestPattern.MatchString(requestID) {
		return OperationReceipt{}, adapterError(ErrInvalidRequest, 400, "finalize request ID is invalid", nil)
	}
	canonicalExpected, expectedDigest, err := CanonicalArtifactContract(expectedArtifacts)
	if err != nil || artifactContractDigest != expectedDigest {
		return OperationReceipt{}, adapterError(ErrInvalidRequest, 400, "trusted artifact contract precondition is invalid", err)
	}
	record, err := a.Get(ctx, workspaceID, ownerID, name)
	if err != nil {
		return OperationReceipt{}, err
	}
	if record.UID != uid || record.ResourceVersion != resourceVersion || !record.Deleting || !contains(record.Finalizers, CleanupFinalizer) {
		return OperationReceipt{}, adapterError(ErrCleanupIncomplete, 409, "sandbox is not ready for receipted cleanup", nil)
	}
	if record.ArtifactContractDigest != artifactContractDigest || !sameArtifactExports(record.Artifacts, canonicalExpected) {
		return OperationReceipt{}, adapterError(ErrConflict, 409, "persisted artifact contract differs from trusted precondition", nil)
	}
	if preExported {
		artifacts = append([]ArtifactReceipt(nil), artifacts...)
	} else {
		artifacts, err = a.exporter.Export(ctx, record, record.Artifacts)
		if err != nil {
			return OperationReceipt{}, adapterError(ErrArtifactExport, 502, "artifact export did not complete", err)
		}
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
	if updatedRecord.UID != uid || !updatedRecord.Deleting || contains(updatedRecord.Finalizers, CleanupFinalizer) || updatedRecord.ArtifactContractDigest != artifactContractDigest || !sameArtifactExports(updatedRecord.Artifacts, canonicalExpected) {
		return OperationReceipt{}, adapterError(ErrCleanupIncomplete, 409, "backend did not complete cleanup finalizer transition", nil)
	}
	updatedRecord.State = StateDeleted
	return NewReceipt(requestID, OperationFinalize, updatedRecord, artifacts, a.now())
}

func render(request CreateRequest, artifactContractDigest, createIntentDigest string) kubeSandbox {
	labels := map[string]string{ManagedLabel: "true", WorkspaceLabel: request.WorkspaceID, OwnerLabel: request.OwnerID, SandboxIDLabel: request.Name}
	podLabels := cloneMap(labels)
	podLabels[QueueLabel] = QueueName
	annotations := map[string]string{"sandboxes.blazn.dev/trust-level": string(request.TrustLevel), "sandboxes.blazn.dev/expires-at": request.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	if encoded, err := json.Marshal(request.Artifacts); err == nil {
		annotations["sandboxes.blazn.dev/artifact-exports"] = string(encoded)
	}
	annotations["sandboxes.blazn.dev/artifact-contract-digest"] = artifactContractDigest
	annotations[CreateIntentAnnotation] = createIntentDigest
	return kubeSandbox{
		APIVersion: APIVersion, Kind: Kind,
		Metadata: kubeMetadata{Name: request.Name, Namespace: Namespace, Labels: labels, Annotations: annotations, Finalizers: []string{CleanupFinalizer}},
		Spec:     kubeSandboxSpec{ShutdownPolicy: "Delete", OperatingMode: "Running", PodTemplate: kubePodTemplate{Metadata: kubePodMetadata{Labels: podLabels}, Spec: renderPodSpec(request)}},
	}
}

func renderPodSpec(request CreateRequest) kubePodSpec {
	mainMounts := make([]kubeVolumeMount, 0, len(request.Sources)+1)
	volumes := make([]kubeVolume, 0, len(request.Sources)+2)
	initContainers := []kubeContainer{}
	if len(request.Sources) > 0 {
		bootstrapMounts := make([]kubeVolumeMount, 0, len(request.Sources)+1)
		for index, source := range request.Sources {
			name := fmt.Sprintf("source-%02d", index)
			volumes = append(volumes, kubeVolume{Name: name, EmptyDir: map[string]any{"sizeLimit": request.EphemeralStorageLimit}})
			bootstrapMounts = append(bootstrapMounts, kubeVolumeMount{Name: name, MountPath: source.Destination})
			mainMounts = append(mainMounts, kubeVolumeMount{Name: name, MountPath: source.Destination, ReadOnly: !source.Writable})
		}
		volumes = append(volumes, kubeVolume{Name: "bootstrap-state", EmptyDir: map[string]any{"medium": "Memory", "sizeLimit": "1Mi"}})
		bootstrapMounts = append(bootstrapMounts, kubeVolumeMount{Name: "bootstrap-state", MountPath: "/run/blazn-bootstrap"})
		initContainers = append(initContainers, helperContainer(sandboxio.BootstrapContainer, request.HelperImage,
			[]string{sandboxio.HelperBinary, "wait-bootstrap"}, "", bootstrapMounts))
	}
	if len(request.Artifacts) > 0 {
		volumes = append(volumes, kubeVolume{Name: "artifacts", EmptyDir: map[string]any{"sizeLimit": request.EphemeralStorageLimit}})
		mainMounts = append(mainMounts, kubeVolumeMount{Name: "artifacts", MountPath: "/workspace/artifacts"})
		initContainers = append(initContainers, helperContainer(sandboxio.ArtifactContainer, request.HelperImage,
			[]string{sandboxio.HelperBinary, "wait-artifact"}, "Always",
			[]kubeVolumeMount{{Name: "artifacts", MountPath: "/workspace/artifacts", ReadOnly: true}}))
	}
	return kubePodSpec{
		RuntimeClassName: request.RuntimeClassName, ServiceAccountName: ServiceAccountName, AutomountServiceAccountToken: false,
		RestartPolicy: "Never", NodeSelector: map[string]string{"kubernetes.io/arch": request.Architecture, "blazn.dev/sandbox-eligible": "true"},
		SecurityContext: map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532), "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		Containers: []kubeContainer{{Name: "main", Image: request.Image, Command: append([]string(nil), request.Command...),
			SecurityContext: restrictedContainerSecurity(),
			Resources: map[string]map[string]string{
				"requests": {"cpu": request.CPURequest, "memory": request.MemoryRequest, "ephemeral-storage": request.EphemeralStorageRequest},
				"limits":   {"cpu": request.CPULimit, "memory": request.MemoryLimit, "ephemeral-storage": request.EphemeralStorageLimit},
			}, VolumeMounts: mainMounts}},
		InitContainers: initContainers, Volumes: volumes,
	}
}

func helperContainer(name, image string, command []string, restartPolicy string, mounts []kubeVolumeMount) kubeContainer {
	return kubeContainer{Name: name, Image: image, Command: command, RestartPolicy: restartPolicy,
		SecurityContext: restrictedContainerSecurity(),
		Resources: map[string]map[string]string{
			"requests": {"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"},
			"limits":   {"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"},
		}, VolumeMounts: mounts}
}

func restrictedContainerSecurity() map[string]any {
	return map[string]any{"allowPrivilegeEscalation": false, "privileged": false, "readOnlyRootFilesystem": true, "capabilities": map[string][]string{"drop": {"ALL"}}}
}

func (a *Adapter) record(object kubeSandbox) (SandboxRecord, error) {
	if object.APIVersion != APIVersion || object.Kind != Kind || object.Metadata.Namespace != Namespace || !dnsLabelPattern.MatchString(object.Metadata.Name) ||
		!objectIDPattern.MatchString(object.Metadata.UID) || !objectIDPattern.MatchString(object.Metadata.ResourceVersion) {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend Sandbox identity is invalid", nil)
	}
	artifactValue, artifactDigest := object.Metadata.Annotations["sandboxes.blazn.dev/artifact-exports"], object.Metadata.Annotations["sandboxes.blazn.dev/artifact-contract-digest"]
	createIntent := object.Metadata.Annotations[CreateIntentAnnotation]
	if !digestPattern.MatchString(createIntent) {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend create intent digest is invalid", nil)
	}
	trimmedArtifacts := strings.TrimSpace(artifactValue)
	artifacts := []ArtifactExport{}
	if len(trimmedArtifacts) < 2 || trimmedArtifacts[0] != '[' || trimmedArtifacts[len(trimmedArtifacts)-1] != ']' || len(artifactValue) > 32768 || json.Unmarshal([]byte(artifactValue), &artifacts) != nil {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend artifact contract is invalid", nil)
	}
	if err := validateArtifactExports(artifacts); err != nil {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend artifact contract violates the frozen export boundary", err)
	}
	canonicalArtifacts, computedDigest, err := CanonicalArtifactContract(artifacts)
	if err != nil || artifactDigest != computedDigest {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend artifact contract digest is invalid", err)
	}
	trustLevel := TrustLevel(object.Metadata.Annotations["sandboxes.blazn.dev/trust-level"])
	if trustLevel != TrustApprovedPOC && trustLevel != TrustUntrusted {
		return SandboxRecord{}, adapterError(ErrBackend, 502, "backend trust level is invalid", nil)
	}
	state, _, _, _ := stateFrom(object)
	return SandboxRecord{
		Name: object.Metadata.Name, Namespace: object.Metadata.Namespace, UID: object.Metadata.UID, ResourceVersion: object.Metadata.ResourceVersion,
		Generation: object.Metadata.Generation, WorkspaceID: object.Metadata.Labels[WorkspaceLabel], OwnerID: object.Metadata.Labels[OwnerLabel],
		QueueName: object.Spec.PodTemplate.Metadata.Labels[QueueLabel], RuntimeClassName: object.Spec.PodTemplate.Spec.RuntimeClassName,
		TrustLevel: trustLevel, State: state,
		Deleting: object.Metadata.DeletionTimestamp != "", Finalizers: append([]string(nil), object.Metadata.Finalizers...), Artifacts: canonicalArtifacts, ArtifactContractDigest: artifactDigest, CreateIntentDigest: createIntent, Labels: cloneMap(object.Metadata.Labels),
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
	decoder := json.NewDecoder(io.LimitReader(response.Body, a.maxBytes+1))
	if err := decoder.Decode(output); err != nil {
		return adapterError(ErrBackend, 502, "Kubernetes API response is invalid", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return adapterError(ErrBackend, 502, "Kubernetes API response is invalid", errors.New("response contains trailing or oversized data"))
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
	return adapterError(code, httpStatus, detail, &kubeHTTPStatusError{statusCode: response.StatusCode})
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
	prefix := "workspaces/" + sandbox.WorkspaceID + "/sandboxes/" + sandbox.Name + "/artifacts/"
	byName := map[string]ArtifactReceipt{}
	for _, receipt := range receipts {
		_, wasRequested := requested[receipt.Name]
		if _, exists := byName[receipt.Name]; exists || !wasRequested || receipt.SchemaVersion != ArtifactSchema || !digestPattern.MatchString(receipt.SHA256) || receipt.ObjectKey != prefix+receipt.Name || path.Clean(receipt.ObjectKey) != receipt.ObjectKey || strings.Contains(receipt.ObjectKey, "..") || receipt.Size < 0 {
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

func sameArtifactExports(left, right []ArtifactExport) bool {
	leftCanonical, leftDigest, leftErr := CanonicalArtifactContract(left)
	rightCanonical, rightDigest, rightErr := CanonicalArtifactContract(right)
	if leftErr != nil || rightErr != nil || leftDigest != rightDigest || len(leftCanonical) != len(rightCanonical) {
		return false
	}
	for index := range leftCanonical {
		if leftCanonical[index] != rightCanonical[index] {
			return false
		}
	}
	return true
}
