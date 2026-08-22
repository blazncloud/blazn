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

	"github.com/KingJammin/blazn/internal/client"
)

const (
	RootHelperSchema      = "blazn.dev/node-root-helper/v1"
	DefaultRootHelperPath = "/usr/local/libexec/blazn-node-helper"
)

type RootOperation string

const (
	RootProbe        RootOperation = "probe"
	RootAuthorize    RootOperation = "authorize_bootstrap"
	RootServiceState RootOperation = "service_state"
	RootCapture      RootOperation = "capture"
	RootApply        RootOperation = "apply"
	RootRollback     RootOperation = "rollback"
	RootVerify       RootOperation = "verify"
	RootJoin         RootOperation = "join"
)

type RootRequest struct {
	SchemaVersion string                 `json:"schemaVersion"`
	Operation     RootOperation          `json:"operation"`
	Platform      string                 `json:"platform"`
	Plan          client.NodeInstallPlan `json:"plan"`
	Ordinal       int64                  `json:"ordinal,omitempty"`
	BackupRoot    string                 `json:"backupRoot,omitempty"`
	Prior         *PriorState            `json:"prior,omitempty"`
	Material      *RootMaterial          `json:"material,omitempty"`
	Join          *RootJoinBinding       `json:"join,omitempty"`
	Bootstrap     *RootBootstrapRequest  `json:"bootstrap,omitempty"`
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
	Credential       string `json:"credential"`
	ClusterID        string `json:"clusterId"`
	ExpectedNodeName string `json:"expectedNodeName"`
	BootstrapTaint   string `json:"bootstrapTaint"`
	WorkerOnly       bool   `json:"workerOnly"`
	ExpectedNodeUID  string `json:"expectedNodeUid,omitempty"`
}
type RootResponse struct {
	SchemaVersion   string             `json:"schemaVersion"`
	OK              bool               `json:"ok"`
	Prior           *PriorState        `json:"prior,omitempty"`
	Service         *ServicePriorState `json:"service,omitempty"`
	NodeUID         string             `json:"nodeUid,omitempty"`
	NodeName        string             `json:"nodeName,omitempty"`
	ResourceVersion string             `json:"resourceVersion,omitempty"`
	ErrorCode       string             `json:"errorCode,omitempty"`
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
	path, args := c.HelperPath, []string{}
	if c.UseSudo {
		path, args = "/usr/bin/sudo", []string{"-n", DefaultRootHelperPath}
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
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.SchemaVersion != RootHelperSchema || !response.OK {
		return RootResponse{}, errors.New("root helper returned an invalid response")
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
	Platform   string
	Privileged PrivilegedClient
	Materials  MaterialResolver
	Join       JoinCoordinator
	plan       client.NodeInstallPlan
	deferred   []client.NodeInstallMutation
	joined     *RootJoinBinding
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
	a.deferred = nil
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
	request := a.request(RootCapture, a.plan, mutation.Ordinal)
	request.BackupRoot = backupRoot
	response, err := a.Privileged.Call(ctx, request)
	if err != nil || response.Prior == nil {
		return PriorState{}, errors.New("capture mutation state failed")
	}
	return *response.Prior, nil
}
func (a *PlatformAdapter) Apply(ctx context.Context, mutation client.NodeInstallMutation) error {
	if mutation.Kind == "label" || mutation.Kind == "taint" {
		a.deferred = append(a.deferred, mutation)
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
	_, err := a.Privileged.Call(ctx, request)
	return err
}
func (a *PlatformAdapter) Verify(ctx context.Context, plan client.NodeInstallPlan) error {
	binding, err := a.Join.WorkerCredential(ctx, plan)
	if err != nil {
		return err
	}
	if !binding.WorkerOnly || binding.ClusterID != plan.Cluster.ID || binding.ExpectedNodeName != plan.Hostname || binding.BootstrapTaint != plan.Cluster.BootstrapTaint {
		return errors.New("worker join credential does not bind the verified plan")
	}
	request := a.request(RootJoin, plan, 0)
	request.Join = &binding
	response, err := a.Privileged.Call(ctx, request)
	if err != nil || response.NodeUID == "" || response.NodeName != binding.ExpectedNodeName || response.ResourceVersion == "" {
		return errors.New("worker join failed")
	}
	binding.ExpectedNodeUID = response.NodeUID
	a.joined = &binding
	for _, mutation := range a.deferred {
		request = a.request(RootApply, plan, mutation.Ordinal)
		request.Join = &RootJoinBinding{ClusterID: binding.ClusterID, ExpectedNodeName: binding.ExpectedNodeName, ExpectedNodeUID: response.NodeUID, BootstrapTaint: binding.BootstrapTaint, WorkerOnly: true}
		if _, err := a.Privileged.Call(ctx, request); err != nil {
			return err
		}
	}
	if err := a.Join.ConfirmJoined(ctx, plan, JoinedNode{Name: response.NodeName, UID: response.NodeUID, ResourceVersion: response.ResourceVersion}); err != nil {
		return err
	}
	request = a.request(RootVerify, plan, 0)
	request.Join = &RootJoinBinding{ClusterID: binding.ClusterID, ExpectedNodeName: binding.ExpectedNodeName, ExpectedNodeUID: response.NodeUID, BootstrapTaint: binding.BootstrapTaint, WorkerOnly: true}
	_, err = a.Privileged.Call(ctx, request)
	return err
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
