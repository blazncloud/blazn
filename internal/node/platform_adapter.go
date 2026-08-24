package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

const (
	RootHelperSchema      = "blazn.dev/node-root-helper/v1"
	DefaultRootHelperPath = "/usr/local/bin/blazn"
	RootHelperSubcommand  = "node-root-helper"
	RootObserveSubcommand = "node-root-observe"
)

type RootOperation string

const (
	RootProbe           RootOperation = "probe"
	RootAuthorize       RootOperation = "authorize_bootstrap"
	RootServiceState    RootOperation = "service_state"
	RootCapture         RootOperation = "capture"
	RootApply           RootOperation = "apply"
	RootRollback        RootOperation = "rollback"
	RootVerify          RootOperation = "verify"
	RootObserve         RootOperation = "observe"
	RootJoin            RootOperation = "join"
	RootAbortJoin       RootOperation = "abort_join_intent"
	RootQuarantineJoin  RootOperation = "quarantine_joined_node"
	RootReleaseCapacity RootOperation = "release_node_capacity"
	RootFinalizeState   RootOperation = "finalize_service_state"
	RootRemoveSupport   RootOperation = "remove_service_support"
	RootCreateWAL       RootOperation = "create_wal"
	RootSaveWAL         RootOperation = "save_wal"
	RootLoadWAL         RootOperation = "load_wal"
	RootRemoveWAL       RootOperation = "remove_wal"
	RootSaveReceipt     RootOperation = "save_receipt"
	RootLoadReceipt     RootOperation = "load_receipt"
)

type RootRequest struct {
	SchemaVersion string                     `json:"schemaVersion"`
	Operation     RootOperation              `json:"operation"`
	Platform      string                     `json:"platform"`
	Plan          client.NodeInstallPlan     `json:"plan"`
	Ordinal       int64                      `json:"ordinal,omitempty"`
	BackupRoot    string                     `json:"backupRoot,omitempty"`
	Prior         *PriorState                `json:"prior,omitempty"`
	Material      *RootMaterial              `json:"material,omitempty"`
	Join          *RootJoinBinding           `json:"join,omitempty"`
	Bootstrap     *RootBootstrapRequest      `json:"bootstrap,omitempty"`
	WAL           *InstallWAL                `json:"wal,omitempty"`
	Receipt       *client.NodeInstallReceipt `json:"receipt,omitempty"`
}
type RootBootstrapRequest struct {
	EnrollmentID       string                                `json:"enrollmentId"`
	Token              string                                `json:"token"`
	MachineFingerprint string                                `json:"machineFingerprint"`
	NodePublicKey      string                                `json:"nodePublicKey"`
	Platform           client.NodePlatform                   `json:"platform"`
	Architecture       client.NodeArchitecture               `json:"architecture"`
	KubernetesBinding  *client.KubernetesBinding             `json:"kubernetesBinding,omitempty"`
	PlanSigningKey     client.NodePlanSigningKey             `json:"planSigningKey"`
	Expected           client.ExchangeNodeEnrollmentResponse `json:"expected"`
	ProfileID          string                                `json:"profileId"`
	ProfilePath        string                                `json:"profilePath"`
}

func (RootBootstrapRequest) String() string   { return "root bootstrap request [REDACTED]" }
func (RootBootstrapRequest) GoString() string { return "node.RootBootstrapRequest{Token:[REDACTED]}" }

