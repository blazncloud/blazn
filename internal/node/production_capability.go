package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/KingJammin/blazn/internal/client"
)

type LiveNodeObservation struct {
	CPUMillis, MemoryBytes, DiskBytes            int64
	ServiceActive, NodeReady, Pressure           bool
	Binding                                      client.KubernetesBinding
	RuntimeClasses, SandboxBackends, ReasonCodes []string
}
type LiveNodeObserver interface {
	Observe(context.Context, client.NodeInstallPlan, client.KubernetesBinding) (LiveNodeObservation, error)
}
type ProductionCapabilityProvider struct {
	State    StateStore
	Observer LiveNodeObserver
}

func (p ProductionCapabilityProvider) Capability(ctx context.Context) (client.NodeCapability, error) {
	if p.State == nil {
		return client.NodeCapability{}, errors.New("node capability state is unavailable")
	}
	state, err := p.State.LoadRuntime()
	if err != nil {
		return client.NodeCapability{}, err
	}
	if state.KubernetesBinding == nil {
		return client.NodeCapability{}, errors.New("verified Kubernetes binding is unavailable")
	}
	plan := state.Exchange.Plan
	observer := p.Observer
	if observer == nil {
		observer = HostLiveNodeObserver{}
	}
	observed, err := observer.Observe(ctx, plan, *state.KubernetesBinding)
	if err != nil {
		return client.NodeCapability{}, err
	}
	if observed.Binding.ClusterID != state.KubernetesBinding.ClusterID || observed.Binding.NodeName != state.KubernetesBinding.NodeName || observed.Binding.NodeUID != state.KubernetesBinding.NodeUID {
		return client.NodeCapability{}, errors.New("live Kubernetes identity differs from root-verified binding")
	}
	health := client.NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}
	if !observed.ServiceActive || !observed.NodeReady || observed.Pressure || len(observed.ReasonCodes) > 0 {
		health.Status, health.ReasonCodes = "degraded", append([]string(nil), observed.ReasonCodes...)
		if !observed.ServiceActive {
			health.ReasonCodes = appendUnique(health.ReasonCodes, "service_inactive")
		}
		if !observed.NodeReady {
			health.ReasonCodes = appendUnique(health.ReasonCodes, "worker_not_ready")
		}
		if observed.Pressure {
			health.ReasonCodes = appendUnique(health.ReasonCodes, "worker_pressure")
		}
	}
	workerCPU, workerMemory := observed.CPUMillis-plan.ResourceBounds.ReservedCPUMillis, observed.MemoryBytes-plan.ResourceBounds.ReservedMemoryBytes
	if observed.CPUMillis < plan.Target.MinCPU*1000 || observed.MemoryBytes < plan.Target.MinMemoryBytes || observed.DiskBytes < plan.Target.MinDiskBytes || workerCPU < 1 || workerMemory < 1 {
		return client.NodeCapability{}, errors.New("live host capacity is below the signed minimum")
	}
	capability := client.NodeCapability{
		Version:         1,
		Host:            client.NodeHostCapacity{Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, CPUMillis: observed.CPUMillis, MemoryBytes: observed.MemoryBytes, DiskBytes: observed.DiskBytes, Accelerators: []client.NodeAccelerator{}, Health: health},
		Worker:          client.NodeWorkerCapacity{Platform: client.NodePlatformLinux, Architecture: plan.Target.Architecture, AllocatableCPUMillis: workerCPU, AllocatableMemoryBytes: workerMemory, AllocatableDiskBytes: observed.DiskBytes, Labels: cloneLabels(plan.Labels), Limits: client.NodeCapabilityLimits{MaxConcurrentSandboxes: plan.ResourceBounds.MaxConcurrentAgents, MaxConcurrentAgents: plan.ResourceBounds.MaxConcurrentAgents}, Health: health, KubernetesBinding: observed.Binding},
		SandboxBackends: nonNilStrings(observed.SandboxBackends), RuntimeClasses: nonNilStrings(observed.RuntimeClasses), LocalModels: []client.LocalModelCapability{},
	}
	if err := client.ValidateNodeCapability(capability); err != nil {
		return client.NodeCapability{}, err
	}
	return capability, nil
}

