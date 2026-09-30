package sandboxcontrol

import (
	"encoding/json"
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
	if observe(defaults) || observe(nil) {
		t.Fatal("a Pod missing the sandbox-node toleration must not match")
	}
	wildcard := map[string]any{"operator": "Exists"}
	if observe(append([]any{sandboxNode, wildcard}, defaults...)) || observe(append([]any{wildcard}, defaults...)) {
		t.Fatal("an injected wildcard toleration must not match")
	}
}
