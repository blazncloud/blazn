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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type RootEngine interface {
	Execute(context.Context, RootRequest) (RootResponse, error)
}
type RootRequestAuthorizer interface {
	AuthorizeRootRequest(context.Context, RootRequest) error
}

func RunRootHelper(ctx context.Context, input io.Reader, output io.Writer, engine RootEngine) error {
	if currentUID() != 0 {
		return errors.New("root helper requires UID 0")
	}
	data, err := io.ReadAll(io.LimitReader(input, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("root helper request exceeds limit")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return errors.New("root helper request is invalid")
	}
	allowed := map[string]bool{"schemaVersion": true, "operation": true, "platform": true, "plan": true, "ordinal": true, "backupRoot": true, "prior": true, "material": true, "join": true, "bootstrap": true, "wal": true, "receipt": true}
	for key := range raw {
		if !allowed[key] {
			return fmt.Errorf("root helper field %q is unsupported", key)
		}
	}
	var request RootRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	if request.SchemaVersion != RootHelperSchema || (request.Platform != "linux" && request.Platform != "macos") {
		return errors.New("root helper binding is invalid")
	}
	if err := validateRootRequestShape(request); err != nil {
		return err
	}
	if err := client.ValidateNodeInstallPlan(request.Plan); err != nil {
		return err
	}
	if (request.Platform == "linux" && request.Plan.Target.Platform != client.NodePlatformLinux) || (request.Platform == "macos" && request.Plan.Target.Platform != client.NodePlatformMacOS) {
		return errors.New("root helper plan platform mismatch")
	}
	if request.Operation == RootAuthorize {
		if request.Bootstrap == nil {
			return errors.New("root bootstrap authorization is missing")
		}
	} else {
		authorizer, ok := engine.(RootRequestAuthorizer)
		if !ok {
			return errors.New("root helper authorizer is unavailable")
		}
		if err := authorizer.AuthorizeRootRequest(ctx, request); err != nil {
			return err
		}
		if err := validateRootRequestMaterial(request); err != nil {
			return err
		}
	}
	if engine == nil {
		return errors.New("root helper engine is unavailable")
	}
	response, err := engine.Execute(ctx, request)
	if err != nil {
		return err
	}
	response.SchemaVersion = RootHelperSchema
	response.OK = true
	return json.NewEncoder(output).Encode(response)
}

func validateRootRequestShape(request RootRequest) error {
	noMutationFields := func() bool {
		return request.Ordinal == 0 && request.BackupRoot == "" && request.Prior == nil && request.Material == nil && request.Join == nil && request.WAL == nil && request.Receipt == nil
	}
	switch request.Operation {
	case RootAuthorize:
		if request.Bootstrap == nil || !noMutationFields() {
			return errors.New("root authorize request fields are invalid")
		}
	case RootProbe, RootServiceState, RootFinalizeState, RootObserve, RootRemoveSupport, RootAbortJoin:
		if request.Bootstrap != nil || !noMutationFields() {
			return errors.New("root probe request fields are invalid")
		}
	case RootCapture:
		if request.Bootstrap != nil || request.Ordinal < 1 || request.BackupRoot == "" || request.Prior != nil || request.Material != nil {
			return errors.New("root capture request fields are invalid")
		}
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil || ((mutation.Kind == "label" || mutation.Kind == "taint") != (request.Join != nil)) {
			return errors.New("root capture join binding is invalid")
		}
	case RootApply:
		if request.Bootstrap != nil || request.Ordinal < 1 || request.BackupRoot != "" || request.Prior != nil {
			return errors.New("root apply request fields are invalid")
		}
	case RootRollback:
		if request.Bootstrap != nil || request.Ordinal < 1 || request.BackupRoot == "" || request.Prior == nil || request.Material != nil {
			return errors.New("root rollback request fields are invalid")
		}
	case RootJoin:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join == nil {
			return errors.New("root join request fields are invalid")
		}
	case RootQuarantineJoin:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join == nil || request.WAL != nil || request.Receipt != nil {
			return errors.New("root quarantine request fields are invalid")
		}
	case RootReleaseCapacity:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join == nil || request.WAL != nil || request.Receipt == nil {
			return errors.New("root capacity release request fields are invalid")
		}
	case RootVerify:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join == nil {
			return errors.New("root verify request fields are invalid")
		}
	case RootCreateWAL, RootSaveWAL:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join != nil || request.WAL == nil || request.Receipt != nil {
			return errors.New("root WAL request fields are invalid")
		}
	case RootLoadWAL, RootRemoveWAL, RootLoadReceipt:
		if request.Bootstrap != nil || !noMutationFields() {
			return errors.New("root state request fields are invalid")
		}
	case RootSaveReceipt:
		if request.Bootstrap != nil || request.Ordinal != 0 || request.BackupRoot != "" || request.Prior != nil || request.Material != nil || request.Join != nil || request.WAL != nil || request.Receipt == nil {
			return errors.New("root receipt request fields are invalid")
		}
	default:
		return errors.New("root helper operation is unsupported")
	}
	return nil
}

func validateRootRequestMaterial(request RootRequest) error {
	if request.Operation != RootApply {
		return nil
	}
	mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
	if err != nil {
		return err
	}
	name, _ := mutation.Desired["sourceComponent"].(string)
	if name == "" {
		name, _ = mutation.Desired["componentName"].(string)
	}
	if name == "" {
		if request.Material != nil {
			return errors.New("unrequested root material is present")
		}
		return nil
	}
	if request.Material == nil || request.Material.ComponentName != name {
		return errors.New("root material component binding is invalid")
	}
	for _, component := range request.Plan.Components {
		if component.Name == name {
			if request.Material.SHA256 != component.SHA256 {
				return errors.New("root material digest binding is invalid")
			}
			return nil
		}
	}
	return errors.New("root material component is absent from signed plan")
}

type CommandExecutor interface {
	Run(context.Context, string, ...string) ([]byte, error)
	RunInput(context.Context, string, []byte, ...string) ([]byte, error)
}
type FixedCommandExecutor struct{}
type FixedCommandError struct{ ExitCode int }

func (e *FixedCommandError) Error() string { return "fixed privileged command failed" }

func (FixedCommandExecutor) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return (FixedCommandExecutor{}).RunInput(ctx, path, nil, args...)
}
func (FixedCommandExecutor) RunInput(ctx context.Context, path string, input []byte, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/bin", "LANG=C", "LC_ALL=C", "SNAP=/snap/microk8s/current", "SNAP_DATA=/var/snap/microk8s/current", "SNAP_COMMON=/var/snap/microk8s/common"}
	command.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	command.Stdout = &limitedOutput{writer: &stdout, remaining: 1 << 20}
	command.Stderr = &limitedOutput{writer: &bytes.Buffer{}, remaining: 4096}
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, &FixedCommandError{ExitCode: exitError.ExitCode()}
		}
		return nil, errors.New("fixed privileged command failed")
	}
	return stdout.Bytes(), nil
}

type NativeRootEngine struct {
	Platform             string
	Commands             CommandExecutor
	Now                  func() time.Time
	LimaBindingPath      string
	allowTestJoinRuntime bool
	AuthorityPath        string
	ProfileRoot          string
	CurrentBinaryPath    string
	RootStateRoot        string
	AuthorityHTTPClient  *http.Client
	LookupUser           func(string) (*user.User, error)
	LookupGroup          func(string) (*user.Group, error)
	ObservationIdentity  RootObservedIdentity
}

func (e NativeRootEngine) Execute(ctx context.Context, request RootRequest) (RootResponse, error) {
	if e.Commands == nil {
		e.Commands = FixedCommandExecutor{}
	}
	switch request.Operation {
	case RootAuthorize:
		return RootResponse{}, e.authorizeBootstrap(ctx, request)
	case RootProbe:
		if e.Platform != request.Platform {
			return RootResponse{}, errors.New("root helper OS mismatch")
		}
		binding, err := e.rootKubernetesBinding()
		if err == nil && binding != nil {
			observed, observeErr := e.observeNode(ctx, request.Plan, binding.NodeName)
			if observeErr != nil || observed.UID != binding.NodeUID {
				return RootResponse{}, errors.New("root probe Kubernetes binding differs from authority")
			}
			binding, err = e.updateRootKubernetesBinding(request.Plan, observed)
		}
		return RootResponse{KubernetesBinding: binding}, err
	case RootServiceState:
		return e.serviceState(ctx, request.Plan.NodeService)
	case RootObserve:
		observation, err := e.observeCapability(ctx, request.Plan)
		return RootResponse{Observation: &observation}, err
	case RootCapture:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil {
			return RootResponse{}, err
		}
		prior, err := e.capture(ctx, request.Plan, mutation, request.BackupRoot, request.Join)
		return RootResponse{Prior: &prior}, err
	case RootApply:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil {
			return RootResponse{}, err
		}
		if err := e.apply(ctx, request.Plan, mutation, request.Material, request.Join); err != nil {
			return RootResponse{}, err
		}
		if mutation.Kind == "label" || mutation.Kind == "taint" {
			observed, err := e.observeNode(ctx, request.Plan, request.Join.ExpectedNodeName)
			if err != nil {
				return RootResponse{}, err
			}
			binding, err := e.updateRootKubernetesBinding(request.Plan, observed)
			return RootResponse{KubernetesBinding: binding}, err
		}
		return RootResponse{}, nil
	case RootRollback:
		mutation, err := mutationByOrdinal(request.Plan, request.Ordinal)
		if err != nil || request.Prior == nil {
			return RootResponse{}, errors.New("rollback request is incomplete")
		}
		if err := e.rollback(ctx, request.Plan, mutation, *request.Prior, request.BackupRoot, request.Join); err != nil {
			return RootResponse{}, err
		}
		if mutation.Kind == "label" || mutation.Kind == "taint" {
			observed, err := e.observeNode(ctx, request.Plan, request.Join.ExpectedNodeName)
			if err != nil {
				return RootResponse{}, err
			}
			binding, err := e.updateRootKubernetesBinding(request.Plan, observed)
			return RootResponse{KubernetesBinding: binding}, err
		}
		return RootResponse{}, nil
	case RootJoin:
		if existing, err := e.rootKubernetesBinding(); err != nil {
			return RootResponse{}, err
		} else if existing != nil {
			observed, observeErr := e.observeNode(ctx, request.Plan, existing.NodeName)
			if observeErr != nil || observed.UID != existing.NodeUID {
				return RootResponse{}, errors.New("existing joined Node differs from root authority")
			}
			return RootResponse{NodeUID: observed.UID, NodeName: observed.Name, ResourceVersion: observed.ResourceVersion, KubernetesBinding: existing}, nil
		}
		started, err := e.beginRootJoinIntent(request.Plan, request.Join)
		if err != nil {
			return RootResponse{}, err
		}
		if !started {
			observed, observeErr := e.observeNode(ctx, request.Plan, request.Join.ExpectedNodeName)
			if observeErr != nil {
				return RootResponse{}, errors.New("journaled worker join requires operator recovery before retry")
			}
			binding, bindErr := e.updateRootKubernetesBinding(request.Plan, observed)
			return RootResponse{NodeUID: observed.UID, NodeName: observed.Name, ResourceVersion: observed.ResourceVersion, KubernetesBinding: binding}, bindErr
		}
		joined, err := e.join(ctx, request.Plan, request.Join)
		if err != nil {
			return RootResponse{}, err
		}
		binding, err := e.updateRootKubernetesBinding(request.Plan, joined)
		return RootResponse{NodeUID: joined.UID, NodeName: joined.Name, ResourceVersion: joined.ResourceVersion, KubernetesBinding: binding}, err
	case RootAbortJoin:
		binding, err := e.reconcileRootJoinRecovery(ctx, request.Plan)
		return RootResponse{KubernetesBinding: binding}, err
	case RootQuarantineJoin:
		if err := e.quarantineJoinedNode(ctx, request.Plan, request.Join); err != nil {
			return RootResponse{}, err
		}
		observed, err := e.observeNode(ctx, request.Plan, request.Join.ExpectedNodeName)
		if err != nil {
			return RootResponse{}, err
		}
		binding, err := e.updateRootKubernetesBinding(request.Plan, observed)
		return RootResponse{KubernetesBinding: binding}, err
	case RootReleaseCapacity:
		binding, err := e.releaseNodeCapacity(ctx, request.Plan, request.Join, request.Receipt)
		return RootResponse{KubernetesBinding: binding}, err
	case RootVerify:
		return RootResponse{}, e.verify(ctx, request.Plan, request.Join)
	case RootFinalizeState:
		return RootResponse{}, e.finalizeServiceState(ctx, request.Plan)
	case RootRemoveSupport:
		return RootResponse{}, e.removeServiceSupport(request.Plan)
	case RootCreateWAL, RootSaveWAL, RootLoadWAL, RootRemoveWAL, RootSaveReceipt, RootLoadReceipt:
		return e.executeRootState(request)
	default:
		return RootResponse{}, errors.New("root helper operation is unsupported")
	}
}