type RootMaterial struct {
	ComponentName string `json:"componentName"`
	SHA256        string `json:"sha256"`
	ContentBase64 string `json:"contentBase64,omitempty"`
}
type RootJoinBinding struct {
	Credential              string `json:"credential"`
	ClusterID               string `json:"clusterId"`
	ExpectedNodeName        string `json:"expectedNodeName"`
	BootstrapTaint          string `json:"bootstrapTaint"`
	WorkerOnly              bool   `json:"workerOnly"`
	ExpectedNodeUID         string `json:"expectedNodeUid,omitempty"`
	ExpectedResourceVersion string `json:"expectedResourceVersion,omitempty"`
}
type RootResponse struct {
	SchemaVersion     string                     `json:"schemaVersion"`
	OK                bool                       `json:"ok"`
	Prior             *PriorState                `json:"prior,omitempty"`
	Service           *ServicePriorState         `json:"service,omitempty"`
	NodeUID           string                     `json:"nodeUid,omitempty"`
	NodeName          string                     `json:"nodeName,omitempty"`
	ResourceVersion   string                     `json:"resourceVersion,omitempty"`
	KubernetesBinding *client.KubernetesBinding  `json:"kubernetesBinding,omitempty"`
	WAL               *InstallWAL                `json:"wal,omitempty"`
	Receipt           *client.NodeInstallReceipt `json:"receipt,omitempty"`
	Observation       *RootNodeObservation       `json:"observation,omitempty"`
	ErrorCode         string                     `json:"errorCode,omitempty"`
}

// RootNodeObservation is the deliberately narrow privileged snapshot exposed
// to the unprivileged daemon. It contains no kubeconfig, token, endpoint, pod,
// workload, or arbitrary object data.
type RootNodeObservation struct {
	Binding                client.KubernetesBinding `json:"binding"`
	AllocatableCPUMillis   int64                    `json:"allocatableCpuMillis"`
	AllocatableMemoryBytes int64                    `json:"allocatableMemoryBytes"`
	AllocatableDiskBytes   int64                    `json:"allocatableDiskBytes"`
	ServiceActive          bool                     `json:"serviceActive"`
	NodeReady              bool                     `json:"nodeReady"`
	Pressure               bool                     `json:"pressure"`
	RuntimeClasses         []string                 `json:"runtimeClasses"`
	SandboxBackends        []string                 `json:"sandboxBackends"`
	ReasonCodes            []string                 `json:"reasonCodes"`
	Plan                   RootObservedPlan         `json:"plan"`
	Identity               RootObservedIdentity     `json:"identity"`
}

// RootObservedIdentity is the public identity tuple authorized by the root
// authority and active receipt. It contains no private key or credential.
type RootObservedIdentity struct {
	PublicKey                string `json:"publicKey"`
	PublicKeyFingerprint     string `json:"publicKeyFingerprint"`
	SigningKeyID             string `json:"signingKeyId"`
	Generation               int64  `json:"generation"`
	EnrollmentID             string `json:"enrollmentId"`
	NodeID                   string `json:"nodeId"`
	WorkspaceID              string `json:"workspaceId"`
	ControlPlaneOriginDigest string `json:"controlPlaneOriginDigest"`
}

