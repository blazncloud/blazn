package router

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KingJammin/blazn/internal/proxycontract"
)

type CredentialProvider interface {
	DestinationCredential(context.Context, string) (string, error)
}

type CredentialAdapter interface {
	Apply(*http.Request, proxycontract.Route, string) error
}

type BearerCredentialAdapter struct{}

func (BearerCredentialAdapter) Apply(request *http.Request, _ proxycontract.Route, credential string) error {
	if credential == "" {
		return errors.New("destination credential is empty")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	return nil
}

type DNSResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type EndpointResolver struct{ DNS DNSResolver }

type ResolvedEndpoint struct {
	URL       *url.URL
	Addresses []netip.Addr
	Transport *http.Transport
}

func (r EndpointResolver) Resolve(ctx context.Context, route proxycontract.Route) (ResolvedEndpoint, error) {
	dns := r.DNS
	if dns == nil {
		dns = net.DefaultResolver
	}
	addresses, err := dns.LookupNetIP(ctx, "ip", route.Endpoint.Hostname)
	if err != nil || len(addresses) == 0 {
		return ResolvedEndpoint{}, fmt.Errorf("resolve endpoint: %w", err)
	}
	for _, address := range addresses {
		if err := validateAddress(route.Endpoint.ResolvedAddressPolicy, address.Unmap()); err != nil {
			return ResolvedEndpoint{}, err
		}
	}
	endpoint := &url.URL{Scheme: route.Endpoint.Scheme, Host: net.JoinHostPort(route.Endpoint.Hostname, strconv.Itoa(route.Endpoint.Port)), Path: route.Endpoint.BasePath}
	port := strconv.Itoa(route.Endpoint.Port)
	dialer := &net.Dialer{Timeout: time.Duration(route.HealthTimeoutMS) * time.Millisecond}
	pinned := append([]netip.Addr(nil), addresses...)
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: time.Duration(route.HealthTimeoutMS) * time.Millisecond,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, address := range pinned {
				connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
				if dialErr == nil {
					return connection, nil
				}
				last = dialErr
			}
			return nil, last
		},
	}
	return ResolvedEndpoint{URL: endpoint, Addresses: pinned, Transport: transport}, nil
}

func validateAddress(policy proxycontract.ResolvedAddressPolicy, address netip.Addr) error {
	if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return errors.New("endpoint resolved to a forbidden address")
	}
	switch policy {
	case proxycontract.AddressLoopbackOnly:
		if !address.IsLoopback() {
			return errors.New("local endpoint did not resolve to loopback")
		}
	case proxycontract.AddressNodeTunnel:
		if !address.IsLoopback() {
			return errors.New("POC node tunnel must terminate on loopback")
		}
	case proxycontract.AddressPublicUnicast:
		if address.IsLoopback() || address.IsPrivate() || isMetadataAddress(address) {
			return errors.New("external endpoint resolved to a non-public address")
		}
	default:
		return errors.New("unknown resolved address policy")
	}
	return nil
}

func isMetadataAddress(address netip.Addr) bool {
	for _, prefix := range []string{"169.254.169.254/32", "100.100.100.200/32", "fd00:ec2::254/128"} {
		parsed := netip.MustParsePrefix(prefix)
		if parsed.Contains(address) {
			return true
		}
	}
	return false
}

func redirectPolicy(route proxycontract.Route) func(*http.Request, []*http.Request) error {
	allowed := make(map[string]bool, len(route.Endpoint.HostnameAllowlist))
	for _, host := range route.Endpoint.HostnameAllowlist {
		allowed[strings.ToLower(host)] = true
	}
	return func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("redirect limit exceeded")
		}
		if next.URL.Scheme != route.Endpoint.Scheme || !allowed[strings.ToLower(next.URL.Hostname())] || next.URL.Port() != strconv.Itoa(route.Endpoint.Port) {
			return errors.New("redirect leaves the validated route")
		}
		return nil
	}
}

func authenticateAndStrip(header http.Header, listenerToken string) bool {
	values := make([]string, 0, 2)
	for _, raw := range header.Values("Authorization") {
		if parts := strings.SplitN(raw, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			values = append(values, parts[1])
		}
	}
	values = append(values, header.Values("x-api-key")...)
	valid := len(values) == 1 && listenerToken != "" && subtle.ConstantTimeCompare([]byte(values[0]), []byte(listenerToken)) == 1
	stripCredentialHeaders(header)
	return valid
}

func stripCredentialHeaders(header http.Header) {
	for name := range header {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "x-api-key":
			header.Del(name)
		}
	}
}