func (e NativeRootEngine) observeCapability(ctx context.Context, plan client.NodeInstallPlan) (RootNodeObservation, error) {
	if e.ObservationIdentity.PublicKey == "" || e.ObservationIdentity.PublicKeyFingerprint == "" || e.ObservationIdentity.SigningKeyID == "" || e.ObservationIdentity.Generation < 1 || e.ObservationIdentity.EnrollmentID != plan.EnrollmentID || e.ObservationIdentity.NodeID != plan.NodeID || e.ObservationIdentity.WorkspaceID != plan.WorkspaceID || e.ObservationIdentity.ControlPlaneOriginDigest == "" {
		return RootNodeObservation{}, errors.New("root-authorized node identity observation is unavailable")
	}
	binding, err := e.rootKubernetesBinding()
	if err != nil || binding == nil {
		return RootNodeObservation{}, errors.New("root-authorized Kubernetes binding is unavailable")
	}
	return e.observeCapabilityBinding(ctx, plan, *binding)
}

func (e NativeRootEngine) observeCapabilityBinding(ctx context.Context, plan client.NodeInstallPlan, binding client.KubernetesBinding) (RootNodeObservation, error) {
	output, err := e.kubectl(ctx, plan, "get", "node", binding.NodeName, "-o", "json")
	if err != nil {
		return RootNodeObservation{}, err
	}
	var value struct {
		Metadata struct{ Name, UID, ResourceVersion string } `json:"metadata"`
		Status   struct {
			Allocatable map[string]string               `json:"allocatable"`
			Conditions  []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	}
	if err := decodeSingleJSON(output, &value); err != nil || value.Metadata.Name != binding.NodeName || value.Metadata.UID != binding.NodeUID || value.Metadata.ResourceVersion == "" {
		return RootNodeObservation{}, errors.New("live Kubernetes node observation is invalid")
	}
	cpu, err := parseCPUQuantity(value.Status.Allocatable["cpu"])
	if err != nil {
		return RootNodeObservation{}, errors.New("worker allocatable CPU is invalid")
	}
	memory, err := parseByteQuantity(value.Status.Allocatable["memory"])
	if err != nil {
		return RootNodeObservation{}, errors.New("worker allocatable memory is invalid")
	}
	disk, err := parseByteQuantity(value.Status.Allocatable["ephemeral-storage"])
	if err != nil {
		return RootNodeObservation{}, errors.New("worker allocatable storage is invalid")
	}
	result := RootNodeObservation{Binding: client.KubernetesBinding{ClusterID: binding.ClusterID, NodeName: binding.NodeName, NodeUID: binding.NodeUID, ResourceVersion: value.Metadata.ResourceVersion}, AllocatableCPUMillis: cpu, AllocatableMemoryBytes: memory, AllocatableDiskBytes: disk, RuntimeClasses: []string{}, SandboxBackends: []string{}, ReasonCodes: []string{}, Plan: RootObservedPlan{PlanID: plan.PlanID, ExpiresAt: plan.ExpiresAt, Digest: plan.Digest, Signature: plan.Signature}, Identity: e.ObservationIdentity}
	service, serviceErr := e.serviceState(ctx, plan.NodeService)
	result.ServiceActive = serviceErr == nil && service.Service != nil && service.Service.Active
	if serviceErr != nil {
		result.ReasonCodes = append(result.ReasonCodes, "service_observation_failed")
	}
	for _, condition := range value.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			result.NodeReady = true
		}
		if (condition.Type == "MemoryPressure" || condition.Type == "DiskPressure" || condition.Type == "PIDPressure") && condition.Status == "True" {
			result.Pressure = true
		}
	}
	if classes, classErr := e.kubectl(ctx, plan, "get", "runtimeclass", "-o", "json"); classErr == nil {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if decodeSingleJSON(classes, &list) == nil {
			for _, item := range list.Items {
				if item.Metadata.Name != "" {
					result.RuntimeClasses = append(result.RuntimeClasses, item.Metadata.Name)
				}
			}
		} else {
			result.ReasonCodes = appendUnique(result.ReasonCodes, "runtimeclass_discovery_invalid")
		}
	} else {
		result.ReasonCodes = appendUnique(result.ReasonCodes, "runtimeclass_discovery_failed")
	}
	if resources, resourceErr := e.kubectl(ctx, plan, "api-resources", "-o", "name"); resourceErr == nil {
		if strings.Contains(string(resources), "sandboxes.agents.x-k8s.io") {
			controller, controllerErr := e.kubectl(ctx, plan, "get", "deployment", "agent-sandbox-controller", "-n", "agent-sandbox-system", "-o", "json")
			if controllerErr == nil && agentSandboxControllerAvailable(controller) {
				result.SandboxBackends = append(result.SandboxBackends, "kubernetes-agent-sandbox")
			} else {
				result.ReasonCodes = appendUnique(result.ReasonCodes, "sandbox_controller_unavailable")
			}
		}
	} else {
		result.ReasonCodes = appendUnique(result.ReasonCodes, "api_discovery_failed")
	}
	sort.Strings(result.RuntimeClasses)
	sort.Strings(result.SandboxBackends)
	sort.Strings(result.ReasonCodes)
	return result, nil
}

func decodeSingleJSON(value []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func parseCPUQuantity(value string) (int64, error) {
	if strings.HasSuffix(value, "m") {
		millis, err := strconv.ParseInt(strings.TrimSuffix(value, "m"), 10, 64)
		if err != nil || millis < 1 {
			return 0, errors.New("invalid CPU quantity")
		}
		return millis, nil
	}
	cores, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cores < 1 || cores > (1<<50)/1000 {
		return 0, errors.New("invalid CPU quantity")
	}
	return cores * 1000, nil
}

func parseByteQuantity(value string) (int64, error) {
	multipliers := map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "K": 1000, "M": 1000 * 1000, "G": 1000 * 1000 * 1000}
	for suffix, multiplier := range multipliers {
		if strings.HasSuffix(value, suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(value, suffix), 10, 64)
			if err != nil || n < 1 || n > (1<<62)/multiplier {
				return 0, errors.New("invalid byte quantity")
			}
			return n * multiplier, nil
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 {
		return 0, errors.New("invalid byte quantity")
	}
	return n, nil
}

func (e NativeRootEngine) executeRootState(request RootRequest) (RootResponse, error) {
	paths, err := NodeProductionPaths(request.Plan.Target.Platform)
	if err != nil {
		return RootResponse{}, err
	}
	store := FileStateStore{Root: paths.RootStateRoot}
	switch request.Operation {
	case RootCreateWAL:
		err = store.CreateWAL(*request.WAL)
	case RootSaveWAL:
		err = store.SaveWAL(*request.WAL)
	case RootLoadWAL:
		var wal InstallWAL
		wal, err = store.LoadWAL()
		if errors.Is(err, os.ErrNotExist) {
			return RootResponse{ErrorCode: "not_found"}, nil
		}
		return RootResponse{WAL: &wal}, err
	case RootRemoveWAL:
		err = store.RemoveWAL()
	case RootSaveReceipt:
		err = store.SaveReceipt(*request.Receipt)
	case RootLoadReceipt:
		var receipt client.NodeInstallReceipt
		receipt, err = store.LoadReceipt()
		if errors.Is(err, os.ErrNotExist) {
			return RootResponse{ErrorCode: "not_found"}, nil
		}
		return RootResponse{Receipt: &receipt}, err
	}
	return RootResponse{}, err
}

func (e NativeRootEngine) finalizeServiceState(ctx context.Context, plan client.NodeInstallPlan) error {
	paths, err := NodeProductionPaths(plan.Target.Platform)
	if err != nil {
		return err
	}
	if plan.Target.Platform == client.NodePlatformMacOS {
		if err := e.ensureMacOSServiceIdentity(ctx, plan.NodeService, paths.ServiceStateRoot); err != nil {
			return err
		}
	}
	account, err := user.Lookup(plan.NodeService.RunAsUser)
	if err != nil {
		return errors.New("node service account is unavailable")
	}
	group, err := user.LookupGroup(plan.NodeService.RunAsGroup)
	if err != nil {
		return errors.New("node service group is unavailable")
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(group.Gid)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return errors.New("node service identity must be dedicated and non-root")
	}
	if err := filepath.Walk(paths.ServiceStateRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("node service state contains an unsafe entry")
		}
		mode := os.FileMode(0600)
		if info.IsDir() {
			mode = 0700
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	}); err != nil {
		return err
	}
	policyPath := nodeObservationPolicyPath(plan.Target.Platform)
	policy := nodeObservationPolicy(plan)
	if info, statErr := os.Lstat(policyPath); statErr == nil {
		existing, readErr := readBoundedRegular(policyPath, 4096)
		if readErr != nil {
			return readErr
		}
		owner, _, ownerOK := fileOwner(info)
		if !ownerOK || owner != 0 || info.Mode().Perm() != 0440 || !bytes.Equal(existing, policy) {
			return errors.New("preexisting node observation policy differs from receipt ownership")
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		if err := writeRootAtomic(policyPath, policy, 0440, 0, 0); err != nil {
			return err
		}
	} else {
		return statErr
	}
	_, err = e.Commands.Run(ctx, "/usr/sbin/visudo", "-c", "-f", policyPath)
	return err
}

func (e NativeRootEngine) removeServiceSupport(plan client.NodeInstallPlan) error {
	if plan.NodeService.RunAsUser != "blazn-node" && plan.NodeService.RunAsUser != "_blazn-node" {
		return errors.New("node service identity is invalid")
	}
	path := nodeObservationPolicyPath(plan.Target.Platform)
	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		return nil
	} else if statErr != nil {
		return statErr
	}
	value, err := readBoundedRegular(path, 4096)
	if err != nil {
		return err
	}
	if !bytes.Equal(value, nodeObservationPolicy(plan)) {
		return errors.New("node observation policy is not receipt-bound")
	}
	return os.Remove(path)
}

