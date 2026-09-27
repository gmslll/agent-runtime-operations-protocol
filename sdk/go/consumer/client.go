// Package consumer implements the trusted Bot/Gateway AROP consumer core.
package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	dispatchwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/dispatch"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

const maxResponseBytes = 8 << 20

var (
	ErrTicketExpired  = errors.New("dispatch ticket expired")
	ErrUnsafeEndpoint = errors.New("unsafe delivery endpoint")
)

// TokenSource returns a short-lived Control Plane credential. The client never
// persists or logs the returned bearer value.
type TokenSource interface {
	Token(context.Context) (string, error)
}

// IdempotencySource creates a new dispatch key. A fresh key is mandatory after
// expiry; reusing the expired ticket/key is deliberately forbidden.
type IdempotencySource interface {
	NewKey(context.Context) (string, error)
}

// Resolver is resolved on every new connection.
type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// AddressPolicy approves every resolved address before connect.
type AddressPolicy interface{ Allow(netip.Addr) bool }

// Config defines a trusted consumer.
type Config struct {
	ControlPlaneURL string
	Tokens          TokenSource
	Idempotency     IdempotencySource
	Resolver        Resolver
	AddressPolicy   AddressPolicy
	HTTPClient      *http.Client
	Clock           func() time.Time
	DialTimeout     time.Duration
	RequestTimeout  time.Duration
}

// Client performs Control Plane dispatch and Direct/Proxy delivery.
type Client struct {
	base    *url.URL
	tokens  TokenSource
	keys    IdempotencySource
	http    *http.Client
	clock   func() time.Time
	timeout time.Duration
}

// New constructs a client with DNS/IP revalidation on every network connect.
func New(config Config) (*Client, error) {
	base, err := parseHTTPSBase(config.ControlPlaneURL)
	if err != nil || config.Tokens == nil || config.Idempotency == nil {
		return nil, errors.New("invalid consumer configuration")
	}
	if config.Resolver == nil {
		config.Resolver = net.DefaultResolver
	}
	if config.AddressPolicy == nil {
		config.AddressPolicy = PublicAddresses{}
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.DialTimeout < time.Second || config.DialTimeout > 30*time.Second || config.RequestTimeout < time.Second || config.RequestTimeout > 5*time.Minute {
		return nil, errors.New("invalid consumer timeout")
	}
	httpClient, err := secureHTTPClient(config.HTTPClient, config.Resolver, config.AddressPolicy, config.DialTimeout)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, tokens: config.Tokens, keys: config.Idempotency, http: httpClient, clock: config.Clock, timeout: config.RequestTimeout}, nil
}

// PublicAddresses rejects addresses that must not be reached through a runtime
// supplied endpoint, including loopback/private/link-local/metadata ranges.
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

// Dispatch obtains the single Control Plane-selected Attempt. The consumer
// never substitutes a registry candidate itself.
func (client *Client) Dispatch(ctx context.Context, runID, idempotencyKey string) (dispatchwire.DispatchTicket, error) {
	if !validRunID(runID) || !validKey(idempotencyKey) {
		return dispatchwire.DispatchTicket{}, errors.New("invalid dispatch request")
	}
	token, err := client.tokens.Token(ctx)
	if err != nil || !validBearer(token) {
		return dispatchwire.DispatchTicket{}, errors.New("control plane authentication unavailable")
	}
	target := *client.base
	target.Path = "/v1/agent-runs/" + runID + ":dispatch"
	requestContext, cancel := client.requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), http.NoBody)
	if err != nil {
		return dispatchwire.DispatchTicket{}, errors.New("build dispatch request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := client.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrUnsafeEndpoint) {
			return dispatchwire.DispatchTicket{}, ErrUnsafeEndpoint
		}
		return dispatchwire.DispatchTicket{}, errors.New("dispatch request failed")
	}
	defer response.Body.Close()
	body, err := bounded(response.Body)
	if err != nil {
		return dispatchwire.DispatchTicket{}, err
	}
	if response.StatusCode != http.StatusCreated || !singleContentType(response.Header, "application/json") || response.Header.Get("Cache-Control") != "no-store" {
		return dispatchwire.DispatchTicket{}, decodeRemoteError(response.StatusCode, body)
	}
	ticket, err := dispatchwire.DecodeDispatchTicket(body)
	if err != nil || string(ticket.RunID) != runID {
		return dispatchwire.DispatchTicket{}, errors.New("invalid dispatch response")
	}
	return ticket, nil
}

