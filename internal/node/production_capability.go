package node

import (
	"context"
	"errors"

	"github.com/KingJammin/blazn/internal/client"
)

type ProductionCapabilityProvider struct {
	State StateStore
}

func (p ProductionCapabilityProvider) Capability(context.Context) (client.NodeCapability, error) {
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
	cpu := plan.Target.MinCPU * 1000
	memory := plan.Target.MinMemoryBytes
	disk := plan.Target.MinDiskBytes
	workerCPU := cpu - plan.ResourceBounds.ReservedCPUMillis
	workerMemory := memory - plan.ResourceBounds.ReservedMemoryBytes
	if workerCPU < 1 || workerMemory < 1 || disk < 1 {
		return client.NodeCapability{}, errors.New("signed node capacity bounds are invalid")
	}
	health := client.NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}
	capability := client.NodeCapability{
		Version:         1,
		Host:            client.NodeHostCapacity{Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, CPUMillis: cpu, MemoryBytes: memory, DiskBytes: disk, Accelerators: []client.NodeAccelerator{}, Health: health},
		Worker:          client.NodeWorkerCapacity{Platform: client.NodePlatformLinux, Architecture: plan.Target.Architecture, AllocatableCPUMillis: workerCPU, AllocatableMemoryBytes: workerMemory, AllocatableDiskBytes: disk, Labels: cloneLabels(plan.Labels), Limits: client.NodeCapabilityLimits{MaxConcurrentSandboxes: plan.ResourceBounds.MaxConcurrentAgents, MaxConcurrentAgents: plan.ResourceBounds.MaxConcurrentAgents}, Health: health, KubernetesBinding: *state.KubernetesBinding},
		SandboxBackends: []string{}, RuntimeClasses: []string{}, LocalModels: []client.LocalModelCapability{},
	}
	if err := client.ValidateNodeCapability(capability); err != nil {
		return client.NodeCapability{}, err
	}
	return capability, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}