func nodeObservationPolicy(plan client.NodeInstallPlan) []byte {
	return []byte("# blazn receipt-owned node observation policy " + plan.PlanID + "\n" + plan.NodeService.RunAsUser + " ALL=(root) NOPASSWD: /usr/local/bin/blazn node-root-observe\n")
}

func nodeObservationPolicyPath(platform client.NodePlatform) string {
	if platform == client.NodePlatformMacOS {
		return "/private/etc/sudoers.d/blazn-node-observe"
	}
	return "/etc/sudoers.d/blazn-node-observe"
}

func (e NativeRootEngine) ensureMacOSServiceIdentity(ctx context.Context, service client.NodeInstallService, home string) error {
	if service.RunAsUser != "_blazn-node" || service.RunAsGroup != "_blazn-node" || home != MacOSNodeServiceStateRoot {
		return errors.New("macOS node service identity contract is invalid")
	}
	lookupUser, lookupGroup := e.LookupUser, e.LookupGroup
	if lookupUser == nil {
		lookupUser = user.Lookup
	}
	if lookupGroup == nil {
		lookupGroup = user.LookupGroup
	}
	if account, err := lookupUser(service.RunAsUser); err == nil {
		group, groupErr := lookupGroup(service.RunAsGroup)
		if groupErr != nil || account.Username != service.RunAsUser || account.Uid != "299" || account.Gid != "299" || account.HomeDir != home || group.Name != service.RunAsGroup || group.Gid != "299" {
			return errors.New("existing macOS node service identity differs from the dedicated contract")
		}
		return e.verifyMacOSDSCLIdentity(ctx, home)
	}
	if account, err := user.LookupId("299"); err == nil && account.Username != service.RunAsUser {
		return errors.New("macOS node service UID is already occupied")
	}
	if group, err := user.LookupGroupId("299"); err == nil && group.Name != service.RunAsGroup {
		return errors.New("macOS node service GID is already occupied")
	}
	commands := [][]string{{".", "-create", "/Groups/_blazn-node"}, {".", "-create", "/Groups/_blazn-node", "PrimaryGroupID", "299"}, {".", "-create", "/Users/_blazn-node"}, {".", "-create", "/Users/_blazn-node", "UniqueID", "299"}, {".", "-create", "/Users/_blazn-node", "PrimaryGroupID", "299"}, {".", "-create", "/Users/_blazn-node", "NFSHomeDirectory", home}, {".", "-create", "/Users/_blazn-node", "UserShell", "/usr/bin/false"}, {".", "-create", "/Users/_blazn-node", "IsHidden", "1"}}
	for _, args := range commands {
		if _, err := e.Commands.Run(ctx, "/usr/bin/dscl", args...); err != nil {
			return errors.New("create dedicated macOS node service identity failed")
		}
	}
	return e.verifyMacOSDSCLIdentity(ctx, home)
}

func (e NativeRootEngine) verifyMacOSDSCLIdentity(ctx context.Context, home string) error {
	attributes, attrErr := e.Commands.Run(ctx, "/usr/bin/dscl", ".", "-read", "/Users/_blazn-node", "UniqueID", "PrimaryGroupID", "NFSHomeDirectory", "UserShell", "IsHidden")
	if attrErr != nil || !exactDSCLAttributes(attributes, map[string]string{"UniqueID": "299", "PrimaryGroupID": "299", "NFSHomeDirectory": home, "UserShell": "/usr/bin/false", "IsHidden": "1"}) {
		return errors.New("existing macOS node service account properties differ from contract")
	}
	groupAttributes, groupAttrErr := e.Commands.Run(ctx, "/usr/bin/dscl", ".", "-read", "/Groups/_blazn-node", "PrimaryGroupID")
	if groupAttrErr != nil || !exactDSCLAttributes(groupAttributes, map[string]string{"PrimaryGroupID": "299"}) {
		return errors.New("existing macOS node service group differs from contract")
	}
	for _, attribute := range []string{"GroupMembership", "GroupMembers"} {
		value, memberErr := e.Commands.Run(ctx, "/usr/bin/dscl", ".", "-read", "/Groups/_blazn-node", attribute)
		if memberErr == nil && strings.TrimSpace(string(value)) != "" {
			return errors.New("existing macOS node service group has supplementary members")
		}
		if memberErr != nil {
			var commandErr *FixedCommandError
			if !errors.As(memberErr, &commandErr) || commandErr.ExitCode != 1 {
				return errors.New("existing macOS node service membership cannot be verified")
			}
		}
	}
	return nil
}

func exactDSCLAttributes(output []byte, expected map[string]string) bool {
	actual := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return false
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if key == "" || actual[key] != "" {
			return false
		}
		actual[key] = value
	}
	if len(actual) != len(expected) {
		return false
	}
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}
func (e NativeRootEngine) serviceState(ctx context.Context, service client.NodeInstallService) (RootResponse, error) {
	state := ServicePriorState{}
	if service.Manager == "systemd" {
		if output, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-enabled", service.UnitName); err == nil {
			switch strings.TrimSpace(string(output)) {
			case "enabled":
				state.Enabled = true
			case "disabled":
			default:
				return RootResponse{}, errors.New("capture systemd enabled state is ambiguous")
			}
		} else if err != nil && !fixedExit(err, 1) {
			return RootResponse{}, errors.New("capture systemd enabled state failed")
		}
		if output, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-active", service.UnitName); err == nil {
			switch strings.TrimSpace(string(output)) {
			case "active":
				state.Active = true
			case "inactive":
			default:
				return RootResponse{}, errors.New("capture systemd active state is ambiguous")
			}
		} else if err != nil && !fixedExit(err, 3) {
			return RootResponse{}, errors.New("capture systemd active state failed")
		}
	} else {
		if _, err := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+service.UnitName); err == nil {
			state.Enabled = true
			state.Active = true
		} else if !fixedExit(err, 113) && !fixedExit(err, 3) {
			return RootResponse{}, errors.New("capture launchd state failed")
		}
	}
	return RootResponse{Service: &state}, nil
}
func fixedExit(err error, code int) bool {
	var commandErr *FixedCommandError
	return errors.As(err, &commandErr) && commandErr.ExitCode == code
}
func (e NativeRootEngine) capture(ctx context.Context, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, backupRoot string, join *RootJoinBinding) (PriorState, error) {
	if !canonicalPath(backupRoot) {
		return PriorState{}, errors.New("backup root is unsafe")
	}
	if mutation.Kind == "label" || mutation.Kind == "taint" {
		state, err := e.readClusterNode(ctx, plan, join)
		if err != nil {
			return PriorState{}, err
		}
		metadata := map[string]string{"kind": mutation.Kind, "target": mutation.Target, "present": "false"}
		if mutation.Kind == "label" {
			if value, ok := state.Labels[mutation.Target]; ok {
				metadata["present"], metadata["value"] = "true", value
			}
		} else {
			for _, taint := range state.Taints {
				if taint.Key == mutation.Target && taint.Effect == stringValue(mutation.Desired["effect"]) {
					metadata["present"], metadata["value"], metadata["effect"] = "true", taint.Value, taint.Effect
					break
				}
			}
		}
		if metadata["present"] == "false" {
			return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
		}
		return e.backupMetadata(backupRoot, plan, mutation, metadata)
	}
	if (mutation.Kind == "systemd_unit" || mutation.Kind == "launchd_unit") && mutation.Action == "enable" {
		response, err := e.serviceState(ctx, plan.NodeService)
		if err != nil || response.Service == nil {
			return PriorState{}, errors.New("service state cannot be captured exactly")
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "service", "manager": plan.NodeService.Manager, "name": plan.NodeService.UnitName, "enabled": strconv.FormatBool(response.Service.Enabled), "active": strconv.FormatBool(response.Service.Active)})
	}
	if mutation.Kind == "group" {
		group, err := user.LookupGroup(mutation.Target)
		if err != nil {
			var unknown user.UnknownGroupError
			if errors.As(err, &unknown) {
				return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
			}
			return PriorState{}, err
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "group", "name": group.Name, "gid": group.Gid})
	}
	if mutation.Kind == "user" {
		account, err := user.Lookup(mutation.Target)
		if err != nil {
			var unknown user.UnknownUserError
			if errors.As(err, &unknown) {
				return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
			}
			return PriorState{}, err
		}
		output, err := e.Commands.Run(ctx, "/usr/bin/getent", "passwd", mutation.Target)
		parts := strings.Split(strings.TrimSpace(string(output)), ":")
		if err != nil || len(parts) != 7 || parts[0] != account.Username || parts[2] != account.Uid || parts[3] != account.Gid || parts[5] != account.HomeDir {
			return PriorState{}, errors.New("preexisting user state cannot be captured exactly")
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "user", "name": account.Username, "uid": account.Uid, "gid": account.Gid, "home": account.HomeDir, "shell": parts[6]})
	}
	if mutation.Kind == "package" {
		manager := stringValue(mutation.Desired["manager"])
		version, err := e.installedPackageVersion(ctx, mutation.Target, manager)
		if err != nil {
			var commandError *FixedCommandError
			if errors.As(err, &commandError) && commandError.ExitCode == 1 {
				return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
			}
			return PriorState{}, err
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "package", "name": mutation.Target, "manager": manager, "version": version})
	}
	if mutation.Kind == "directory" {
		info, err := os.Lstat(mutation.Target)
		if errors.Is(err, os.ErrNotExist) {
			return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return PriorState{}, errors.New("preexisting directory state is unsafe")
		}
		uid, _, ownerOK := fileOwner(info)
		gid, groupOK := fileGroup(info)
		if !ownerOK || !groupOK {
			return PriorState{}, errors.New("preexisting directory ownership is unavailable")
		}
		return e.backupMetadata(backupRoot, plan, mutation, map[string]string{"kind": "directory", "mode": strconv.FormatUint(uint64(info.Mode().Perm()), 8), "uid": strconv.FormatInt(uid, 10), "gid": strconv.FormatInt(gid, 10)})
	}
	if strings.HasPrefix(mutation.Target, "/") {
		info, err := os.Lstat(mutation.Target)
		if errors.Is(err, os.ErrNotExist) {
			return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return PriorState{}, errors.New("mutation target is unsafe")
		}
		content := []byte{}
		if info.Mode().IsRegular() {
			content, err = readBoundedRegular(mutation.Target, 512<<20)
			if err != nil {
				return PriorState{}, err
			}
		}
		return e.backupFile(backupRoot, plan, mutation, info, content)
	}
	return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
}
func (e NativeRootEngine) backupMetadata(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, value any) (PriorState, error) {
	encoded, _ := json.Marshal(value)
	return e.writeBackup(root, plan, mutation, encoded, "metadata_snapshot", 0600, 0, 0)
}
func (e NativeRootEngine) backupFile(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, info os.FileInfo, content []byte) (PriorState, error) {
	owner, _, ok := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ok || !groupOK {
		return PriorState{}, errors.New("target owner is unavailable")
	}
	return e.writeBackup(root, plan, mutation, content, "file_backup", info.Mode().Perm(), owner, group)
}
func (e NativeRootEngine) writeBackup(root string, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, value []byte, kind string, mode os.FileMode, uid, gid int64) (PriorState, error) {
	id := backupID(plan.PlanID, mutation.Ordinal, value)
	if err := ensurePrivateDirectory(root, 0); err != nil {
		return PriorState{}, err
	}
	path := filepath.Join(root, id)
	if err := writeRootAtomic(path, value, 0600, 0, 0); err != nil {
		return PriorState{}, err
	}
	sum := sha256.Sum256(value)
	m, u, g := int64(mode.Perm()), uid, gid
	return PriorState{State: "preexisting_exact", Material: client.NodeRollbackMaterial{Kind: kind, Locator: "receipt-backup://" + id, Digest: "sha256:" + hex.EncodeToString(sum[:]), Mode: &m, UID: &u, GID: &g}}, nil
}