// Invoke dispatches and delivers. If a ticket expires before/during delivery,
// it obtains one fresh Attempt once using a new idempotency key.
func (client *Client) Invoke(ctx context.Context, runID string, request runwire.RunRequest) (runwire.RunStatus, error) {
	for attempt := 0; attempt < 2; attempt++ {
		key, err := client.keys.NewKey(ctx)
		if err != nil || !validKey(key) {
			return runwire.RunStatus{}, errors.New("dispatch idempotency unavailable")
		}
		ticket, err := client.Dispatch(ctx, runID, key)
		if err != nil {
			return runwire.RunStatus{}, err
		}
		status, err := client.Deliver(ctx, ticket, request)
		if !errors.Is(err, ErrTicketExpired) {
			return status, err
		}
	}
	return runwire.RunStatus{}, ErrTicketExpired
}

// Deliver sends the immutable RunRequest using the ticket-selected mode.
func (client *Client) Deliver(ctx context.Context, ticket dispatchwire.DispatchTicket, request runwire.RunRequest) (runwire.RunStatus, error) {
	expires, err := time.Parse(time.RFC3339Nano, string(ticket.Delivery.ExpiresAt))
	if err != nil || !client.clock().UTC().Before(expires) {
		return runwire.RunStatus{}, ErrTicketExpired
	}
	body, err := runwire.EncodeRunRequest(request)
	if err != nil || string(ticket.RunID) == "" || string(ticket.AttemptID) == "" || !validBearer(ticket.RunToken) {
		return runwire.RunStatus{}, errors.New("invalid delivery request")
	}
	var target string
	var bearerToken string
	switch ticket.Delivery.Mode {
	case "direct":
		target = string(ticket.Delivery.Endpoint)
		bearerToken = ticket.RunToken
	case "proxy":
		proxy := *client.base
		proxy.Path = "/v1/agent-runs/" + string(ticket.RunID) + ":deliver"
		target = proxy.String()
		bearerToken, err = client.tokens.Token(ctx)
		if err != nil || !validBearer(bearerToken) {
			return runwire.RunStatus{}, errors.New("control plane authentication unavailable")
		}
	default:
		return runwire.RunStatus{}, errors.New("unsupported delivery mode")
	}
	if _, err = parseHTTPSURL(target); err != nil {
		return runwire.RunStatus{}, ErrUnsafeEndpoint
	}
	requestContext, cancel := client.requestContext(ctx)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return runwire.RunStatus{}, errors.New("build delivery request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+bearerToken)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Idempotency-Key", string(ticket.AttemptID))
	if ticket.Delivery.Mode == "proxy" {
		httpRequest.Header.Set("X-AROP-Run-Token", ticket.RunToken)
		httpRequest.Header.Set("X-AROP-Runtime-Endpoint", string(ticket.Delivery.Endpoint))
	}
	response, err := client.http.Do(httpRequest)
	if err != nil {
		if errors.Is(err, ErrUnsafeEndpoint) {
			return runwire.RunStatus{}, ErrUnsafeEndpoint
		}
		return runwire.RunStatus{}, errors.New("delivery request failed")
	}
	defer response.Body.Close()
	responseBody, err := bounded(response.Body)
	if err != nil {
		return runwire.RunStatus{}, err
	}
	if response.StatusCode == http.StatusUnauthorized && remoteCode(responseBody) == "TICKET_EXPIRED" {
		return runwire.RunStatus{}, ErrTicketExpired
	}
	if response.StatusCode != http.StatusAccepted || !singleContentType(response.Header, "application/json") {
		return runwire.RunStatus{}, decodeRemoteError(response.StatusCode, responseBody)
	}
	status, err := runwire.DecodeRunStatus(responseBody)
	if err != nil || string(status.RunID) != string(ticket.RunID) {
		return runwire.RunStatus{}, errors.New("invalid delivery response")
	}
	return status, nil
}

// GetStatus queries the selected runtime without selecting another instance.
func (client *Client) GetStatus(ctx context.Context, ticket dispatchwire.DispatchTicket) (runwire.RunStatus, error) {
	base := strings.TrimSuffix(string(ticket.Delivery.Endpoint), "/v1/runs")
	return client.runtimeCall(ctx, ticket, http.MethodGet, base+"/v1/runs/"+string(ticket.RunID), nil)
}

// Command sends one command to the selected runtime.
func (client *Client) Command(ctx context.Context, ticket dispatchwire.DispatchTicket, command runwire.RunCommand) (runwire.RunStatus, error) {
	body, err := runwire.EncodeRunCommand(command)
	if err != nil {
		return runwire.RunStatus{}, err
	}
	base := strings.TrimSuffix(string(ticket.Delivery.Endpoint), "/v1/runs")
	return client.runtimeCall(ctx, ticket, http.MethodPost, base+"/v1/runs/"+string(ticket.RunID)+"/commands", body)
}

