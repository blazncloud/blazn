package sandboxcontrol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestObservedPodAcceptsOnlyTheSandboxNodeTolerationPlusDefaults(t *testing.T) {
	expected := kubePodSpec{ServiceAccountName: ServiceAccountName, RestartPolicy: "Never", NodeSelector: map[string]string{"blazn.dev/sandbox-eligible": "true"}, Tolerations: []kubeToleration{SandboxNodeToleration}, SecurityContext: map[string]any{}, Containers: []kubeContainer{{Name: "main", Image: "image", Command: []string{"run"}}}}
	defaults := []any{
		map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
		map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
	}
	sandboxNode := map[string]any{"key": "blazn.dev/sandbox-node", "operator": "Equal", "value": "true", "effect": "NoSchedule"}
	observe := func(tolerations []any) bool {
		spec, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(spec, &object); err != nil {
			t.Fatal(err)
		}
		if tolerations == nil {
			delete(object, "tolerations")
		} else {
			object["tolerations"] = tolerations
		}
		raw, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		return sameObservedPodMaterialSpec(raw, expected)
	}
	if !observe(append([]any{sandboxNode}, defaults...)) {
		t.Fatal("the sandbox-node toleration followed by the admission defaults must match")
	}
	if !observe([]any{sandboxNode}) {
		t.Fatal("the sandbox-node toleration without defaults must match")
	}
	if !observe(defaults) || !observe(nil) {
		t.Fatal("a Pod admitted before the sandbox-node toleration (defaults only or none) must still match")
	}
	wildcard := map[string]any{"operator": "Exists"}
	if observe(append([]any{sandboxNode, wildcard}, defaults...)) || observe(append([]any{wildcard}, defaults...)) {
		t.Fatal("an injected wildcard toleration must not match")
	}
}

func TestLegacySandboxSpecWithoutTolerationStillMatches(t *testing.T) {
	manifest := render(testCreate(), "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64))
	legacy := manifest.Spec
	legacy.PodTemplate.Spec.Tolerations = nil
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMaterialSpec(raw, legacyTolerationSpec(raw, manifest.Spec)) {
		t.Fatal("a Sandbox created before the sandbox-node toleration must still match")
	}
	widened := manifest.Spec
	widened.PodTemplate.Spec.Tolerations = []kubeToleration{{Operator: "Exists"}}
	raw, err = json.Marshal(widened)
	if err != nil {
		t.Fatal(err)
	}
	if sameMaterialSpec(raw, legacyTolerationSpec(raw, manifest.Spec)) {
		t.Fatal("a widened toleration must not match")
	}
}

func TestRenderedPodsTargetTheWorkspaceNodesAndConfiguredLocalQueue(t *testing.T) {
	defer func() { _ = SetLocalQueue(QueueName) }()
	request := testCreate()
	manifest := render(request, "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64))
	if manifest.Spec.PodTemplate.Spec.NodeSelector[WorkspaceNodeLabel] != request.WorkspaceID || manifest.Spec.PodTemplate.Metadata.Labels[QueueLabel] != QueueName {
		t.Fatalf("default render: %#v %#v", manifest.Spec.PodTemplate.Spec.NodeSelector, manifest.Spec.PodTemplate.Metadata.Labels)
	}
	if err := SetLocalQueue("other-queue"); err == nil {
		t.Fatal("an unreviewed LocalQueue must be refused")
	}
	if err := SetLocalQueue(DedicatedLocalQueue); err != nil {
		t.Fatal(err)
	}
	manifest = render(request, "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64))
	if manifest.Spec.PodTemplate.Metadata.Labels[QueueLabel] != DedicatedLocalQueue || !blaznQueue(QueueName) || !blaznQueue(DedicatedLocalQueue) || blaznQueue("m1-local") {
		t.Fatalf("dedicated queue render: %#v", manifest.Spec.PodTemplate.Metadata.Labels)
	}
}

func TestLegacySandboxWithoutWorkspaceSelectorStillMatches(t *testing.T) {
	manifest := render(testCreate(), "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64))
	legacy := manifest.Spec
	legacy.PodTemplate.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64", "blazn.dev/sandbox-eligible": "true"}
	legacy.PodTemplate.Spec.Tolerations = nil
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMaterialSpec(raw, legacyTolerationSpec(raw, manifest.Spec)) {
		t.Fatal("a Sandbox created before workspace placement must still match")
	}
	foreign := manifest.Spec
	foreign.PodTemplate.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64", "blazn.dev/sandbox-eligible": "true", WorkspaceNodeLabel: "00000000-0000-4000-8000-000000000000"}
	raw, err = json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if sameMaterialSpec(raw, legacyTolerationSpec(raw, manifest.Spec)) {
		t.Fatal("a Sandbox pinned to another workspace's nodes must not match")
	}
}