type HostLiveNodeObserver struct{}

func (HostLiveNodeObserver) Observe(ctx context.Context, plan client.NodeInstallPlan, binding client.KubernetesBinding) (LiveNodeObservation, error) {
	o := LiveNodeObservation{CPUMillis: int64(runtime.NumCPU()) * 1000, Binding: binding, RuntimeClasses: []string{}, SandboxBackends: []string{}, ReasonCodes: []string{}}
	memory, err := liveMemoryBytes(ctx)
	if err != nil {
		return o, err
	}
	o.MemoryBytes = memory
	var stat syscall.Statfs_t
	if err := syscall.Statfs(string(os.PathSeparator), &stat); err != nil {
		return o, err
	}
	o.DiskBytes = int64(stat.Bavail) * int64(stat.Bsize)
	platform := "linux"
	if plan.Target.Platform == client.NodePlatformMacOS {
		platform = "macos"
	}
	engine := NativeRootEngine{Platform: platform, Commands: FixedCommandExecutor{}}
	service, serviceErr := engine.serviceState(ctx, plan.NodeService)
	o.ServiceActive = serviceErr == nil && service.Service != nil && service.Service.Active
	output, err := engine.kubectl(ctx, plan, "get", "node", binding.NodeName, "-o", "json")
	if err != nil {
		o.ReasonCodes = append(o.ReasonCodes, "worker_observation_failed")
		return o, nil
	}
	var node struct {
		Metadata struct{ Name, UID, ResourceVersion string } `json:"metadata"`
		Status   struct {
			Conditions []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(output, &node) != nil || node.Metadata.Name != binding.NodeName || node.Metadata.UID == "" || node.Metadata.ResourceVersion == "" {
		return o, errors.New("live Kubernetes node response is invalid")
	}
	o.Binding.NodeUID, o.Binding.ResourceVersion = node.Metadata.UID, node.Metadata.ResourceVersion
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			o.NodeReady = true
		}
		if (condition.Type == "MemoryPressure" || condition.Type == "DiskPressure" || condition.Type == "PIDPressure") && condition.Status == "True" {
			o.Pressure = true
		}
	}
	if classes, classErr := engine.kubectl(ctx, plan, "get", "runtimeclass", "-o", "json"); classErr == nil {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if json.Unmarshal(classes, &list) == nil {
			for _, item := range list.Items {
				if item.Metadata.Name != "" {
					o.RuntimeClasses = append(o.RuntimeClasses, item.Metadata.Name)
				}
			}
		}
	}
	if resources, resourceErr := engine.kubectl(ctx, plan, "api-resources", "-o", "name"); resourceErr == nil && strings.Contains(string(resources), "sandboxes.agents.x-k8s.io") {
		controller, controllerErr := engine.kubectl(ctx, plan, "get", "deployment", "agent-sandbox-controller", "-n", "agent-sandbox-system", "-o", "json")
		if controllerErr == nil && agentSandboxControllerAvailable(controller) {
			o.SandboxBackends = append(o.SandboxBackends, "kubernetes-agent-sandbox")
		} else {
			o.ReasonCodes = appendUnique(o.ReasonCodes, "sandbox_controller_unavailable")
		}
	}
	return o, nil
}

func agentSandboxControllerAvailable(value []byte) bool {
	var deployment struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			AvailableReplicas  int64 `json:"availableReplicas"`
		} `json:"status"`
	}
	return json.Unmarshal(value, &deployment) == nil &&
		deployment.Metadata.Generation > 0 &&
		deployment.Status.ObservedGeneration == deployment.Metadata.Generation &&
		deployment.Status.AvailableReplicas > 0
}

func liveMemoryBytes(ctx context.Context) (int64, error) {
	if runtime.GOOS == "linux" {
		value, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(value), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				kb, parseErr := strconv.ParseInt(fields[1], 10, 64)
				return kb * 1024, parseErr
			}
		}
		return 0, errors.New("host memory is unavailable")
	}
	output, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, errors.New("host memory is unavailable")
	}
	return strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
}

func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
