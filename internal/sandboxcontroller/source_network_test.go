package sandboxcontroller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type fakeNetworkPolicyAPI struct {
	t        *testing.T
	mu       sync.Mutex
	policies map[string]networkPolicy
	order    []string
}

func (f *fakeNetworkPolicyAPI) serve(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	collection := "/apis/networking.k8s.io/v1/namespaces/" + sandboxcontrol.Namespace + "/networkpolicies"
	response.Header().Set("Content-Type", "application/json")
	if request.URL.Path == collection && request.Method == http.MethodGet {
		items := make([]networkPolicy, 0, len(f.policies))
		for _, policy := range f.policies {
			items = append(items, policy)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"items": items})
		return
	}
	if request.URL.Path == collection && request.Method == http.MethodPost {
		var policy networkPolicy
		if err := json.NewDecoder(request.Body).Decode(&policy); err != nil {
			f.t.Fatal(err)
		}
		if _, exists := f.policies[policy.Metadata.Name]; exists {
			response.WriteHeader(http.StatusConflict)
			return
		}
		policy.Metadata.UID = "uid-" + policy.Metadata.Name
		policy.Metadata.ResourceVersion = "1"
		f.policies[policy.Metadata.Name] = policy
		f.order = append(f.order, "create:"+policy.Metadata.Name)
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(policy)
		return
	}
	prefix := collection + "/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		response.WriteHeader(http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(request.URL.Path, prefix)
	policy, exists := f.policies[name]
	switch request.Method {
	case http.MethodGet:
		if !exists {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(response).Encode(policy)
	case http.MethodDelete:
		if !exists {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		var options struct {
			Preconditions map[string]string `json:"preconditions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&options); err != nil || options.Preconditions["uid"] != policy.Metadata.UID || options.Preconditions["resourceVersion"] != policy.Metadata.ResourceVersion {
			f.t.Fatalf("delete options=%#v err=%v", options, err)
		}
		delete(f.policies, name)
		f.order = append(f.order, "delete:"+name)
		_ = json.NewEncoder(response).Encode(map[string]string{"status": "Success"})
	default:
		response.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestKubernetesSourceNetworkTransitionsFromBoundedBootstrapToDefaultDeny(t *testing.T) {
	api := &fakeNetworkPolicyAPI{t: t, policies: map[string]networkPolicy{}}
	server := httptest.NewTLSServer(http.HandlerFunc(api.serve))
	defer server.Close()
	network, err := NewKubernetesSourceNetwork(KubernetesSourceNetworkConfig{BaseURL: server.URL, HTTPClient: server.Client(),
		DNSCIDRs: []string{"10.152.183.10/32"}, SourceCIDRs: map[string][]string{"example.test": {"203.0.113.4/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, state := createFixture(t)
	item.Sources = []Source{{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
	observation := *state.AdmissionObservation
	if err := network.Prepare(context.Background(), item, observation); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	if len(api.policies) != 2 || len(api.policies[bootstrapPolicyName(item)].Spec.Egress) != 2 || len(api.policies[runtimePolicyName(item)].Spec.Egress) != 0 {
		t.Fatalf("bootstrap policies=%#v", api.policies)
	}
	dns := api.policies[bootstrapPolicyName(item)].Spec.Egress[0].To
	if len(dns) != 1 || dns[0].IPBlock != nil || dns[0].NamespaceSelector == nil || dns[0].PodSelector == nil ||
		dns[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" || dns[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("DNS peer is not bound to the kube-system/kube-dns identity: %#v", dns)
	}
	api.mu.Unlock()
	manifest := sourceManifest(item.Sources)
	receipt, err := sandboxio.NewSourceMaterializationReceipt(manifest, []sandboxio.SourceMaterialization{{Name: "repo", URL: item.Sources[0].URL,
		Destination: item.Sources[0].Destination, Commit: item.Sources[0].Commit, Tree: item.Sources[0].Commit,
		ContentDigest: "sha256:" + strings.Repeat("e", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := network.Restrict(context.Background(), item, observation, receipt); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.policies) != 1 || api.policies[runtimePolicyName(item)].Metadata.Name == "" ||
		len(api.order) != 3 || api.order[0] != "create:"+runtimePolicyName(item) || api.order[1] != "create:"+bootstrapPolicyName(item) || api.order[2] != "delete:"+bootstrapPolicyName(item) {
		t.Fatalf("runtime policies=%#v order=%v", api.policies, api.order)
	}
}

func TestKubernetesSourceNetworkReplacesLegacyDNSIPWithoutOpeningEgress(t *testing.T) {
	api := &fakeNetworkPolicyAPI{t: t, policies: map[string]networkPolicy{}}
	server := httptest.NewTLSServer(http.HandlerFunc(api.serve))
	defer server.Close()
	network, err := NewKubernetesSourceNetwork(KubernetesSourceNetworkConfig{BaseURL: server.URL, HTTPClient: server.Client(),
		DNSCIDRs: []string{"10.1.114.113/32"}, SourceCIDRs: map[string][]string{"example.test": {"203.0.113.4/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, state := createFixture(t)
	item.Sources = []Source{{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
	observation := *state.AdmissionObservation
	labels := map[string]string{sandboxcontrol.ManagedLabel: "true", sandboxcontrol.WorkspaceLabel: item.WorkspaceID, sandboxcontrol.SandboxIDLabel: item.SandboxID}
	runtimePolicy := network.policy(item, observation, runtimePolicyName(item), nil)
	runtimePolicy.Metadata.UID = "uid-runtime"
	runtimePolicy.Metadata.ResourceVersion = "1"
	api.policies[runtimePolicyName(item)] = runtimePolicy
	bootstrapPolicy := network.policy(item, observation, bootstrapPolicyName(item), []networkPolicyEgress{
		{To: cidrPeers(network.dnsCIDRs), Ports: []networkPolicyPort{{Protocol: "UDP", Port: 53}, {Protocol: "TCP", Port: 53}}},
		{To: cidrPeers([]string{"203.0.113.4/32"}), Ports: []networkPolicyPort{{Protocol: "TCP", Port: 443}}},
	})
	bootstrapPolicy.Metadata.Labels = labels
	bootstrapPolicy.Metadata.UID = "uid-bootstrap"
	bootstrapPolicy.Metadata.ResourceVersion = "1"
	api.policies[bootstrapPolicyName(item)] = bootstrapPolicy
	if err := network.Prepare(context.Background(), item, observation); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	updated := api.policies[bootstrapPolicyName(item)]
	if len(updated.Spec.Egress) != 2 || len(updated.Spec.Egress[0].To) != 1 || updated.Spec.Egress[0].To[0].PodSelector == nil ||
		len(api.order) != 2 || api.order[0] != "delete:"+bootstrapPolicyName(item) || api.order[1] != "create:"+bootstrapPolicyName(item) {
		t.Fatalf("legacy replacement policy=%#v order=%v", updated, api.order)
	}
}

func TestKubernetesSourceNetworkRejectsUnapprovedHostsCIDRsAndUnknownOwnedPolicy(t *testing.T) {
	for _, config := range []KubernetesSourceNetworkConfig{
		{BaseURL: "https://kubernetes.test", HTTPClient: http.DefaultClient, DNSCIDRs: []string{"10.0.0.1/24"}, SourceCIDRs: map[string][]string{"example.test": {"203.0.113.0/24"}}},
		{BaseURL: "https://kubernetes.test", HTTPClient: http.DefaultClient, DNSCIDRs: []string{"10.0.0.0/24"}, SourceCIDRs: map[string][]string{"example.test": {"203.0.113.4/32"}}},
		{BaseURL: "https://kubernetes.test", HTTPClient: http.DefaultClient, DNSCIDRs: []string{"10.0.0.1/32"}, SourceCIDRs: map[string][]string{"example.test": {"0.0.0.0/0"}}},
		{BaseURL: "https://kubernetes.test", HTTPClient: http.DefaultClient, DNSCIDRs: []string{"::1/128"}, SourceCIDRs: map[string][]string{"example.test": {"2001:db8::1/128"}}},
		{BaseURL: "https://kubernetes.test", HTTPClient: http.DefaultClient, DNSCIDRs: []string{"10.0.0.1/32"}, SourceCIDRs: map[string][]string{"Example.test": {"203.0.113.0/24"}}},
	} {
		if _, err := NewKubernetesSourceNetwork(config); err == nil {
			t.Fatal("unsafe NetworkPolicy configuration accepted")
		}
	}
	item, state := createFixture(t)
	item.Sources = []Source{{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
	labels := map[string]string{sandboxcontrol.ManagedLabel: "true", sandboxcontrol.WorkspaceLabel: item.WorkspaceID, sandboxcontrol.SandboxIDLabel: item.SandboxID}
	api := &fakeNetworkPolicyAPI{t: t, policies: map[string]networkPolicy{"unknown-allow": {Metadata: networkPolicyMetadata{Name: "unknown-allow", Labels: labels}}}}
	server := httptest.NewTLSServer(http.HandlerFunc(api.serve))
	defer server.Close()
	network, err := NewKubernetesSourceNetwork(KubernetesSourceNetworkConfig{BaseURL: server.URL, HTTPClient: server.Client(),
		DNSCIDRs: []string{"10.152.183.10/32"}, SourceCIDRs: map[string][]string{"example.test": {"203.0.113.4/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := network.Prepare(context.Background(), item, *state.AdmissionObservation); err == nil {
		t.Fatal("unknown managed policy was accepted")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if _, exists := api.policies[bootstrapPolicyName(item)]; exists {
		t.Fatal("failed policy-set verification left temporary source egress behind")
	}
}