func (e NativeRootEngine) apply(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, material *RootMaterial, join *RootJoinBinding) error {
	switch m.Kind {
	case "group":
		gid := number(m.Desired["gid"])
		if existing, err := user.LookupGroup(m.Target); err == nil {
			if existing.Gid != strconv.FormatInt(gid, 10) {
				return errors.New("existing group differs from exact signed GID")
			}
			return nil
		}
		_, err := e.Commands.Run(ctx, "/usr/sbin/groupadd", "--system", "--gid", strconv.FormatInt(gid, 10), m.Target)
		return err
	case "user":
		if _, err := user.Lookup(m.Target); err == nil {
			return e.verifyUser(ctx, m)
		}
		_, err := e.Commands.Run(ctx, "/usr/sbin/useradd", "--system", "--uid", strconv.FormatInt(number(m.Desired["uid"]), 10), "--gid", stringValue(m.Desired["group"]), "--home-dir", stringValue(m.Desired["home"]), "--shell", stringValue(m.Desired["shell"]), m.Target)
		return err
	case "directory":
		if err := verifyNoSymlinkTraversal(m.Target); err != nil {
			return err
		}
		if m.Action == "adopt_exact" {
			return verifyExactDirectory(m.Target, os.FileMode(m.Mode), m.UID, m.GID)
		}
		if err := os.MkdirAll(m.Target, os.FileMode(m.Mode)); err != nil {
			return err
		}
		if err := os.Chown(m.Target, int(m.UID), int(m.GID)); err != nil {
			return err
		}
		return os.Chmod(m.Target, os.FileMode(m.Mode))
	case "file", "certificate":
		if component := planComponent(plan, material); component != nil && material.ContentBase64 == "" {
			if m.Action != "adopt_exact" || (component.SourceClass != "current_binary" && component.SourceClass != "embedded") {
				return errors.New("digest-only adopted material binding is invalid")
			}
			if component.SourceClass == "current_binary" && component.ArtifactType != "binary" {
				return errors.New("current binary mutation binding is invalid")
			}
			return verifyFileDigestAndMetadata(m.Target, component.SHA256, os.FileMode(m.Mode), m.UID, m.GID)
		}
		content, err := decodeMaterial(material)
		if err != nil {
			return err
		}
		if m.Action == "adopt_exact" {
			return verifyExactFile(m.Target, content, os.FileMode(m.Mode), m.UID, m.GID)
		}
		return writeRootAtomic(m.Target, content, os.FileMode(m.Mode), int(m.UID), int(m.GID))
	case "systemd_unit", "launchd_unit":
		if m.Action == "enable" {
			if m.Kind == "systemd_unit" {
				_, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "enable", "--now", plan.NodeService.UnitName)
				return err
			}
			paths, pathErr := NodeProductionPaths(plan.Target.Platform)
			if pathErr != nil {
				return pathErr
			}
			if identityErr := e.ensureMacOSServiceIdentity(ctx, plan.NodeService, paths.ServiceStateRoot); identityErr != nil {
				return identityErr
			}
			if _, err := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+plan.NodeService.UnitName); err == nil {
				return nil
			}
			_, err := e.Commands.Run(ctx, "/bin/launchctl", "bootstrap", "system", m.Target)
			return err
		}
		content, err := decodeMaterial(material)
		if err != nil {
			return err
		}
		if err := writeRootAtomic(m.Target, content, os.FileMode(m.Mode), int(m.UID), int(m.GID)); err != nil {
			return err
		}
		if m.Kind == "systemd_unit" {
			_, err = e.Commands.Run(ctx, "/usr/bin/systemctl", "daemon-reload")
		}
		return err
	case "package":
		manager := stringValue(m.Desired["manager"])
		if m.Action == "adopt_exact" {
			return e.verifyPackage(ctx, m, manager)
		}
		name, cleanup, err := e.stageHTTPSPackage(ctx, plan, m, material)
		if err != nil {
			return err
		}
		defer cleanup()
		if manager == "snap" {
			_, err = e.Commands.Run(ctx, "/usr/bin/snap", "install", name, "--dangerous")
		} else if manager == "brew" {
			_, err = e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "install", name)
		} else {
			_, err = e.Commands.Run(ctx, "/usr/bin/apt-get", "install", "-y", name)
		}
		return err
	case "image":
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.ctr", "images", "pull", m.Target)
		return err
	case "label":
		return e.applyClusterMutation(ctx, plan, m, join, false)
	case "taint":
		return e.applyClusterMutation(ctx, plan, m, join, false)
	case "firewall":
		_, err := e.Commands.Run(ctx, "/usr/sbin/ufw", "allow", strconv.FormatInt(number(m.Desired["port"]), 10)+"/"+stringValue(m.Desired["protocol"]))
		return err
	default:
		return errors.New("mutation kind is unsupported")
	}
}

func planComponent(plan client.NodeInstallPlan, material *RootMaterial) *client.NodeInstallComponent {
	if material == nil {
		return nil
	}
	for index := range plan.Components {
		if plan.Components[index].Name == material.ComponentName && plan.Components[index].SHA256 == material.SHA256 {
			return &plan.Components[index]
		}
	}
	return nil
}
func (e NativeRootEngine) verifyPackage(ctx context.Context, m client.NodeInstallMutation, manager string) error {
	expected := stringValue(m.Desired["version"])
	installed, err := e.installedPackageVersion(ctx, m.Target, manager)
	if err != nil {
		return err
	}
	if manager == "snap" {
		revision := strings.TrimPrefix(expected, "v1.35.6-rev")
		if revision == expected || installed != "revision:"+revision {
			return errors.New("installed snap revision differs from signed plan")
		}
		return nil
	}
	if installed != expected {
		if manager == "brew" {
			return errors.New("installed brew version differs from signed plan")
		}
		return errors.New("installed package version differs from signed plan")
	}
	return nil
}