// RootObservedPlan exposes only public signed-plan provenance. It deliberately
// excludes credentials, origins, mutations, rollback material, and key bytes.
type RootObservedPlan struct {
	PlanID    string `json:"planId"`
	ExpiresAt string `json:"expiresAt"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
}

type PrivilegedClient interface {
	Call(context.Context, RootRequest) (RootResponse, error)
}
type MaterialResolver interface {
	Resolve(context.Context, client.NodeInstallComponent) ([]byte, error)
}
type JoinedNode struct{ Name, UID, ResourceVersion string }
type JoinCoordinator interface {
	WorkerCredential(context.Context, client.NodeInstallPlan) (RootJoinBinding, error)
	ConfirmJoined(context.Context, client.NodeInstallPlan, JoinedNode) error
}

type PipePrivilegedClient struct {
	HelperPath string
	UseSudo    bool
	Timeout    time.Duration
}

type PipeObservationClient struct {
	HelperPath string
	Timeout    time.Duration
}

func (c PipeObservationClient) Call(ctx context.Context, request RootRequest) (RootResponse, error) {
	if request.Operation != RootObserve || request.SchemaVersion != RootHelperSchema {
		return RootResponse{}, errors.New("observation helper accepts only the fixed observe operation")
	}
	if c.HelperPath == "" {
		c.HelperPath = DefaultRootHelperPath
	}
	if c.HelperPath != DefaultRootHelperPath || c.Timeout < time.Second || c.Timeout > 2*time.Minute {
		return RootResponse{}, errors.New("observation helper configuration is invalid")
	}
	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, "/usr/bin/sudo", "-n", c.HelperPath, RootObserveSubcommand)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	var stdout bytes.Buffer
	command.Stdout = &limitedOutput{writer: &stdout, remaining: 2 << 20}
	command.Stderr = &limitedOutput{writer: &bytes.Buffer{}, remaining: 4096}
	if err := command.Run(); err != nil {
		return RootResponse{}, errors.New("root observation helper failed")
	}
	return decodeRootResponse(&stdout)
}

func (c PipePrivilegedClient) Call(ctx context.Context, request RootRequest) (RootResponse, error) {
	var response RootResponse
	if c.HelperPath == "" {
		c.HelperPath = DefaultRootHelperPath
	}
	if c.HelperPath != DefaultRootHelperPath || c.Timeout < time.Second || c.Timeout > 2*time.Minute {
		return response, errors.New("root helper configuration is invalid")
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 2<<20 {
		return response, errors.New("root helper request is invalid")
	}
	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	path, args := c.HelperPath, []string{RootHelperSubcommand}
	if c.UseSudo {
		path, args = "/usr/bin/sudo", []string{"-n", DefaultRootHelperPath, RootHelperSubcommand}
	}
	command := exec.CommandContext(runCtx, path, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin = bytes.NewReader(encoded)
	var stdout bytes.Buffer
	command.Stdout = &limitedOutput{writer: &stdout, remaining: 2 << 20}
	command.Stderr = &limitedOutput{writer: &bytes.Buffer{}, remaining: 4096}
	if err := command.Run(); err != nil {
		return response, errors.New("root helper operation failed")
	}
	return decodeRootResponse(&stdout)
}

func decodeRootResponse(input io.Reader) (RootResponse, error) {
	var response RootResponse
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.SchemaVersion != RootHelperSchema || !response.OK {
		return RootResponse{}, errors.New("root helper returned an invalid response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RootResponse{}, errors.New("root helper returned trailing output")
	}
	return response, nil
}

type limitedOutput struct {
	writer    *bytes.Buffer
	remaining int
}

func (l *limitedOutput) Write(value []byte) (int, error) {
	if len(value) > l.remaining {
		return 0, errors.New("root helper output exceeded limit")
	}
	l.remaining -= len(value)
	return l.writer.Write(value)
}

type PlatformAdapter struct {
	Platform         string
	Privileged       PrivilegedClient
	Materials        MaterialResolver
	Join             JoinCoordinator
	plan             client.NodeInstallPlan
	joined           *RootJoinBinding
	joinConfirmed    bool
	checkpoint       func(string) error
	bootstrapBinding *client.KubernetesBinding
}

func (a *PlatformAdapter) SetInstallCheckpoint(checkpoint func(string) error) {
	a.checkpoint = checkpoint
}
func (a *PlatformAdapter) saveCheckpoint(value string) error {
	if a.checkpoint == nil {
		return nil
	}
	return a.checkpoint(value)
}

func (a *PlatformAdapter) KubernetesBinding() *client.KubernetesBinding {
	if a.joined == nil || a.joined.ExpectedNodeUID == "" || a.joined.ExpectedResourceVersion == "" {
		return nil
	}
	return &client.KubernetesBinding{ClusterID: a.joined.ClusterID, NodeName: a.joined.ExpectedNodeName, NodeUID: a.joined.ExpectedNodeUID, ResourceVersion: a.joined.ExpectedResourceVersion}
}

func (a *PlatformAdapter) FinalizeServiceState(ctx context.Context, plan client.NodeInstallPlan) error {
	_, err := a.Privileged.Call(ctx, a.request(RootFinalizeState, plan, 0))
	return err
}

func (a *PlatformAdapter) ReleaseNodeCapacity(ctx context.Context, plan client.NodeInstallPlan, receipt client.NodeInstallReceipt) (*client.KubernetesBinding, error) {
	if a.joined == nil || a.joined.ExpectedNodeUID == "" || a.joined.ExpectedResourceVersion == "" {
		return nil, errors.New("capacity release requires the verified Kubernetes binding")
	}
	request := a.request(RootReleaseCapacity, plan, 0)
	request.Join = a.joined
	request.Receipt = &receipt
	response, err := a.Privileged.Call(ctx, request)
	if err != nil {
		return nil, err
	}
	if response.KubernetesBinding == nil || response.KubernetesBinding.ClusterID != a.joined.ClusterID || response.KubernetesBinding.NodeName != a.joined.ExpectedNodeName || response.KubernetesBinding.NodeUID != a.joined.ExpectedNodeUID || response.KubernetesBinding.ResourceVersion == "" {
		return nil, errors.New("capacity release lost the root-authorized Kubernetes binding")
	}
	a.joined.ExpectedResourceVersion = response.KubernetesBinding.ResourceVersion
	binding := *response.KubernetesBinding
	return &binding, nil
}
func (a *PlatformAdapter) RemoveServiceSupport(ctx context.Context, plan client.NodeInstallPlan) error {
	_, err := a.Privileged.Call(ctx, a.request(RootRemoveSupport, plan, 0))
	return err
}
func (a *PlatformAdapter) AbortIncompleteJoin(ctx context.Context, plan client.NodeInstallPlan) error {
	if plan.Mode != client.NodeModeFresh {
		return nil
	}
	if a.joined != nil {
		request := a.request(RootQuarantineJoin, plan, 0)
		request.Join = a.joined
		response, err := a.Privileged.Call(ctx, request)
		if err != nil {
			return err
		}
		if response.KubernetesBinding == nil || response.KubernetesBinding.NodeUID != a.joined.ExpectedNodeUID {
			return errors.New("quarantine response lost exact joined UID")
		}
		a.joined.ExpectedResourceVersion = response.KubernetesBinding.ResourceVersion
		return errors.New("joined worker remains quarantined for explicit recovery")
	}
	_, err := a.Privileged.Call(ctx, a.request(RootAbortJoin, plan, 0))
	return err
}

func (a *PlatformAdapter) ReconcileRecovery(ctx context.Context, plan client.NodeInstallPlan) error {
	if plan.Mode != client.NodeModeFresh {
		return nil
	}
	reconciler, ok := a.Join.(interface {
		ReconcileRecovery(context.Context, client.NodeInstallPlan, *JoinedNode) error
	})
	if !ok {
		return errors.New("join recovery coordinator is unavailable")
	}
	if a.joined == nil {
		response, err := a.Privileged.Call(ctx, a.request(RootAbortJoin, plan, 0))
		if err != nil {
			return err
		}
		if response.KubernetesBinding == nil {
			return reconciler.ReconcileRecovery(ctx, plan, nil)
		}
		binding := response.KubernetesBinding
		a.joined = &RootJoinBinding{ClusterID: binding.ClusterID, ExpectedNodeName: binding.NodeName, ExpectedNodeUID: binding.NodeUID, ExpectedResourceVersion: binding.ResourceVersion, BootstrapTaint: plan.Cluster.BootstrapTaint, WorkerOnly: true}
	}
	joined := &JoinedNode{Name: a.joined.ExpectedNodeName, UID: a.joined.ExpectedNodeUID, ResourceVersion: a.joined.ExpectedResourceVersion}
	if err := reconciler.ReconcileRecovery(ctx, plan, joined); err != nil {
		return err
	}
	a.joinConfirmed = true
	return nil
}

func NewPlatformAdapter(platform string, privileged PrivilegedClient, materials MaterialResolver, join JoinCoordinator) (*PlatformAdapter, error) {
	if (platform != "linux" && platform != "macos") || privileged == nil || materials == nil || join == nil {
		return nil, errors.New("platform adapter configuration is incomplete")
	}
	return &PlatformAdapter{Platform: platform, Privileged: privileged, Materials: materials, Join: join}, nil
}
func (a *PlatformAdapter) AuthorizeBootstrap(ctx context.Context, authorization BootstrapAuthorization) error {
	if err := authorization.Validate(); err != nil {
		return err
	}
	request := a.request(RootAuthorize, authorization.Expected.Plan, 0)
	request.Bootstrap = &RootBootstrapRequest{EnrollmentID: authorization.EnrollmentID, Token: authorization.Token, MachineFingerprint: authorization.MachineFingerprint, NodePublicKey: authorization.NodePublicKey, Platform: authorization.Platform, Architecture: authorization.Architecture, KubernetesBinding: authorization.KubernetesBinding, PlanSigningKey: authorization.PlanSigningKey, Expected: authorization.Expected, ProfileID: authorization.ProfileID, ProfilePath: authorization.ProfilePath}
	_, err := a.Privileged.Call(ctx, request)
	if err == nil && authorization.KubernetesBinding != nil {
		binding := *authorization.KubernetesBinding
		a.bootstrapBinding = &binding
	}
	return err
}
func (a *PlatformAdapter) Preflight(ctx context.Context, plan client.NodeInstallPlan) error {
	if err := client.ValidateNodeInstallPlan(plan); err != nil {
		return err
	}
	if (a.Platform == "linux" && plan.Target.Platform != client.NodePlatformLinux) || (a.Platform == "macos" && plan.Target.Platform != client.NodePlatformMacOS) {
		return errors.New("install plan platform differs from adapter")
	}
	response, err := a.Privileged.Call(ctx, a.request(RootProbe, plan, 0))
	if err != nil || !response.OK {
		return errors.New("privileged root helper preflight failed")
	}
	a.plan = plan
	if response.KubernetesBinding != nil {
		binding := *response.KubernetesBinding
		a.bootstrapBinding = &binding
		a.joined = &RootJoinBinding{ClusterID: binding.ClusterID, ExpectedNodeName: binding.NodeName, ExpectedNodeUID: binding.NodeUID, ExpectedResourceVersion: binding.ResourceVersion, BootstrapTaint: plan.Cluster.BootstrapTaint, WorkerOnly: true}
	}
	return nil
}
func (a *PlatformAdapter) ServiceState(ctx context.Context, service client.NodeInstallService) (ServicePriorState, error) {
	response, err := a.Privileged.Call(ctx, a.request(RootServiceState, a.plan, 0))
	if err != nil || response.Service == nil {
		return ServicePriorState{}, errors.New("capture service state failed")
	}
	return *response.Service, nil
}
func (a *PlatformAdapter) Capture(ctx context.Context, mutation client.NodeInstallMutation, backupRoot string) (PriorState, error) {
	if mutation.Kind == "label" || mutation.Kind == "taint" {
		if err := a.ensureJoined(ctx, a.plan); err != nil {
			return PriorState{}, err
		}
	}
	request := a.request(RootCapture, a.plan, mutation.Ordinal)
	request.BackupRoot = backupRoot
	request.Join = a.joined
	response, err := a.Privileged.Call(ctx, request)
	if err != nil || response.Prior == nil {
		return PriorState{}, errors.New("capture mutation state failed")
	}
	return *response.Prior, nil
}
func (a *PlatformAdapter) Apply(ctx context.Context, mutation client.NodeInstallMutation) error {
	if mutation.Kind == "label" || mutation.Kind == "taint" {
		if err := a.ensureJoined(ctx, a.plan); err != nil {
			return err
		}
		request := a.request(RootApply, a.plan, mutation.Ordinal)
		request.Join = a.joined
		response, err := a.Privileged.Call(ctx, request)
		if err != nil {
			return err
		}
		if response.KubernetesBinding == nil || response.KubernetesBinding.NodeUID != a.joined.ExpectedNodeUID || response.KubernetesBinding.NodeName != a.joined.ExpectedNodeName {
			return errors.New("cluster mutation did not return the root-authorized Node binding")
		}
		a.joined.ExpectedResourceVersion = response.KubernetesBinding.ResourceVersion
		return nil
	}
	request := a.request(RootApply, a.plan, mutation.Ordinal)
	material, err := a.material(ctx, mutation)
	if err != nil {
		return err
	}
	request.Material = material
	_, err = a.Privileged.Call(ctx, request)
	return err
}
func (a *PlatformAdapter) Rollback(ctx context.Context, mutation client.NodeInstallMutation, prior PriorState) error {
	request := a.request(RootRollback, a.plan, mutation.Ordinal)
	request.Prior = &prior
	request.BackupRoot = a.plan.Rollback.BackupRoot
	request.Join = a.joined
	response, err := a.Privileged.Call(ctx, request)
	if err == nil && (mutation.Kind == "label" || mutation.Kind == "taint") {
		if response.KubernetesBinding == nil || a.joined == nil || response.KubernetesBinding.NodeUID != a.joined.ExpectedNodeUID || response.KubernetesBinding.NodeName != a.joined.ExpectedNodeName {
			return errors.New("cluster rollback did not return the root-authorized Node binding")
		}
		a.joined.ExpectedResourceVersion = response.KubernetesBinding.ResourceVersion
	}
	return err
}
func (a *PlatformAdapter) Verify(ctx context.Context, plan client.NodeInstallPlan) error {
	if err := a.ensureJoined(ctx, plan); err != nil {
		return err
	}
	binding := *a.joined
	if plan.Mode == client.NodeModeFresh && !a.joinConfirmed {
		if err := a.saveCheckpoint("broker_consume"); err != nil {
			return err
		}
		if err := a.Join.ConfirmJoined(ctx, plan, JoinedNode{Name: binding.ExpectedNodeName, UID: binding.ExpectedNodeUID, ResourceVersion: binding.ExpectedResourceVersion}); err != nil {
			return err
		}
		a.joinConfirmed = true
		if err := a.saveCheckpoint("broker_consumed"); err != nil {
			return err
		}
	}
	if err := a.saveCheckpoint("verify"); err != nil {
		return err
	}
	request := a.request(RootVerify, plan, 0)
	request.Join = &binding
	_, err := a.Privileged.Call(ctx, request)
	return err
}

func (a *PlatformAdapter) ensureJoined(ctx context.Context, plan client.NodeInstallPlan) error {
	if a.joined != nil && a.joined.ExpectedNodeUID != "" && a.joined.ExpectedResourceVersion != "" {
		return nil
	}
	var binding RootJoinBinding
	var response RootResponse
	if plan.Mode == client.NodeModeFresh {
		if err := a.saveCheckpoint("join_intent"); err != nil {
			return err
		}
		issued, err := a.Join.WorkerCredential(ctx, plan)
		if err != nil {
			return err
		}
		binding = issued
		if !binding.WorkerOnly || binding.ClusterID != plan.Cluster.ID || binding.ExpectedNodeName != plan.Hostname || binding.BootstrapTaint != plan.Cluster.BootstrapTaint {
			return errors.New("worker join credential does not bind the verified plan")
		}
		request := a.request(RootJoin, plan, 0)
		request.Join = &binding
		if err := a.saveCheckpoint("join"); err != nil {
			return err
		}
		response, err = a.Privileged.Call(ctx, request)
		if err != nil || response.NodeUID == "" || response.NodeName != binding.ExpectedNodeName || response.ResourceVersion == "" {
			return errors.New("worker join failed")
		}
	} else {
		if a.bootstrapBinding == nil || a.bootstrapBinding.ClusterID != plan.Cluster.ID || a.bootstrapBinding.NodeName != plan.Hostname || a.bootstrapBinding.NodeUID == "" || a.bootstrapBinding.ResourceVersion == "" {
			return errors.New("adopted worker binding does not match root-authorized plan")
		}
		response = RootResponse{NodeUID: a.bootstrapBinding.NodeUID, NodeName: a.bootstrapBinding.NodeName, ResourceVersion: a.bootstrapBinding.ResourceVersion}
		binding = RootJoinBinding{ClusterID: a.bootstrapBinding.ClusterID, ExpectedNodeName: a.bootstrapBinding.NodeName, ExpectedNodeUID: a.bootstrapBinding.NodeUID, ExpectedResourceVersion: a.bootstrapBinding.ResourceVersion, BootstrapTaint: plan.Cluster.BootstrapTaint, WorkerOnly: true}
	}
	binding.ExpectedNodeUID = response.NodeUID
	binding.ExpectedResourceVersion = response.ResourceVersion
	a.joined = &binding
	if err := a.saveCheckpoint("binding"); err != nil {
		return err
	}
	return nil
}
func (a *PlatformAdapter) request(operation RootOperation, plan client.NodeInstallPlan, ordinal int64) RootRequest {
	return RootRequest{SchemaVersion: RootHelperSchema, Operation: operation, Platform: a.Platform, Plan: plan, Ordinal: ordinal}
}
func (a *PlatformAdapter) material(ctx context.Context, mutation client.NodeInstallMutation) (*RootMaterial, error) {
	name, _ := mutation.Desired["sourceComponent"].(string)
	if name == "" {
		name, _ = mutation.Desired["componentName"].(string)
	}
	if name == "" {
		return nil, nil
	}
	for _, component := range a.plan.Components {
		if component.Name == name {
			if component.SourceClass == "https" || component.SourceClass == "current_binary" || (component.SourceClass == "embedded" && mutation.Action == "adopt_exact") {
				return &RootMaterial{ComponentName: name, SHA256: component.SHA256}, nil
			}
			content, err := a.Materials.Resolve(ctx, component)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(content)
			if hex.EncodeToString(sum[:]) != component.SHA256 {
				return nil, errors.New("resolved component digest differs from signed plan")
			}
			return &RootMaterial{ComponentName: name, SHA256: component.SHA256, ContentBase64: base64.RawStdEncoding.EncodeToString(content)}, nil
		}
	}
	return nil, errors.New("mutation references an unknown component")
}

type TrustedMaterialResolver struct {
	Profile           client.NodeTrustedInstallProfile
	CurrentBinaryPath string
	Embedded          map[string][]byte
	HTTP              *http.Client
	MaxBytes          int64
}

func (r TrustedMaterialResolver) Resolve(ctx context.Context, component client.NodeInstallComponent) ([]byte, error) {
	switch component.SourceClass {
	case "current_binary":
		return readBoundedRegular(r.CurrentBinaryPath, r.MaxBytes)
	case "embedded":
		value, ok := r.Embedded[component.Name]
		if !ok {
			return nil, errors.New("embedded component is unavailable")
		}
		return append([]byte(nil), value...), nil
	case "https":
		if r.HTTP == nil {
			return nil, errors.New("HTTPS material client is unavailable")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, component.Source, nil)
		if err != nil {
			return nil, err
		}
		clientCopy := *r.HTTP
		clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return errors.New("too many component redirects")
			}
			return client.ValidateNodeComponentRedirect(r.Profile, component, req.URL.String())
		}
		response, err := clientCopy.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("component download status %d", response.StatusCode)
		}
		limit := r.MaxBytes
		if limit <= 0 {
			limit = 512 << 20
		}
		value, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(value)) > limit {
			return nil, errors.New("component download exceeds limit")
		}
		return value, nil
	default:
		return nil, errors.New("component source class is unsupported")
	}
}
func readBoundedRegular(path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 512 << 20
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("material path is unsafe")
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("material changed while opening")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(value)) > limit {
		return nil, errors.New("material exceeds limit")
	}
	return value, nil
}

func SupportedPlatformAdapter(privileged PrivilegedClient, materials MaterialResolver, join JoinCoordinator) (*PlatformAdapter, error) {
	switch runtime.GOOS {
	case "linux":
		return NewPlatformAdapter("linux", privileged, materials, join)
	case "darwin":
		return NewPlatformAdapter("macos", privileged, materials, join)
	default:
		return nil, fmt.Errorf("unsupported privileged platform %s", runtime.GOOS)
	}
}

func mutationByOrdinal(plan client.NodeInstallPlan, ordinal int64) (client.NodeInstallMutation, error) {
	for _, mutation := range plan.Mutations {
		if mutation.Ordinal == ordinal {
			return mutation, nil
		}
	}
	return client.NodeInstallMutation{}, errors.New("mutation ordinal is not in the signed plan")
}
func sortedMutations(plan client.NodeInstallPlan) []client.NodeInstallMutation {
	values := append([]client.NodeInstallMutation(nil), plan.Mutations...)
	sort.Slice(values, func(i, j int) bool { return values[i].Ordinal < values[j].Ordinal })
	return values
}
func canonicalPath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.Contains(value, "..")
}
