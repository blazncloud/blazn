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

	"github.com/blazncloud/blazn/internal/proxycontract"
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
		if address.IsLoopback() || address.IsPrivate() || isNonPublicAddress(address) {
			return errors.New("external endpoint resolved to a non-public address")
		}
	default:
		return errors.New("unknown resolved address policy")
	}
	return nil
}

func isNonPublicAddress(address netip.Addr) bool {
	for _, prefix := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128",
		"100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		parsed := netip.MustParsePrefix(prefix)
		if parsed.Contains(address) {
			return true
		}
	}
	return false
}

func redirectPolicy(route proxycontract.Route) func(*http.Request, []*http.Request) error {
	return func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("redirect limit exceeded")
		}
		port := next.URL.Port()
		if port == "" {
			if next.URL.Scheme == "https" {
				port = "443"
			} else if next.URL.Scheme == "http" {
				port = "80"
			}
		}
		if next.URL.Scheme != route.Endpoint.Scheme || !strings.EqualFold(next.URL.Hostname(), route.Endpoint.Hostname) || port != strconv.Itoa(route.Endpoint.Port) {
			return errors.New("redirect leaves the validated route")
		}
		if next.URL.User != nil {
			return errors.New("redirect URL userinfo is forbidden")
		}
		base := strings.TrimRight(route.Endpoint.BasePath, "/") + "/"
		if next.URL.Path != strings.TrimRight(route.Endpoint.BasePath, "/") && !strings.HasPrefix(next.URL.Path, base) {
			return errors.New("redirect leaves the validated route base path")
		}
		return nil
	}
}

func authenticateAndStrip(header http.Header, listenerToken string) bool {
	authorization, apiKeys := header.Values("Authorization"), header.Values("x-api-key")
	var sourceCredential string
	validShape := false
	switch {
	case len(authorization) == 1 && len(apiKeys) == 0:
		parts := strings.SplitN(authorization[0], " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != "" {
			sourceCredential, validShape = parts[1], true
		}
	case len(authorization) == 0 && len(apiKeys) == 1 && apiKeys[0] != "":
		sourceCredential, validShape = apiKeys[0], true
	}
	valid := validShape && listenerToken != "" && subtle.ConstantTimeCompare([]byte(sourceCredential), []byte(listenerToken)) == 1
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
