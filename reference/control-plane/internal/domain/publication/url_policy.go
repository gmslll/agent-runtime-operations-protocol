package publication

import (
	"net"
	"strings"
)

var carrierGradeNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// ClassifyAllowedHost performs syntax-only classification. It deliberately
// does not resolve DNS, open sockets, follow redirects, or consult ambient
// resolver configuration; those connection-time checks belong to later
// phases.
func ClassifyAllowedHost(declared string) AllowedHost {
	host := AllowedHost{Declared: declared, Canonical: strings.ToLower(declared), Class: HostInvalid}
	if declared == "" || strings.TrimSpace(declared) != declared || strings.ContainsAny(declared, "/:@?#[]\\") {
		return host
	}
	if strings.HasPrefix(host.Canonical, "*.") {
		suffix := strings.TrimPrefix(host.Canonical, "*.")
		if strings.Count(host.Canonical, "*") == 1 && validPublicHostname(suffix) && !ambiguousIPName(suffix) && net.ParseIP(suffix) == nil {
			host.Class = HostWildcardName
		}
		return host
	}
	if ip := net.ParseIP(declared); ip != nil {
		host.Canonical = ip.String()
		switch {
		case isMetadataIP(ip):
			host.Class = HostMetadata
		case ip.IsLoopback():
			host.Class = HostLoopback
		case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
			host.Class = HostLinkLocal
		case deniedIPLiteral(ip):
			host.Class = HostPrivate
		case ip.To4() != nil:
			host.Class = HostGlobalIPLiteral
		}
		return host
	}
	if isMetadataName(host.Canonical) {
		host.Class = HostMetadata
		return host
	}
	if host.Canonical == "localhost" || strings.HasSuffix(host.Canonical, ".localhost") {
		host.Class = HostLoopback
		return host
	}
	if validPublicHostname(host.Canonical) && !ambiguousIPName(host.Canonical) {
		host.Class = HostPublicName
	}
	return host
}

func deniedIPLiteral(ip net.IP) bool {
	return ip == nil || ip.IsPrivate() || carrierGradeNAT.Contains(ip) || !ip.IsGlobalUnicast()
}

func ambiguousIPName(value string) bool {
	labels := strings.Split(value, ".")
	allNumeric := true
	for _, label := range labels {
		if strings.HasPrefix(label, "0x") || strings.HasPrefix(label, "0X") {
			return true
		}
		for _, character := range label {
			if character < '0' || character > '9' {
				allNumeric = false
				break
			}
		}
	}
	return allNumeric
}

func classifyAllowedHosts(values []string) ([]AllowedHost, error) {
	result := make([]AllowedHost, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		host := ClassifyAllowedHost(value)
		if !host.Class.Allowed() {
			return nil, NewError(CategoryValidation, ReasonAllowedHostDenied)
		}
		if _, exists := seen[host.Canonical]; exists {
			return nil, NewError(CategoryValidation, ReasonAllowedHostDenied)
		}
		seen[host.Canonical] = struct{}{}
		result = append(result, host)
	}
	return result, nil
}

func isMetadataName(value string) bool {
	return value == "metadata" || value == "metadata.google.internal" || value == "metadata.azure.internal"
}

func isMetadataIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4.Equal(net.IPv4(169, 254, 169, 254)) || v4.Equal(net.IPv4(100, 100, 100, 200))
	}
	return false
}
