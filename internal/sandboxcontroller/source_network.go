package sandboxcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

const (
	networkPolicyAPIVersion = "networking.k8s.io/v1"
	networkPolicyKind       = "NetworkPolicy"
	maxSourceCIDRs          = 64
)

type KubernetesSourceNetworkConfig struct {
	BaseURL     string
	HTTPClient  *http.Client
	DNSCIDRs    []string
	SourceCIDRs map[string][]string
}

type KubernetesSourceNetwork struct {
	base        string
	client      *http.Client
	dnsCIDRs    []string
	sourceCIDRs map[string][]string
}

type networkPolicy struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Metadata   networkPolicyMetadata `json:"metadata"`
	Spec       networkPolicySpec     `json:"spec"`
}

type networkPolicyMetadata struct {
	Name            string                  `json:"name"`
	Namespace       string                  `json:"namespace,omitempty"`
	UID             string                  `json:"uid,omitempty"`
	ResourceVersion string                  `json:"resourceVersion,omitempty"`
	Labels          map[string]string       `json:"labels"`
	OwnerReferences []networkPolicyOwnerRef `json:"ownerReferences"`
}

type networkPolicyOwnerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type networkPolicySpec struct {
	PodSelector networkPolicySelector `json:"podSelector"`
	PolicyTypes []string              `json:"policyTypes"`
	Ingress     []json.RawMessage     `json:"ingress,omitempty"`
	Egress      []networkPolicyEgress `json:"egress,omitempty"`
}

type networkPolicySelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []json.RawMessage `json:"matchExpressions,omitempty"`
}
type networkPolicyEgress struct {
	To    []networkPolicyPeer `json:"to"`
	Ports []networkPolicyPort `json:"ports"`
}
type networkPolicyPeer struct {
	IPBlock networkPolicyIPBlock `json:"ipBlock"`
}
type networkPolicyIPBlock struct {
	CIDR string `json:"cidr"`
}
type networkPolicyPort struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

func NewKubernetesSourceNetwork(config KubernetesSourceNetworkConfig) (*KubernetesSourceNetwork, error) {
	endpoint, err := url.Parse(config.BaseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Host == "" || endpoint.Path != "" || endpoint.RawPath != "" ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || !validKubernetesHost(endpoint.Hostname()) || !validKubernetesPort(endpoint.Port()) ||
		endpoint.Host != net.JoinHostPort(endpoint.Hostname(), endpoint.Port()) || config.HTTPClient == nil {
		return nil, errors.New("source NetworkPolicy client configuration is invalid")
	}
	dns, err := canonicalCIDRs(config.DNSCIDRs)
	if err != nil || len(dns) == 0 {
		return nil, errors.New("source NetworkPolicy DNS boundary is invalid")
	}
	sources := make(map[string][]string, len(config.SourceCIDRs))
	for host, values := range config.SourceCIDRs {
		if !validSourceHostname(host) || host != strings.ToLower(host) {
			return nil, errors.New("source NetworkPolicy host boundary is invalid")
		}
		canonical, err := canonicalCIDRs(values)
		if err != nil || len(canonical) == 0 {
			return nil, errors.New("source NetworkPolicy CIDR boundary is invalid")
		}
		sources[host] = canonical
	}
	return &KubernetesSourceNetwork{base: strings.TrimSuffix(config.BaseURL, "/"), client: config.HTTPClient, dnsCIDRs: dns, sourceCIDRs: sources}, nil
}

func (n *KubernetesSourceNetwork) Prepare(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation) error {
	if err := validateSourceObservation(item, observation); err != nil {
		return err
	}
	sourceCIDRs, err := n.itemSourceCIDRs(item)
	if err != nil {
		return err
	}
	deny := n.policy(item, observation, runtimePolicyName(item), nil)
	allow := n.policy(item, observation, bootstrapPolicyName(item), []networkPolicyEgress{
		{To: cidrPeers(n.dnsCIDRs), Ports: []networkPolicyPort{{Protocol: "UDP", Port: 53}, {Protocol: "TCP", Port: 53}}},
		{To: cidrPeers(sourceCIDRs), Ports: []networkPolicyPort{{Protocol: "TCP", Port: 443}}},
	})
	if _, err := n.ensure(ctx, deny); err != nil {
		return err
	}
	createdAllow, err := n.ensure(ctx, allow)
	if err != nil {
		return err
	}
	if err := n.verifyOwnedSet(ctx, item, []string{bootstrapPolicyName(item), runtimePolicyName(item)}); err != nil {
		return errors.Join(err, n.delete(ctx, createdAllow))
	}
	return nil
}

func (n *KubernetesSourceNetwork) Restrict(ctx context.Context, item WorkItem, observation sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) error {
	if err := validateSourceReceipt(item, observation, receipt); err != nil {
		return err
	}
	sourceCIDRs, err := n.itemSourceCIDRs(item)
	if err != nil {
		return err
	}
	deny := n.policy(item, observation, runtimePolicyName(item), nil)
	if _, err := n.ensure(ctx, deny); err != nil {
		return err
	}
	allow, status, err := n.get(ctx, bootstrapPolicyName(item))
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		wanted := n.policy(item, observation, bootstrapPolicyName(item), []networkPolicyEgress{
			{To: cidrPeers(n.dnsCIDRs), Ports: []networkPolicyPort{{Protocol: "UDP", Port: 53}, {Protocol: "TCP", Port: 53}}},
			{To: cidrPeers(sourceCIDRs), Ports: []networkPolicyPort{{Protocol: "TCP", Port: 443}}},
		})
		if !sameNetworkPolicy(allow, wanted) || allow.Metadata.UID == "" || allow.Metadata.ResourceVersion == "" {
			return errors.New("bootstrap NetworkPolicy identity changed")
		}
		if err := n.delete(ctx, allow); err != nil {
			return err
		}
	} else if status != http.StatusNotFound {
		return fmt.Errorf("bootstrap NetworkPolicy lookup returned HTTP %d", status)
	}
	_, status, err = n.get(ctx, bootstrapPolicyName(item))
	if err != nil || status != http.StatusNotFound {
		return errors.Join(err, errors.New("bootstrap NetworkPolicy still exists"))
	}
	return n.verifyOwnedSet(ctx, item, []string{runtimePolicyName(item)})
}