func (client *Client) runtimeCall(ctx context.Context, ticket dispatchwire.DispatchTicket, method, target string, body []byte) (runwire.RunStatus, error) {
	if _, err := parseHTTPSURL(target); err != nil || !validBearer(ticket.RunToken) {
		return runwire.RunStatus{}, ErrUnsafeEndpoint
	}
	requestContext, cancel := client.requestContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, target, bytes.NewReader(body))
	if err != nil {
		return runwire.RunStatus{}, errors.New("build runtime request")
	}
	request.Header.Set("Authorization", "Bearer "+ticket.RunToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrUnsafeEndpoint) {
			return runwire.RunStatus{}, ErrUnsafeEndpoint
		}
		return runwire.RunStatus{}, errors.New("runtime request failed")
	}
	defer response.Body.Close()
	responseBody, err := bounded(response.Body)
	if err != nil {
		return runwire.RunStatus{}, err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return runwire.RunStatus{}, decodeRemoteError(response.StatusCode, responseBody)
	}
	status, err := runwire.DecodeRunStatus(responseBody)
	if err != nil || string(status.RunID) != string(ticket.RunID) {
		return runwire.RunStatus{}, errors.New("invalid runtime response")
	}
	return status, nil
}

func secureHTTPClient(source *http.Client, resolver Resolver, policy AddressPolicy, timeout time.Duration) (*http.Client, error) {
	client := &http.Client{}
	if source != nil {
		*client = *source
	}
	var transport *http.Transport
	if client.Transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		original, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil, errors.New("consumer transport must be *http.Transport")
		}
		transport = original.Clone()
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrUnsafeEndpoint
		}
		addresses, err := resolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 || len(addresses) > 32 {
			return nil, ErrUnsafeEndpoint
		}
		for _, candidate := range addresses {
			parsed, ok := netip.AddrFromSlice(candidate.IP)
			if !ok || !policy.Allow(parsed.Unmap()) {
				return nil, ErrUnsafeEndpoint
			}
		}
		var last error
		for _, candidate := range addresses {
			connection, connectErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if connectErr == nil {
				return connection, nil
			}
			last = connectErr
		}
		return nil, last
	}
	client.Transport = transport
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 || request.URL.Scheme != "https" || len(via) == 0 || !sameOrigin(request.URL, via[0].URL) || request.URL.User != nil {
			return http.ErrUseLastResponse
		}
		return nil
	}
	client.Timeout = 0 // request contexts are the sole timeout authority.
	return client, nil
}

func (client *Client) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= client.timeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, client.timeout)
}

func parseHTTPSBase(value string) (*url.URL, error) {
	parsed, err := parseHTTPSURL(value)
	if err != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid HTTPS base URL")
	}
	parsed.Path = ""
	return parsed, nil
}

func parseHTTPSURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrUnsafeEndpoint
	}
	return parsed, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func bounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, errors.New("remote response exceeds limit")
	}
	return data, nil
}

type remoteError struct {
	Category, Code, Message string
	Retryable               bool
	RetryAfterSeconds       *int64 `json:"retry_after_seconds"`
}

func decodeRemoteError(status int, body []byte) error {
	var value remoteError
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.Code == "" || value.Message == "" {
		return fmt.Errorf("remote request failed with status %d", status)
	}
	return fmt.Errorf("remote %s (%d)", value.Code, status)
}

func remoteCode(body []byte) string {
	var value struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &value)
	return value.Code
}

func singleContentType(header http.Header, expected string) bool {
	values := header.Values("Content-Type")
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), expected)
}

func validRunID(value string) bool {
	return strings.HasPrefix(value, "run_") && len(value) == 40
}

func validKey(value string) bool {
	return len(value) >= 8 && len(value) <= 200 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validBearer(value string) bool {
	return len(value) >= 16 && len(value) <= 8192 && !strings.ContainsAny(value, " \t\r\n")
}

// ParseRetryAfter validates integer Retry-After headers for callers that need
// explicit retry scheduling.
func ParseRetryAfter(header http.Header) (time.Duration, error) {
	values := header.Values("Retry-After")
	if len(values) != 1 {
		return 0, errors.New("invalid Retry-After header")
	}
	seconds, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || seconds < 1 || seconds > 86400 {
		return 0, errors.New("invalid Retry-After header")
	}
	return time.Duration(seconds) * time.Second, nil
}
