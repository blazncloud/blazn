package router

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/blazncloud/blazn/internal/proxycontract"
)

const maxPolicyBytes = 1 << 20

// LoadPolicy reads an owner-only, regular policy file and applies both the
// generated schema checks and router semantic checks before any network work.
func LoadPolicy(path string) (proxycontract.Policy, string, error) {
	var zero proxycontract.Policy
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return zero, "", fmt.Errorf("%w: stat: %v", ErrPolicyInvalid, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return zero, "", fmt.Errorf("%w: policy must be a regular file", ErrPolicyInvalid)
	}
	f, err := openPolicyFile(clean)
	if err != nil {
		return zero, "", fmt.Errorf("%w: open: %v", ErrPolicyInvalid, err)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return zero, "", fmt.Errorf("%w: policy changed while opening", ErrPolicyInvalid)
	}
	if runtime.GOOS != "windows" && openedInfo.Mode().Perm()&0o077 != 0 {
		return zero, "", fmt.Errorf("%w: policy must not be accessible by group or others", ErrPolicyInvalid)
	}
	if err := verifyPolicyOwner(openedInfo); err != nil {
		return zero, "", fmt.Errorf("%w: %v", ErrPolicyInvalid, err)
	}
	policy, err := proxycontract.DecodePolicy(io.LimitReader(f, maxPolicyBytes+1))
	if err != nil {
		return zero, "", fmt.Errorf("%w: %v", ErrPolicyInvalid, err)
	}
	if openedInfo.Size() > maxPolicyBytes {
		return zero, "", fmt.Errorf("%w: policy exceeds %d bytes", ErrPolicyInvalid, maxPolicyBytes)
	}
	if err := validatePolicySemantics(policy); err != nil {
		return zero, "", fmt.Errorf("%w: %v", ErrPolicyInvalid, err)
	}
	digest, err := proxycontract.ContractDigest(policy)
	if err != nil {
		return zero, "", fmt.Errorf("%w: digest: %v", ErrPolicyInvalid, err)
	}
	return policy, digest, nil
}

func validatePolicySemantics(policy proxycontract.Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if policy.Fallback.MaxAttempts > 2 {
		return fmt.Errorf("maximum two attempts are permitted")
	}
	for name, alias := range policy.Aliases {
		if len(alias.RouteIDs) > policy.Fallback.MaxAttempts {
			return fmt.Errorf("alias %q has more routes than maxAttempts", name)
		}
	}
	for _, route := range policy.Routes {
		if strings.EqualFold(route.Endpoint.Hostname, "localhost") {
			return fmt.Errorf("route %s must use an explicit loopback address", route.ID)
		}
		if route.Endpoint.BasePath != "/v1" {
			return fmt.Errorf("route %s basePath must be /v1 for the POC", route.ID)
		}
		if route.DestinationClass == proxycontract.DestinationLocalNode && !strings.HasPrefix(route.CredentialRef, "node-route://") {
			return fmt.Errorf("route %s local credentialRef must use node-route://", route.ID)
		}
		if (route.DestinationClass == proxycontract.DestinationProvider || route.DestinationClass == proxycontract.DestinationBlaznCloud) && !strings.HasPrefix(route.CredentialRef, "workspace-vault://") {
			return fmt.Errorf("route %s external credentialRef must use workspace-vault://", route.ID)
		}
		if route.DestinationProtocol == proxycontract.ProtocolAnthropicMessages {
			return fmt.Errorf("route %s: Anthropic destination translation is outside this lane", route.ID)
		}
	}
	return nil
}

type routeIndex struct {
	policy proxycontract.Policy
	byID   map[string]proxycontract.Route
}

func newRouteIndex(policy proxycontract.Policy) (*routeIndex, error) {
	if err := validatePolicySemantics(policy); err != nil {
		return nil, err
	}
	index := &routeIndex{policy: policy, byID: make(map[string]proxycontract.Route, len(policy.Routes))}
	for _, route := range policy.Routes {
		index.byID[route.ID] = route
	}
	return index, nil
}

func (r *routeIndex) selectRoutes(request proxycontract.NormalizedRequest) ([]proxycontract.Route, error) {
	return r.selectRoutesFor(routedRequest{normalized: request})
}

func (r *routeIndex) selectRoutesFor(request routedRequest) ([]proxycontract.Route, error) {
	normalized := request.normalized
	alias, ok := r.policy.Aliases[normalized.ModelAlias]
	if !ok {
		return nil, safeError("model_not_found", "model alias is not defined by policy", 404, false)
	}
	if normalized.DataClass != alias.DataClass {
		return nil, safeError("policy_denied", "request data class does not match the model alias", 403, false)
	}
	if normalized.Limits.MaxOutputTokens > r.policy.RequestLimits.MaxOutputTokens {
		return nil, safeError("policy_denied", "requested output exceeds policy limit", 403, false)
	}
	if normalized.Stream && !r.policy.RequestLimits.Streaming {
		return nil, safeError("unsupported_capability", "streaming is disabled by policy", 400, false)
	}
	selected := make([]proxycontract.Route, 0, len(alias.RouteIDs))
	for _, id := range alias.RouteIDs {
		route := r.byID[id]
		if !containsProtocol(route.SourceProtocols, normalized.Protocol) || !containsDataClass(route.AcceptedDataClasses, normalized.DataClass) || !containsBoundary(alias.AllowedDestinationBoundaries, route.DataBoundary) {
			continue
		}
		if !capabilitiesCover(route.Capabilities, normalized.CapabilitiesRequired) || costRank(route.CostClass) > costRank(r.policy.RequestLimits.MaxCostClass) {
			continue
		}
		if !routePreservesRequest(route, request) {
			continue
		}
		selected = append(selected, route)
	}
	if len(selected) == 0 {
		return nil, safeError("no_compliant_route", "no route satisfies policy and request capabilities", 403, false)
	}
	if len(selected) > r.policy.Fallback.MaxAttempts {
		selected = selected[:r.policy.Fallback.MaxAttempts]
	}
	return selected, nil
}

func routePreservesRequest(route proxycontract.Route, request routedRequest) bool {
	if len(request.normalized.Limits.Stop) > 0 && route.DestinationProtocol != proxycontract.ProtocolOpenAIChat {
		return false
	}
	metadata := request.source.responses
	if metadata == nil {
		return true
	}
	requiresResponses := len(metadata.include) > 0 || len(metadata.reasoning) > 0 && string(metadata.reasoning) != "null" || len(metadata.reasoningItems) > 0 || len(metadata.streamOptions) > 0 && string(metadata.streamOptions) != "null" || len(metadata.clientMetadata) > 0 && string(metadata.clientMetadata) != "null"
	return !requiresResponses || route.DestinationProtocol == proxycontract.ProtocolOpenAIResponses
}

func containsProtocol(values []proxycontract.Protocol, want proxycontract.Protocol) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsDataClass(values []proxycontract.DataClass, want proxycontract.DataClass) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsBoundary(values []proxycontract.DataBoundary, want proxycontract.DataBoundary) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func capabilitiesCover(have, want []proxycontract.Capability) bool {
	set := make(map[proxycontract.Capability]bool, len(have))
	for _, capability := range have {
		set[capability] = true
	}
	for _, capability := range want {
		if !set[capability] {
			return false
		}
	}
	return true
}
func costRank(value proxycontract.CostClass) int {
	switch value {
	case proxycontract.CostLocal:
		return 0
	case proxycontract.CostIncluded:
		return 1
	case proxycontract.CostMeteredLow:
		return 2
	case proxycontract.CostMeteredHigh:
		return 3
	default:
		return 99
	}
}