func (e NativeRootEngine) installedPackageVersion(ctx context.Context, target, manager string) (string, error) {
	if manager == "snap" {
		output, err := e.Commands.Run(ctx, "/usr/bin/snap", "list", target, "--unicode=never")
		if err != nil {
			return "", err
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		fields := strings.Fields(lines[len(lines)-1])
		if len(fields) < 3 || fields[0] != target {
			return "", errors.New("installed snap response is invalid")
		}
		return "revision:" + fields[2], nil
	}
	if manager == "brew" {
		output, err := e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "list", "--versions", target)
		if err != nil {
			return "", err
		}
		fields := strings.Fields(string(output))
		if len(fields) != 2 || fields[0] != target {
			return "", errors.New("installed brew response is invalid")
		}
		return fields[1], nil
	}
	if manager != "apt" {
		return "", errors.New("package manager is unsupported")
	}
	output, err := e.Commands.Run(ctx, "/usr/bin/dpkg-query", "-W", "-f=${Version}", target)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
func (e NativeRootEngine) verifyUser(ctx context.Context, m client.NodeInstallMutation) error {
	output, err := e.Commands.Run(ctx, "/usr/bin/getent", "passwd", m.Target)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(parts) != 7 || parts[0] != m.Target || parts[2] != strconv.FormatInt(number(m.Desired["uid"]), 10) || parts[3] != strconv.FormatInt(number(m.Desired["gid"]), 10) || parts[5] != stringValue(m.Desired["home"]) || parts[6] != stringValue(m.Desired["shell"]) {
		return errors.New("existing user differs from exact signed account")
	}
	return nil
}
func verifyExactFile(path string, content []byte, mode os.FileMode, uid, gid int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode.Perm() {
		return errors.New("adopted file metadata differs from signed plan")
	}
	owner, _, ok := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ok || !groupOK || owner != uid || group != gid {
		return errors.New("adopted file ownership differs from signed plan")
	}
	value, err := readBoundedRegular(path, 512<<20)
	if err != nil {
		return err
	}
	if !bytes.Equal(value, content) {
		return errors.New("adopted file content differs from signed plan")
	}
	return nil
}
func verifyExactDirectory(path string, mode os.FileMode, uid, gid int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode.Perm() {
		return errors.New("adopted directory metadata differs from signed plan")
	}
	owner, _, ok := fileOwner(info)
	group, groupOK := fileGroup(info)
	if !ok || !groupOK || owner != uid || group != gid {
		return errors.New("adopted directory ownership differs from signed plan")
	}
	return nil
}
func (e NativeRootEngine) rollback(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, prior PriorState, backupRoot string, join *RootJoinBinding) error {
	if err := e.verifyRollbackDesired(ctx, plan, m, join); err != nil {
		if matches, priorErr := e.matchesCapturedPrior(ctx, plan, m, prior, backupRoot, join); priorErr == nil && matches {
			return nil
		}
		return fmt.Errorf("rollback compare-and-swap rejected drift: %w", err)
	}
	if prior.State == "absent" {
		switch m.Kind {
		case "group":
			_, err := e.Commands.Run(ctx, "/usr/sbin/groupdel", m.Target)
			return err
		case "user":
			_, err := e.Commands.Run(ctx, "/usr/sbin/userdel", m.Target)
			return err
		case "systemd_unit":
			if err := os.Remove(m.Target); err != nil {
				return err
			}
			_, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "daemon-reload")
			return err
		case "launchd_unit":
			return os.Remove(m.Target)
		case "package":
			manager := stringValue(m.Desired["manager"])
			if manager == "snap" {
				_, err := e.Commands.Run(ctx, "/usr/bin/snap", "remove", m.Target)
				return err
			}
			if manager == "brew" {
				_, err := e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "uninstall", m.Target)
				return err
			}
			_, err := e.Commands.Run(ctx, "/usr/bin/apt-get", "remove", "-y", m.Target)
			return err
		case "label":
			return e.applyClusterMutation(ctx, plan, m, join, true)
		case "taint":
			return e.applyClusterMutation(ctx, plan, m, join, true)
		case "image":
			_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.ctr", "images", "remove", m.Target)
			return err
		case "firewall":
			_, err := e.Commands.Run(ctx, "/usr/sbin/ufw", "delete", "allow", strconv.FormatInt(number(m.Desired["port"]), 10)+"/"+stringValue(m.Desired["protocol"]))
			return err
		default:
			if strings.HasPrefix(m.Target, "/") {
				return os.Remove(m.Target)
			}
		}
		return nil
	}
	if prior.Material.Kind == "file_backup" {
		if prior.Material.Mode == nil || prior.Material.UID == nil || prior.Material.GID == nil {
			return errors.New("file rollback metadata is incomplete")
		}
		path, err := client.ResolveNodeRollbackLocator(backupRoot, prior.Material.Locator)
		if err != nil {
			return err
		}
		content, err := readBoundedRegular(path, 512<<20)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		if "sha256:"+hex.EncodeToString(sum[:]) != prior.Material.Digest {
			return errors.New("rollback backup digest mismatch")
		}
		if err := writeRootAtomic(m.Target, content, os.FileMode(*prior.Material.Mode), int(*prior.Material.UID), int(*prior.Material.GID)); err != nil {
			return err
		}
		if m.Kind == "systemd_unit" {
			_, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "daemon-reload")
			return err
		}
		return nil
	}
	if prior.Material.Kind == "metadata_snapshot" {
		path, err := client.ResolveNodeRollbackLocator(backupRoot, prior.Material.Locator)
		if err != nil {
			return err
		}
		content, err := readBoundedRegular(path, 1<<20)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		if "sha256:"+hex.EncodeToString(sum[:]) != prior.Material.Digest {
			return errors.New("rollback metadata digest mismatch")
		}
		var metadata map[string]string
		if json.Unmarshal(content, &metadata) != nil {
			return errors.New("rollback metadata is invalid")
		}
		switch metadata["kind"] {
		case "group":
			_, err = e.Commands.Run(ctx, "/usr/sbin/groupmod", "--gid", metadata["gid"], m.Target)
			return err
		case "user":
			args := []string{"--uid", metadata["uid"], "--gid", metadata["gid"], "--home", metadata["home"], "--shell", metadata["shell"], m.Target}
			_, err = e.Commands.Run(ctx, "/usr/sbin/usermod", args...)
			return err
		case "package":
			manager := metadata["manager"]
			if manager == "snap" {
				_, err = e.Commands.Run(ctx, "/usr/bin/snap", "refresh", m.Target, "--revision", strings.TrimPrefix(metadata["version"], "revision:"))
				return err
			}
			if manager == "brew" {
				_, err = e.Commands.Run(ctx, "/opt/homebrew/bin/brew", "install", m.Target+"@"+metadata["version"])
				return err
			}
			_, err = e.Commands.Run(ctx, "/usr/bin/apt-get", "install", "-y", m.Target+"="+metadata["version"])
			return err
		case "service":
			return e.restoreServicePrior(ctx, plan, m, metadata)
		case "directory":
			return restoreDirectoryMetadata(m.Target, metadata)
		case "label":
			if m.Kind != "label" || metadata["target"] != m.Target || metadata["present"] != "true" {
				return errors.New("rollback label metadata is invalid")
			}
			restore := m
			restore.Desired = map[string]any{"value": metadata["value"]}
			return e.applyClusterMutation(ctx, plan, restore, join, false)
		case "taint":
			if m.Kind != "taint" || metadata["target"] != m.Target || metadata["present"] != "true" || metadata["effect"] == "" {
				return errors.New("rollback taint metadata is invalid")
			}
			restore := m
			restore.Desired = map[string]any{"value": metadata["value"], "effect": metadata["effect"]}
			return e.applyClusterMutation(ctx, plan, restore, join, false)
		default:
			return errors.New("rollback metadata kind is unsupported")
		}
	}
	return errors.New("rollback material kind is unsupported")
}

func (e NativeRootEngine) matchesCapturedPrior(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, prior PriorState, backupRoot string, join *RootJoinBinding) (bool, error) {
	if prior.State == "absent" {
		switch m.Kind {
		case "label", "taint":
			state, err := e.readClusterNode(ctx, plan, join)
			if err != nil {
				return false, err
			}
			if m.Kind == "label" {
				_, ok := state.Labels[m.Target]
				return !ok, nil
			}
			for _, taint := range state.Taints {
				if taint.Key == m.Target && taint.Effect == stringValue(m.Desired["effect"]) {
					return false, nil
				}
			}
			return true, nil
		case "group":
			_, err := user.LookupGroup(m.Target)
			var unknown user.UnknownGroupError
			return errors.As(err, &unknown), nil
		case "user":
			_, err := user.Lookup(m.Target)
			var unknown user.UnknownUserError
			return errors.As(err, &unknown), nil
		case "package":
			_, err := e.installedPackageVersion(ctx, m.Target, stringValue(m.Desired["manager"]))
			return fixedExit(err, 1), nil
		default:
			_, err := os.Lstat(m.Target)
			return errors.Is(err, os.ErrNotExist), nil
		}
	}
	path, err := client.ResolveNodeRollbackLocator(backupRoot, prior.Material.Locator)
	if err != nil {
		return false, err
	}
	content, err := readBoundedRegular(path, 512<<20)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(sum[:]) != prior.Material.Digest {
		return false, errors.New("captured prior digest mismatch")
	}
	if prior.Material.Kind == "file_backup" {
		if prior.Material.Mode == nil || prior.Material.UID == nil || prior.Material.GID == nil {
			return false, errors.New("captured file metadata is incomplete")
		}
		info, statErr := os.Lstat(m.Target)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != os.FileMode(*prior.Material.Mode).Perm() {
			return false, nil
		}
		owner, _, ownerOK := fileOwner(info)
		group, groupOK := fileGroup(info)
		if !ownerOK || !groupOK || owner != *prior.Material.UID || group != *prior.Material.GID {
			return false, nil
		}
		current, readErr := readBoundedRegular(m.Target, 512<<20)
		return readErr == nil && bytes.Equal(current, content), readErr
	}
	if prior.Material.Kind != "metadata_snapshot" {
		return false, nil
	}
	var metadata map[string]string
	if decodeSingleJSON(content, &metadata) != nil {
		return false, errors.New("captured prior metadata is invalid")
	}
	switch metadata["kind"] {
	case "service":
		response, err := e.serviceState(ctx, plan.NodeService)
		if err != nil || response.Service == nil {
			return false, err
		}
		enabled, _ := strconv.ParseBool(metadata["enabled"])
		active, _ := strconv.ParseBool(metadata["active"])
		return response.Service.Enabled == enabled && response.Service.Active == active, nil
	case "directory":
		info, err := os.Lstat(m.Target)
		if err != nil {
			return false, nil
		}
		uid, _, ownerOK := fileOwner(info)
		gid, groupOK := fileGroup(info)
		mode, _ := strconv.ParseUint(metadata["mode"], 8, 32)
		return info.IsDir() && ownerOK && groupOK && uid == mustParseInt(metadata["uid"]) && gid == mustParseInt(metadata["gid"]) && info.Mode().Perm() == os.FileMode(mode), nil
	case "label", "taint":
		state, err := e.readClusterNode(ctx, plan, join)
		if err != nil {
			return false, err
		}
		if metadata["kind"] == "label" {
			value, ok := state.Labels[m.Target]
			return ok && value == metadata["value"], nil
		}
		for _, taint := range state.Taints {
			if taint.Key == m.Target && taint.Value == metadata["value"] && taint.Effect == metadata["effect"] {
				return true, nil
			}
		}
		return false, nil
	case "group":
		group, err := user.LookupGroup(m.Target)
		return err == nil && group.Gid == metadata["gid"], nil
	case "user":
		account, err := user.Lookup(m.Target)
		return err == nil && account.Uid == metadata["uid"] && account.Gid == metadata["gid"] && account.HomeDir == metadata["home"], nil
	case "package":
		version, err := e.installedPackageVersion(ctx, m.Target, metadata["manager"])
		return err == nil && version == metadata["version"], nil
	}
	return false, nil
}
func mustParseInt(value string) int64 { parsed, _ := strconv.ParseInt(value, 10, 64); return parsed }