func (n *KubernetesSourceNetwork) itemSourceCIDRs(item WorkItem) ([]string, error) {
	set := map[string]bool{}
	for _, source := range item.Sources {
		parsed, err := url.Parse(source.URL)
		if err != nil || parsed.Hostname() == "" {
			return nil, errors.New("source URL host is invalid")
		}
		values, ok := n.sourceCIDRs[parsed.Hostname()]
		if !ok {
			return nil, errors.New("source host lacks an approved CIDR boundary")
		}
		for _, value := range values {
			set[value] = true
		}
	}
	if len(set) == 0 || len(set) > maxSourceCIDRs {
		return nil, errors.New("source CIDR set is invalid")
	}
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}

func (n *KubernetesSourceNetwork) policy(item WorkItem, observation sandboxcontrol.AdmissionObservation, name string, egress []networkPolicyEgress) networkPolicy {
	labels := map[string]string{sandboxcontrol.ManagedLabel: "true", sandboxcontrol.WorkspaceLabel: item.WorkspaceID, sandboxcontrol.SandboxIDLabel: item.SandboxID}
	policyTypes := []string{"Ingress", "Egress"}
	if egress != nil {
		policyTypes = []string{"Egress"}
	}
	return networkPolicy{APIVersion: networkPolicyAPIVersion, Kind: networkPolicyKind,
		Metadata: networkPolicyMetadata{Name: name, Namespace: sandboxcontrol.Namespace, Labels: labels,
			OwnerReferences: []networkPolicyOwnerRef{{APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind, Name: item.SandboxID, UID: observation.Sandbox.UID, Controller: true}}},
		Spec: networkPolicySpec{PodSelector: networkPolicySelector{MatchLabels: labels}, PolicyTypes: policyTypes, Egress: egress}}
}

func (n *KubernetesSourceNetwork) ensure(ctx context.Context, wanted networkPolicy) (networkPolicy, error) {
	current, status, err := n.get(ctx, wanted.Metadata.Name)
	if err != nil {
		return networkPolicy{}, err
	}
	if status == http.StatusOK {
		if !sameNetworkPolicy(current, wanted) || current.Metadata.UID == "" || current.Metadata.ResourceVersion == "" {
			return networkPolicy{}, errors.New("owned NetworkPolicy differs from the exact intent")
		}
		return current, nil
	}
	if status != http.StatusNotFound {
		return networkPolicy{}, fmt.Errorf("NetworkPolicy lookup returned HTTP %d", status)
	}
	var created networkPolicy
	status, err = n.call(ctx, http.MethodPost, n.collectionPath(), wanted, &created)
	if err != nil || status != http.StatusCreated || !sameNetworkPolicy(created, wanted) || created.Metadata.UID == "" || created.Metadata.ResourceVersion == "" {
		return networkPolicy{}, errors.Join(err, errors.New("NetworkPolicy creation did not return exact identity"))
	}
	return created, nil
}

