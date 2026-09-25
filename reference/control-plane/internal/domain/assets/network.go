package assets

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// SystemResolver performs a fresh operating-system resolver lookup for each
// connect or redirect decision. It deliberately carries no application cache.
type SystemResolver struct {
	Resolver *net.Resolver
}

func (resolver SystemResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	lookup := resolver.Resolver
	if lookup == nil {
		lookup = net.DefaultResolver
	}
	addresses, err := lookup.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	result := make([]net.IP, len(addresses))
	for index, address := range addresses {
		result[index] = append(net.IP(nil), address...)
	}
	return result, nil
}

// AddressClass is the connection-time classification of one resolved address.
type AddressClass string

const (
	AddressPublic    AddressClass = "public"
	AddressLoopback  AddressClass = "loopback"
	AddressLinkLocal AddressClass = "link-local"
	AddressPrivate   AddressClass = "private"
	AddressMetadata  AddressClass = "cloud-metadata"
	AddressDenied    AddressClass = "denied"
)

// ClassifyIP classifies one address. Metadata and non-global ranges are denied.
func ClassifyIP(ip net.IP) AddressClass {
	if ip == nil {
		return AddressDenied
	}
	ip = normalizeIP(ip)
	switch {
	case isMetadataIP(ip):
		return AddressMetadata
	case ip.IsLoopback():
		return AddressLoopback
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return AddressLinkLocal
	case ip.IsPrivate() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast():
		return AddressPrivate
	default:
		return AddressPublic
	}
}

func (class AddressClass) Allowed() bool { return class == AddressPublic }

// ResolvedEndpoint is one connect or redirect hop after a fresh lookup.
type ResolvedEndpoint struct {
	URL       string
	Host      string
	Addresses []net.IP
}

// EvaluateEndpoint resolves host and requires every returned address to be
// public. An empty answer is denied. Callers must invoke this again for the
// next redirect; a previous allow does not carry forward.
func EvaluateEndpoint(ctx context.Context, resolver NameResolver, rawURL string) (ResolvedEndpoint, error) {
	if resolver == nil {
		return ResolvedEndpoint{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	parsed, err := parseBrokerURL(rawURL)
	if err != nil {
		return ResolvedEndpoint{}, NewError(CategoryNetwork, ReasonNetworkDenied)
	}
	host := parsed.Hostname()
	if deniedName(host) {
		return ResolvedEndpoint{}, NewError(CategoryNetwork, ReasonNetworkDenied)
	}
	addresses, err := lookup(ctx, resolver, host)
	if err != nil {
		return ResolvedEndpoint{}, NewError(CategoryNetwork, ReasonNetworkDenied, err)
	}
	if err := requirePublic(addresses); err != nil {
		return ResolvedEndpoint{}, err
	}
	copied := make([]net.IP, len(addresses))
	for i, address := range addresses {
		copied[i] = append(net.IP(nil), normalizeIP(address)...)
	}
	return ResolvedEndpoint{URL: parsed.String(), Host: host, Addresses: copied}, nil
}

// EvaluateRedirects checks the initial connect and every subsequent hop with
// a fresh lookup. More than MaxRedirectHops follow-ups are denied.
func EvaluateRedirects(ctx context.Context, resolver NameResolver, initial string, redirects []string) ([]ResolvedEndpoint, error) {
	if len(redirects) > MaxRedirectHops {
		return nil, NewError(CategoryNetwork, ReasonNetworkDenied)
	}
	chain := make([]ResolvedEndpoint, 0, 1+len(redirects))
	current, err := EvaluateEndpoint(ctx, resolver, initial)
	if err != nil {
		return nil, err
	}
	chain = append(chain, current)
	for _, next := range redirects {
		hop, err := EvaluateEndpoint(ctx, resolver, next)
		if err != nil {
			return nil, err
		}
		chain = append(chain, hop)
	}
	return chain, nil
}

func parseBrokerURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "https" || parsed.Fragment != "" {
		return nil, errDenied
	}
	host := parsed.Hostname()
	if host == "" || strings.TrimSpace(host) != host {
		return nil, errDenied
	}
	return parsed, nil
}

var errDenied = errors.New("denied")

func lookup(ctx context.Context, resolver NameResolver, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return resolver.LookupIP(ctx, host)
}

func requirePublic(addresses []net.IP) error {
	if len(addresses) == 0 {
		return NewError(CategoryNetwork, ReasonNetworkDenied)
	}
	for _, address := range addresses {
		if !ClassifyIP(address).Allowed() {
			return NewError(CategoryNetwork, ReasonNetworkDenied)
		}
	}
	return nil
}

func deniedName(host string) bool {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" || name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return true
	}
	return name == "metadata" || name == "metadata.google.internal" || strings.HasSuffix(name, ".metadata.google.internal")
}

func normalizeIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func isMetadataIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return ip.Equal(net.ParseIP("fd00:ec2::254"))
	}
	return v4.Equal(net.IPv4(169, 254, 169, 254)) || v4.Equal(net.IPv4(100, 100, 100, 200))
}