func (e NativeRootEngine) restoreServicePrior(ctx context.Context, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, metadata map[string]string) error {
	enabled, enabledErr := strconv.ParseBool(metadata["enabled"])
	active, activeErr := strconv.ParseBool(metadata["active"])
	if enabledErr != nil || activeErr != nil || metadata["name"] != plan.NodeService.UnitName || metadata["manager"] != plan.NodeService.Manager {
		return errors.New("rollback service metadata is invalid")
	}
	if metadata["manager"] == "systemd" {
		if _, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "daemon-reload"); err != nil {
			return err
		}
		enableAction := "disable"
		if enabled {
			enableAction = "enable"
		}
		if _, err := e.Commands.Run(ctx, "/usr/bin/systemctl", enableAction, metadata["name"]); err != nil {
			return err
		}
		activeAction := "stop"
		if active {
			activeAction = "start"
		}
		_, err := e.Commands.Run(ctx, "/usr/bin/systemctl", activeAction, metadata["name"])
		return err
	}
	_, currentErr := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+metadata["name"])
	if active || enabled {
		if currentErr == nil {
			return nil
		}
		_, err := e.Commands.Run(ctx, "/bin/launchctl", "bootstrap", "system", mutation.Target)
		return err
	}
	if currentErr != nil {
		return nil
	}
	_, err := e.Commands.Run(ctx, "/bin/launchctl", "bootout", "system/"+metadata["name"])
	return err
}

func (e NativeRootEngine) verifyRollbackDesired(ctx context.Context, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, join *RootJoinBinding) error {
	if mutation.Kind == "label" || mutation.Kind == "taint" {
		if join == nil || join.ExpectedNodeUID == "" {
			return errors.New("cluster rollback binding is unavailable")
		}
		output, err := e.kubectl(ctx, plan, "get", "node", join.ExpectedNodeName, "-o", "json")
		if err != nil {
			return err
		}
		var node struct {
			Metadata struct {
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Taints []clusterTaint `json:"taints"`
			} `json:"spec"`
		}
		if json.Unmarshal(output, &node) != nil || node.Metadata.UID != join.ExpectedNodeUID {
			return errors.New("cluster rollback node differs from binding")
		}
		return e.verifyMutation(ctx, plan, mutation, node.Metadata.Labels, node.Spec.Taints)
	}
	return e.verifyMutation(ctx, plan, mutation, nil, nil)
}

func restoreDirectoryMetadata(target string, metadata map[string]string) error {
	mode, modeErr := strconv.ParseUint(metadata["mode"], 8, 32)
	uid, uidErr := strconv.ParseInt(metadata["uid"], 10, 32)
	gid, gidErr := strconv.ParseInt(metadata["gid"], 10, 32)
	if metadata["kind"] != "directory" || modeErr != nil || uidErr != nil || gidErr != nil || !canonicalPath(target) {
		return errors.New("rollback directory metadata is invalid")
	}
	if err := verifyNoSymlinkTraversal(target); err != nil {
		return err
	}
	if err := os.Chown(target, int(uid), int(gid)); err != nil {
		return err
	}
	return os.Chmod(target, os.FileMode(mode))
}

func (e NativeRootEngine) join(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) (JoinedNode, error) {
	if binding == nil || !binding.WorkerOnly || binding.ClusterID != plan.Cluster.ID {
		return JoinedNode{}, errors.New("join binding is invalid")
	}
	payload, urls, err := decodeJoinCredential(binding.Credential, binding)
	if err != nil {
		return JoinedNode{}, err
	}
	_ = payload
	if len(urls) == 0 {
		return JoinedNode{}, errors.New("join credential has no endpoint")
	}
	if err := e.verifyJoinRuntime(ctx, plan); err != nil {
		return JoinedNode{}, err
	}
	input := []byte(urls[0] + "\n")
	if e.Platform == "linux" {
		if _, err := e.Commands.RunInput(ctx, "/snap/microk8s/current/usr/bin/python3", input, "-c", microK8sJoinStdinProgram); err != nil {
			return JoinedNode{}, err
		}
	} else {
		vm, err := readLimaVM(plan, e.LimaBindingPath)
		if err != nil {
			return JoinedNode{}, err
		}
		if _, err := e.Commands.RunInput(ctx, "/usr/local/bin/limactl", input, "shell", vm, "sudo", "/usr/bin/env", "SNAP=/snap/microk8s/current", "SNAP_DATA=/var/snap/microk8s/current", "SNAP_COMMON=/var/snap/microk8s/common", "/snap/microk8s/current/usr/bin/python3", "-c", microK8sJoinStdinProgram); err != nil {
			return JoinedNode{}, err
		}
	}
	return e.observeNode(ctx, plan, binding.ExpectedNodeName)
}

const microK8sJoinStdinProgram = `import importlib.util,sys
p="/snap/microk8s/current/scripts/wrappers/join.py"
s=importlib.util.spec_from_file_location("blazn_microk8s_join",p)
m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
c=sys.stdin.readline().strip()
if not c: raise SystemExit(2)
m.join.callback(c,"as-worker",False,False)
`
const pinnedJoinSHA256 = "cd050fe6926af0ae07a7505834eb92a94c1f370f89e7cdcedb79e95ed63ad419"

func (e NativeRootEngine) verifyJoinRuntime(ctx context.Context, plan client.NodeInstallPlan) error {
	if e.allowTestJoinRuntime {
		return nil
	}
	if e.Platform == "linux" {
		value, err := os.ReadFile("/snap/microk8s/current/scripts/wrappers/join.py")
		if err != nil {
			return err
		}
		sum := sha256.Sum256(value)
		if hex.EncodeToString(sum[:]) != pinnedJoinSHA256 {
			return errors.New("pinned MicroK8s join helper digest mismatch")
		}
		return nil
	}
	vm, err := readLimaVM(plan, e.LimaBindingPath)
	if err != nil {
		return err
	}
	output, err := e.Commands.Run(ctx, "/usr/local/bin/limactl", "shell", vm, "sha256sum", "/snap/microk8s/current/scripts/wrappers/join.py")
	if err != nil || !strings.HasPrefix(string(output), pinnedJoinSHA256+" ") {
		return errors.New("Lima MicroK8s join helper digest mismatch")
	}
	return nil
}
func (e NativeRootEngine) kubectl(ctx context.Context, plan client.NodeInstallPlan, args ...string) ([]byte, error) {
	if e.Platform == "linux" {
		return e.Commands.Run(ctx, "/snap/bin/microk8s.kubectl", args...)
	}
	vm, err := readLimaVM(plan, e.LimaBindingPath)
	if err != nil {
		return nil, err
	}
	fixed := append([]string{"shell", vm, "sudo", "/snap/bin/microk8s.kubectl"}, args...)
	return e.Commands.Run(ctx, "/usr/local/bin/limactl", fixed...)
}
func (e NativeRootEngine) applyClusterMutation(ctx context.Context, plan client.NodeInstallPlan, m client.NodeInstallMutation, join *RootJoinBinding, remove bool) error {
	state, err := e.readClusterNode(ctx, plan, join)
	if err != nil {
		return err
	}
	operations := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": state.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": state.ResourceVersion}}
	if m.Kind == "label" {
		path := "/metadata/labels/" + strings.ReplaceAll(strings.ReplaceAll(m.Target, "~", "~0"), "/", "~1")
		prior, exists := state.Labels[m.Target]
		if remove {
			if !exists {
				return nil
			}
			operations = append(operations, map[string]any{"op": "test", "path": path, "value": prior}, map[string]any{"op": "remove", "path": path})
		} else {
			if !state.LabelsPresent {
				operations = append(operations, map[string]any{"op": "add", "path": "/metadata/labels", "value": map[string]string{m.Target: stringValue(m.Desired["value"])}})
			} else {
				op := "add"
				if exists {
					op = "replace"
					operations = append(operations, map[string]any{"op": "test", "path": path, "value": prior})
				}
				operations = append(operations, map[string]any{"op": op, "path": path, "value": stringValue(m.Desired["value"])})
			}
		}
	} else {
		updated := append([]clusterTaint(nil), state.Taints...)
		found := -1
		for index, taint := range updated {
			if taint.Key == m.Target && taint.Effect == stringValue(m.Desired["effect"]) {
				found = index
				break
			}
		}
		if remove {
			if found < 0 {
				return nil
			}
			updated = append(updated[:found], updated[found+1:]...)
		} else {
			desired := clusterTaint{Key: m.Target, Value: stringValue(m.Desired["value"]), Effect: stringValue(m.Desired["effect"])}
			if found < 0 {
				updated = append(updated, desired)
			} else {
				updated[found] = desired
			}
		}
		if state.TaintsPresent {
			operations = append(operations, map[string]any{"op": "test", "path": "/spec/taints", "value": state.Taints}, map[string]any{"op": "replace", "path": "/spec/taints", "value": updated})
		} else {
			operations = append(operations, map[string]any{"op": "add", "path": "/spec/taints", "value": updated})
		}
	}
	encoded, err := json.Marshal(operations)
	if err != nil {
		return err
	}
	output, err := e.kubectl(ctx, plan, "patch", "node", join.ExpectedNodeName, "--type=json", "--patch", string(encoded), "-o", "json")
	if err != nil {
		return errors.New("atomic Kubernetes node mutation failed")
	}
	var result struct {
		Metadata struct{ Name, UID, ResourceVersion string } `json:"metadata"`
	}
	if decodeSingleJSON(output, &result) != nil || result.Metadata.Name != join.ExpectedNodeName || result.Metadata.UID != join.ExpectedNodeUID || result.Metadata.ResourceVersion == "" {
		return errors.New("atomic Kubernetes mutation response is invalid")
	}
	join.ExpectedResourceVersion = result.Metadata.ResourceVersion
	return nil
}

const capacityEligibilityLabel = "blazn.dev/sandbox-eligible"

type capacityNodeState struct {
	Name, UID, ResourceVersion   string
	Labels                       map[string]string
	Taints                       []clusterTaint
	LabelsPresent, TaintsPresent bool
	Unschedulable                *bool
}