func (n *KubernetesSourceNetwork) get(ctx context.Context, name string) (networkPolicy, int, error) {
	var policy networkPolicy
	status, err := n.call(ctx, http.MethodGet, n.resourcePath(name), nil, &policy)
	return policy, status, err
}

func (n *KubernetesSourceNetwork) delete(ctx context.Context, policy networkPolicy) error {
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": policy.Metadata.UID, "resourceVersion": policy.Metadata.ResourceVersion}, "propagationPolicy": "Foreground"}
	status, err := n.call(ctx, http.MethodDelete, n.resourcePath(policy.Metadata.Name), body, nil)
	if err != nil || status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNoContent {
		return errors.Join(err, fmt.Errorf("NetworkPolicy deletion returned HTTP %d", status))
	}
	return nil
}

func (n *KubernetesSourceNetwork) verifyOwnedSet(ctx context.Context, item WorkItem, wanted []string) error {
	var list struct {
		Items []networkPolicy `json:"items"`
	}
	status, err := n.call(ctx, http.MethodGet, n.collectionPath(), nil, &list)
	if err != nil || status != http.StatusOK {
		return errors.Join(err, fmt.Errorf("NetworkPolicy list returned HTTP %d", status))
	}
	actual := make([]string, 0, len(wanted))
	wantedSet := make(map[string]bool, len(wanted))
	for _, name := range wanted {
		wantedSet[name] = true
	}
	podLabels := map[string]string{sandboxcontrol.ManagedLabel: "true", sandboxcontrol.WorkspaceLabel: item.WorkspaceID, sandboxcontrol.SandboxIDLabel: item.SandboxID}
	for _, policy := range list.Items {
		if wantedSet[policy.Metadata.Name] {
			actual = append(actual, policy.Metadata.Name)
			continue
		}
		if selectorMayMatch(policy.Spec.PodSelector, podLabels) {
			return errors.New("an unowned NetworkPolicy may select the Sandbox Pod")
		}
	}
	sort.Strings(actual)
	sort.Strings(wanted)
	if !reflect.DeepEqual(actual, wanted) {
		return errors.New("managed NetworkPolicy set differs from the exact phase")
	}
	return nil
}

func selectorMayMatch(selector networkPolicySelector, labels map[string]string) bool {
	if len(selector.MatchExpressions) != 0 {
		return true
	}
	for name, value := range selector.MatchLabels {
		if labels[name] != value {
			return false
		}
	}
	return true
}

func (n *KubernetesSourceNetwork) call(ctx context.Context, method, endpoint string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "blazn-sandbox-controller-source-network/v1")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := n.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if output != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		decoder := json.NewDecoder(response.Body)
		if err := decoder.Decode(output); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return response.StatusCode, errors.New("NetworkPolicy response is invalid")
		}
	}
	return response.StatusCode, nil
}

func (n *KubernetesSourceNetwork) collectionPath() string {
	return n.base + "/apis/networking.k8s.io/v1/namespaces/" + sandboxcontrol.Namespace + "/networkpolicies"
}
func (n *KubernetesSourceNetwork) resourcePath(name string) string {
	return n.collectionPath() + "/" + url.PathEscape(name)
}

func sameNetworkPolicy(actual, wanted networkPolicy) bool {
	return actual.APIVersion == wanted.APIVersion && actual.Kind == wanted.Kind && actual.Metadata.Name == wanted.Metadata.Name &&
		actual.Metadata.Namespace == wanted.Metadata.Namespace && reflect.DeepEqual(actual.Metadata.Labels, wanted.Metadata.Labels) &&
		reflect.DeepEqual(actual.Metadata.OwnerReferences, wanted.Metadata.OwnerReferences) && reflect.DeepEqual(actual.Spec, wanted.Spec)
}

func canonicalCIDRs(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > maxSourceCIDRs {
		return nil, errors.New("CIDR list is invalid")
	}
	set := map[string]bool{}
	for _, value := range values {
		ip, network, err := net.ParseCIDR(value)
		if err != nil || network.String() != value || !ip.Equal(network.IP) {
			return nil, errors.New("CIDR is noncanonical")
		}
		set[value] = true
	}
	canonical := make([]string, 0, len(set))
	for value := range set {
		canonical = append(canonical, value)
	}
	sort.Strings(canonical)
	return canonical, nil
}

func cidrPeers(values []string) []networkPolicyPeer {
	peers := make([]networkPolicyPeer, len(values))
	for index, value := range values {
		peers[index] = networkPolicyPeer{IPBlock: networkPolicyIPBlock{CIDR: value}}
	}
	return peers
}

func runtimePolicyName(item WorkItem) string   { return "blazn-runtime-deny-" + item.SandboxID }
func bootstrapPolicyName(item WorkItem) string { return "blazn-source-egress-" + item.SandboxID }
