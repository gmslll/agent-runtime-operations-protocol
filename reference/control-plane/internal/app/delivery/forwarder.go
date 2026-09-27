package delivery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
)

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type AddressPolicy interface{ Allow(netip.Addr) bool }

type PublicAddresses struct{}

func (PublicAddresses) Allow(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() {
		return false
	}
	if address.Is4() && address == netip.MustParseAddr("169.254.169.254") {
		return false
	}
	return !address.Is6() || address != netip.MustParseAddr("fd00:ec2::254")
}

type HTTPForwarder struct{ client *http.Client }

func NewHTTPForwarder(source *http.Client, resolver Resolver, policy AddressPolicy) (*HTTPForwarder, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if policy == nil {
		policy = PublicAddresses{}
	}
	client := &http.Client{}
	if source != nil {
		*client = *source
	}
	var transport *http.Transport
	if client.Transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else if existing, ok := client.Transport.(*http.Transport); ok {
		transport = existing.Clone()
	} else {
		return nil, errors.New("delivery transport must be *http.Transport")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("unsafe runtime endpoint")
		}
		addresses, err := resolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 || len(addresses) > 32 {
			return nil, errors.New("unsafe runtime endpoint")
		}
		for _, candidate := range addresses {
			parsed, ok := netip.AddrFromSlice(candidate.IP)
			if !ok || !policy.Allow(parsed.Unmap()) {
				return nil, errors.New("unsafe runtime endpoint")
			}
		}
		var last error
		for _, candidate := range addresses {
			if connection, connectErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port)); connectErr == nil {
				return connection, nil
			} else {
				last = connectErr
			}
		}
		return nil, last
	}
	client.Transport = transport
	client.Timeout = 0
	// Proxy capabilities must never be replayed by redirect processing. The
	// durable Attempt endpoint is the sole permitted delivery target.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPForwarder{client: client}, nil
}

func (forwarder *HTTPForwarder) Deliver(ctx context.Context, attempt dispatch.Attempt, body []byte, token string) ([]byte, error) {
	endpoint, err := url.Parse(attempt.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "/v1/runs" || endpoint.RawQuery != "" || endpoint.Fragment != "" || attempt.TransportProfile != "proxy" || len(token) < 96 {
		return nil, errors.New("invalid proxy delivery")
	}
	requestContext, cancel := context.WithDeadline(ctx, attempt.TicketExpiresAt)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build proxy delivery")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", attempt.AttemptID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := forwarder.client.Do(request)
	if err != nil {
		return nil, errors.New("proxy delivery failed")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(responseBody) > 8<<20 || response.StatusCode != http.StatusAccepted || !singleContentType(response.Header) {
		return nil, errors.New("invalid proxy delivery response")
	}
	return responseBody, nil
}

func singleContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "application/json")
}

var _ Forwarder = (*HTTPForwarder)(nil)