func (e NativeRootEngine) releaseNodeCapacity(ctx context.Context, plan client.NodeInstallPlan, join *RootJoinBinding, receipt *client.NodeInstallReceipt) (*client.KubernetesBinding, error) {
	if join == nil || receipt == nil || join.ClusterID != plan.Cluster.ID || join.ExpectedNodeName != plan.Hostname || join.ExpectedNodeUID == "" || join.ExpectedResourceVersion == "" {
		return nil, errors.New("capacity release requires the exact installed node binding")
	}
	_, _, authorityPath, err := e.authorityPaths()
	if err != nil {
		return nil, err
	}
	authority, err := loadRootAuthority(authorityPath)
	if err != nil || authority.KubernetesBinding == nil || verifyAuthorityReceipt(authority, *receipt, "active") != nil {
		return nil, errors.New("capacity release lacks a trusted active install receipt")
	}
	authorized := authority.KubernetesBinding
	if authorized.ClusterID != join.ClusterID || authorized.NodeName != join.ExpectedNodeName || authorized.NodeUID != join.ExpectedNodeUID {
		return nil, errors.New("capacity release binding differs from root authority")
	}
	state, err := e.readCapacityNode(ctx, plan, authorized.NodeName)
	if err != nil {
		return nil, err
	}
	if state.Name != authorized.NodeName || state.UID != authorized.NodeUID {
		return nil, errors.New("capacity release node UID differs from root authority")
	}
	released, stateErr := validateCapacityState(state)
	if stateErr != nil {
		return nil, stateErr
	}
	if join.ExpectedResourceVersion != authorized.ResourceVersion && !released {
		return nil, errors.New("capacity release request resourceVersion differs from root authority")
	}
	if state.ResourceVersion != authorized.ResourceVersion {
		if !released {
			return nil, errors.New("capacity release resourceVersion differs from its precondition")
		}
		return e.updateRootKubernetesBinding(plan, JoinedNode{Name: state.Name, UID: state.UID, ResourceVersion: state.ResourceVersion})
	}
	if released {
		return e.updateRootKubernetesBinding(plan, JoinedNode{Name: state.Name, UID: state.UID, ResourceVersion: state.ResourceVersion})
	}

	updatedTaints := make([]clusterTaint, 0, len(state.Taints)-1)
	for _, taint := range state.Taints {
		if taint.Key != "blazn.dev/bootstrap" {
			updatedTaints = append(updatedTaints, taint)
		}
	}
	operations := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": state.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": state.ResourceVersion},
		{"op": "test", "path": "/spec/taints", "value": state.Taints},
		{"op": "replace", "path": "/spec/taints", "value": updatedTaints},
	}
	labelPath := "/metadata/labels/" + strings.ReplaceAll(strings.ReplaceAll(capacityEligibilityLabel, "~", "~0"), "/", "~1")
	if state.LabelsPresent {
		operations = append(operations, map[string]any{"op": "add", "path": labelPath, "value": "true"})
	} else {
		operations = append(operations, map[string]any{"op": "add", "path": "/metadata/labels", "value": map[string]string{capacityEligibilityLabel: "true"}})
	}
	if state.Unschedulable != nil && *state.Unschedulable {
		operations = append(operations,
			map[string]any{"op": "test", "path": "/spec/unschedulable", "value": true},
			map[string]any{"op": "replace", "path": "/spec/unschedulable", "value": false},
		)
	}
	encoded, err := json.Marshal(operations)
	if err != nil {
		return nil, err
	}
	output, err := e.kubectl(ctx, plan, "patch", "node", authorized.NodeName, "--type=json", "--patch", string(encoded), "-o", "json")
	if err != nil {
		return nil, errors.New("atomic Kubernetes capacity release failed")
	}
	patched, err := decodeCapacityNode(output)
	if err != nil || patched.Name != authorized.NodeName || patched.UID != authorized.NodeUID {
		return nil, errors.New("capacity release response differs from root authority")
	}
	if released, verifyErr := validateCapacityState(patched); verifyErr != nil || !released {
		return nil, errors.New("capacity release response is not schedulable and eligible")
	}
	readBack, err := e.readCapacityNode(ctx, plan, authorized.NodeName)
	if err != nil || readBack.Name != authorized.NodeName || readBack.UID != authorized.NodeUID {
		return nil, errors.New("capacity release read-back differs from root authority")
	}
	if released, verifyErr := validateCapacityState(readBack); verifyErr != nil || !released {
		return nil, errors.New("capacity release read-back is not schedulable and eligible")
	}
	return e.updateRootKubernetesBinding(plan, JoinedNode{Name: readBack.Name, UID: readBack.UID, ResourceVersion: readBack.ResourceVersion})
}

func validateCapacityState(state capacityNodeState) (bool, error) {
	bootstrapCount := 0
	for _, taint := range state.Taints {
		if taint.Key != "blazn.dev/bootstrap" {
			continue
		}
		if taint.Value != "pending" || taint.Effect != "NoSchedule" {
			return false, errors.New("capacity release found an unexpected bootstrap taint variant")
		}
		bootstrapCount++
	}
	if bootstrapCount > 1 {
		return false, errors.New("capacity release found duplicate bootstrap taints")
	}
	eligibility, hasEligibility := state.Labels[capacityEligibilityLabel]
	if hasEligibility && eligibility != "true" {
		return false, errors.New("capacity release found a conflicting eligibility label")
	}
	unschedulable := state.Unschedulable != nil && *state.Unschedulable
	released := bootstrapCount == 0 && hasEligibility && !unschedulable
	if !released && (bootstrapCount != 1 || hasEligibility) {
		return false, errors.New("capacity release found partially released node state")
	}
	return released, nil
}

func (e NativeRootEngine) readCapacityNode(ctx context.Context, plan client.NodeInstallPlan, name string) (capacityNodeState, error) {
	output, err := e.kubectl(ctx, plan, "get", "node", name, "-o", "json")
	if err != nil {
		return capacityNodeState{}, err
	}
	return decodeCapacityNode(output)
}

func decodeCapacityNode(output []byte) (capacityNodeState, error) {
	var value struct {
		Metadata struct {
			Name, UID, ResourceVersion string
			Labels                     map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Taints        *[]clusterTaint `json:"taints"`
			Unschedulable *bool           `json:"unschedulable"`
		} `json:"spec"`
	}
	if decodeSingleJSON(output, &value) != nil || value.Metadata.Name == "" || value.Metadata.UID == "" || value.Metadata.ResourceVersion == "" {
		return capacityNodeState{}, errors.New("capacity release node response is invalid")
	}
	state := capacityNodeState{Name: value.Metadata.Name, UID: value.Metadata.UID, ResourceVersion: value.Metadata.ResourceVersion, Labels: value.Metadata.Labels, LabelsPresent: value.Metadata.Labels != nil, TaintsPresent: value.Spec.Taints != nil, Unschedulable: value.Spec.Unschedulable}
	if state.Labels == nil {
		state.Labels = map[string]string{}
	}
	if value.Spec.Taints != nil {
		state.Taints = *value.Spec.Taints
	}
	if state.Taints == nil {
		state.Taints = []clusterTaint{}
	}
	return state, nil
}

type clusterTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}
type clusterNodeState struct {
	Name, UID, ResourceVersion   string
	Labels                       map[string]string
	Taints                       []clusterTaint
	LabelsPresent, TaintsPresent bool
}

func (e NativeRootEngine) readClusterNode(ctx context.Context, plan client.NodeInstallPlan, join *RootJoinBinding) (clusterNodeState, error) {
	if join == nil || join.ExpectedNodeUID == "" || join.ExpectedResourceVersion == "" {
		return clusterNodeState{}, errors.New("cluster mutation requires an exact joined node binding")
	}
	output, err := e.kubectl(ctx, plan, "get", "node", join.ExpectedNodeName, "-o", "json")
	if err != nil {
		return clusterNodeState{}, err
	}
	var value struct {
		Metadata struct {
			Name, UID, ResourceVersion string
			Labels                     map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Taints *[]clusterTaint `json:"taints"`
		} `json:"spec"`
	}
	if decodeSingleJSON(output, &value) != nil || value.Metadata.Name != join.ExpectedNodeName || value.Metadata.UID != join.ExpectedNodeUID {
		return clusterNodeState{}, errors.New("cluster mutation node UID differs from binding")
	}
	if value.Metadata.ResourceVersion != join.ExpectedResourceVersion {
		return clusterNodeState{}, errors.New("cluster mutation resourceVersion differs from its precondition")
	}
	labelsPresent := value.Metadata.Labels != nil
	if value.Metadata.Labels == nil {
		value.Metadata.Labels = map[string]string{}
	}
	taintsPresent := value.Spec.Taints != nil
	taints := []clusterTaint{}
	if taintsPresent {
		taints = *value.Spec.Taints
		if taints == nil {
			taints = []clusterTaint{}
		}
	}
	return clusterNodeState{Name: value.Metadata.Name, UID: value.Metadata.UID, ResourceVersion: value.Metadata.ResourceVersion, Labels: value.Metadata.Labels, Taints: taints, LabelsPresent: labelsPresent, TaintsPresent: taintsPresent}, nil
}
func (e NativeRootEngine) observeNode(ctx context.Context, plan client.NodeInstallPlan, name string) (JoinedNode, error) {
	output, err := e.kubectl(ctx, plan, "get", "node", name, "-o", "json")
	if err != nil {
		return JoinedNode{}, err
	}
	var value struct {
		Metadata struct {
			Name            string `json:"name"`
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if json.Unmarshal(output, &value) != nil || value.Metadata.UID == "" || value.Metadata.Name != name || value.Metadata.ResourceVersion == "" {
		return JoinedNode{}, errors.New("joined node binding is unavailable")
	}
	return JoinedNode{Name: value.Metadata.Name, UID: value.Metadata.UID, ResourceVersion: value.Metadata.ResourceVersion}, nil
}
func (e NativeRootEngine) verify(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) error {
	if binding == nil || binding.ExpectedNodeUID == "" {
		return errors.New("verification binding is incomplete")
	}
	joined, err := e.observeNode(ctx, plan, binding.ExpectedNodeName)
	if err != nil || joined.UID != binding.ExpectedNodeUID || joined.ResourceVersion != binding.ExpectedResourceVersion {
		return errors.New("joined node identity or resourceVersion differs from binding")
	}
	output, err := e.kubectl(ctx, plan, "get", "node", binding.ExpectedNodeName, "-o", "json")
	if err != nil {
		return err
	}
	node, err := decodeCapacityNode(output)
	if err != nil || node.Name != binding.ExpectedNodeName || node.UID != binding.ExpectedNodeUID || node.ResourceVersion != binding.ExpectedResourceVersion {
		return errors.New("joined node taint response is invalid")
	}
	released, err := validateCapacityState(node)
	if err != nil {
		return err
	}
	for _, mutation := range sortedMutations(plan) {
		if released && isBootstrapTaintMutation(mutation) {
			continue
		}
		if err := e.verifyMutation(ctx, plan, mutation, node.Labels, node.Taints); err != nil {
			return fmt.Errorf("verify mutation %d: %w", mutation.Ordinal, err)
		}
	}
	return nil
}

func isBootstrapTaintMutation(mutation client.NodeInstallMutation) bool {
	return mutation.Kind == "taint" && mutation.Target == "blazn.dev/bootstrap" && stringValue(mutation.Desired["value"]) == "pending" && stringValue(mutation.Desired["effect"]) == "NoSchedule"
}

func (e NativeRootEngine) verifyQuarantinedJoin(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) error {
	state, err := e.readClusterNode(ctx, plan, binding)
	if err != nil {
		return err
	}
	parts := strings.Split(plan.Cluster.BootstrapTaint, ":")
	if len(parts) != 2 {
		return errors.New("bootstrap quarantine taint is invalid")
	}
	keyValue := strings.SplitN(parts[0], "=", 2)
	if len(keyValue) != 2 {
		return errors.New("bootstrap quarantine taint is invalid")
	}
	for _, taint := range state.Taints {
		if taint.Key == keyValue[0] && taint.Value == keyValue[1] && taint.Effect == parts[1] {
			return nil
		}
	}
	return errors.New("joined worker is not safely quarantined")
}
func (e NativeRootEngine) quarantineJoinedNode(ctx context.Context, plan client.NodeInstallPlan, binding *RootJoinBinding) error {
	parts := strings.Split(plan.Cluster.BootstrapTaint, ":")
	if len(parts) != 2 {
		return errors.New("bootstrap quarantine taint is invalid")
	}
	keyValue := strings.SplitN(parts[0], "=", 2)
	if len(keyValue) != 2 {
		return errors.New("bootstrap quarantine taint is invalid")
	}
	var mutation *client.NodeInstallMutation
	for index := range plan.Mutations {
		candidate := &plan.Mutations[index]
		if candidate.Kind == "taint" && candidate.Target == keyValue[0] && stringValue(candidate.Desired["value"]) == keyValue[1] && stringValue(candidate.Desired["effect"]) == parts[1] {
			mutation = candidate
			break
		}
	}
	if mutation == nil {
		return errors.New("signed plan lacks exact bootstrap quarantine mutation")
	}
	if err := e.applyClusterMutation(ctx, plan, *mutation, binding, false); err != nil {
		return err
	}
	return e.verifyQuarantinedJoin(ctx, plan, binding)
}

func (e NativeRootEngine) verifyMutation(ctx context.Context, plan client.NodeInstallPlan, mutation client.NodeInstallMutation, labels map[string]string, taints []clusterTaint) error {
	switch mutation.Kind {
	case "group":
		group, err := user.LookupGroup(mutation.Target)
		if err != nil || group.Gid != strconv.FormatInt(number(mutation.Desired["gid"]), 10) {
			return errors.New("group differs from signed state")
		}
		output, err := e.Commands.Run(ctx, "/usr/bin/getent", "group", mutation.Target)
		parts := strings.Split(strings.TrimSpace(string(output)), ":")
		if err != nil || len(parts) != 4 || parts[0] != mutation.Target || parts[2] != group.Gid || parts[3] != "" {
			return errors.New("group membership differs from signed state")
		}
		return nil
	case "user":
		return e.verifyUser(ctx, mutation)
	case "directory":
		return verifyExactDirectory(mutation.Target, os.FileMode(mutation.Mode), mutation.UID, mutation.GID)
	case "file", "certificate":
		return verifyFileDigestAndMetadata(mutation.Target, stringValue(mutation.Desired["contentSha256"]), os.FileMode(mutation.Mode), mutation.UID, mutation.GID)
	case "systemd_unit", "launchd_unit":
		if mutation.Action != "enable" {
			return verifyFileDigestAndMetadata(mutation.Target, componentDigest(plan, stringValue(mutation.Desired["sourceComponent"])), os.FileMode(mutation.Mode), mutation.UID, mutation.GID)
		}
		if mutation.Kind == "systemd_unit" {
			enabled, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-enabled", plan.NodeService.UnitName)
			if err != nil || strings.TrimSpace(string(enabled)) != "enabled" {
				return errors.New("systemd service is not enabled")
			}
			active, err := e.Commands.Run(ctx, "/usr/bin/systemctl", "is-active", plan.NodeService.UnitName)
			if err != nil || strings.TrimSpace(string(active)) != "active" {
				return errors.New("systemd service is not active")
			}
			return nil
		}
		_, err := e.Commands.Run(ctx, "/bin/launchctl", "print", "system/"+plan.NodeService.UnitName)
		return err
	case "package":
		return e.verifyPackage(ctx, mutation, stringValue(mutation.Desired["manager"]))
	case "image":
		_, err := e.Commands.Run(ctx, "/snap/bin/microk8s.ctr", "images", "inspect", mutation.Target)
		return err
	case "firewall":
		output, err := e.Commands.Run(ctx, "/usr/sbin/ufw", "status")
		rule := strconv.FormatInt(number(mutation.Desired["port"]), 10) + "/" + stringValue(mutation.Desired["protocol"])
		if err != nil || !strings.Contains(string(output), rule) {
			return errors.New("firewall rule differs from signed state")
		}
		return nil
	case "label":
		if labels[mutation.Target] != stringValue(mutation.Desired["value"]) {
			return errors.New("node label differs from signed state")
		}
		return nil
	case "taint":
		for _, taint := range taints {
			if taint.Key == mutation.Target && taint.Value == stringValue(mutation.Desired["value"]) && taint.Effect == stringValue(mutation.Desired["effect"]) {
				return nil
			}
		}
		return errors.New("node taint differs from signed state")
	default:
		return errors.New("mutation verification is unsupported")
	}
}

func componentDigest(plan client.NodeInstallPlan, name string) string {
	for _, component := range plan.Components {
		if component.Name == name {
			return component.SHA256
		}
	}
	return ""
}

func verifyFileDigestAndMetadata(path, expectedDigest string, mode os.FileMode, uid, gid int64) error {
	if expectedDigest == "" {
		return errors.New("signed file digest is unavailable")
	}
	value, err := readBoundedRegular(path, 512<<20)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != expectedDigest {
		return errors.New("file digest differs from signed state")
	}
	return verifyExactFile(path, value, mode, uid, gid)
}

type joinPayload struct {
	SchemaVersion    string    `json:"schemaVersion"`
	IssuanceID       string    `json:"issuanceId"`
	ClusterID        string    `json:"clusterId"`
	ExpectedNodeName string    `json:"expectedNodeName"`
	BootstrapTaint   string    `json:"bootstrapTaint"`
	WorkerOnly       bool      `json:"workerOnly"`
	ExpiresAt        time.Time `json:"expiresAt"`
	URLs             []string  `json:"urls"`
}

func decodeJoinCredential(encoded string, binding *RootJoinBinding) (joinPayload, []string, error) {
	var payload joinPayload
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return payload, nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 8 {
		return payload, nil, errors.New("worker credential payload fields are invalid")
	}
	for _, key := range []string{"schemaVersion", "issuanceId", "clusterId", "expectedNodeName", "bootstrapTaint", "workerOnly", "expiresAt", "urls"} {
		if fields[key] == nil {
			return payload, nil, errors.New("worker credential payload fields are incomplete")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, nil, err
	}
	if payload.SchemaVersion != "blazn.dev/microk8s-worker-join/v1" || payload.IssuanceID == "" || payload.ClusterID != binding.ClusterID || payload.ExpectedNodeName != binding.ExpectedNodeName || payload.BootstrapTaint != binding.BootstrapTaint || !payload.WorkerOnly || !time.Now().Before(payload.ExpiresAt) {
		return payload, nil, errors.New("worker credential payload is invalid")
	}
	for _, candidate := range payload.URLs {
		parsed, err := url.Parse("https://" + candidate)
		parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || net.ParseIP(parsed.Hostname()) == nil || parsed.Port() == "" || len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) < 16 {
			return payload, nil, errors.New("worker credential endpoint is invalid")
		}
	}
	return payload, payload.URLs, nil
}
func readLimaVM(plan client.NodeInstallPlan, configuredPath string) (string, error) {
	var expected string
	for _, component := range plan.Components {
		if component.Name == "lima-worker-binding" && component.SourceClass == "embedded" && component.ArtifactType == "configuration" {
			expected = component.SHA256
			break
		}
	}
	if expected == "" {
		return "", errors.New("Lima binding component is unavailable")
	}
	path := configuredPath
	if path == "" {
		path = "/Library/Application Support/Blazn/lima-worker-binding.json"
	}
	value, err := readBoundedRegular(path, 64<<10)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != expected {
		return "", errors.New("Lima binding digest differs from signed component")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(value, &raw) != nil || len(raw) != 4 {
		return "", errors.New("Lima binding is invalid")
	}
	var binding struct {
		SchemaVersion string `json:"schemaVersion"`
		ClusterID     string `json:"clusterId"`
		VMName        string `json:"vmName"`
		WorkerName    string `json:"workerName"`
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || binding.SchemaVersion != "blazn.dev/lima-worker-binding/v1" || binding.ClusterID != plan.Cluster.ID || binding.WorkerName != plan.Hostname || binding.VMName == "" {
		return "", errors.New("Lima binding does not match signed plan")
	}
	return binding.VMName, nil
}
func decodeMaterial(material *RootMaterial) ([]byte, error) {
	if material == nil {
		return nil, errors.New("mutation material is missing")
	}
	value, err := base64.RawStdEncoding.DecodeString(material.ContentBase64)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != material.SHA256 {
		return nil, errors.New("mutation material digest mismatch")
	}
	return value, nil
}
func writeRootAtomic(path string, value []byte, mode os.FileMode, uid, gid int) error {
	if !canonicalPath(path) {
		return errors.New("root write path is unsafe")
	}
	if err := verifyNoSymlinkTraversal(path); err != nil {
		return err
	}
	return writeRootAtomicNative(path, value, mode, uid, gid)
}
func backupID(planID string, ordinal int64, content []byte) string {
	contentSum := sha256.Sum256(content)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%x", planID, ordinal, contentSum)))
	value := sum[:16]
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
func number(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}
func stringValue(value any) string { result, _ := value.(string); return result }
