// Package worker implements the public AROP v1 Worker Pull client and claim
// loop. The client treats delivery as at-least-once and never logs or persists
// bearer or lease tokens.
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

const maxWorkerResponseBytes = 8 << 20

var (
	workerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	claimIDPattern  = regexp.MustCompile(`^clm_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// CredentialSource returns a short-lived worker credential. Implementations
// must not return a credential containing whitespace or separators.
type CredentialSource interface {
	Credential(context.Context) (string, error)
}

type CredentialSourceFunc func(context.Context) (string, error)

func (source CredentialSourceFunc) Credential(ctx context.Context) (string, error) {
	return source(ctx)
}

// ClientConfig configures the transport-only Worker Pull client.
type ClientConfig struct {
	BaseURL        string
	HTTPClient     *http.Client
	Credential     CredentialSource
	RequestTimeout time.Duration
}

// Client performs the four Worker Pull operations. It uses exactly one
// RoundTrip per operation, so redirects can never forward credentials.
type Client struct {
	base       *url.URL
	http       *http.Client
	credential CredentialSource
	timeout    time.Duration
}

// RemoteError is a validated AROP error response. It contains no response
// message/details so remote systems cannot inject secrets into caller logs.
type RemoteError struct {
	StatusCode        int
	Category          string
	Code              string
	Retryable         bool
	RetryAfterSeconds *uint64
}

func (failure *RemoteError) Error() string {
	return fmt.Sprintf("worker request failed: status=%d code=%s", failure.StatusCode, failure.Code)
}

func NewClient(config ClientConfig) (*Client, error) {
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || (base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" || config.Credential == nil {
		return nil, errors.New("invalid worker client configuration")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 35 * time.Second
	}
	if config.RequestTimeout < time.Second || config.RequestTimeout > 5*time.Minute {
		return nil, errors.New("invalid worker request timeout")
	}
	base.Path = ""
	return &Client{base: base, http: config.HTTPClient, credential: config.Credential, timeout: config.RequestTimeout}, nil
}

// Claim returns (claim, true, nil) for 200 and (_, false, nil) for 204.
func (client *Client) Claim(ctx context.Context, workerID string, request workerwire.WorkerClaimRequest) (workerwire.WorkerClaim, bool, error) {
	if !validWorkerID(workerID) {
		return workerwire.WorkerClaim{}, false, errors.New("invalid worker id")
	}
	body, err := workerwire.EncodeWorkerClaimRequest(request)
	if err != nil {
		return workerwire.WorkerClaim{}, false, errors.New("invalid worker claim request")
	}
	response, err := client.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/claims:next", body, "")
	if err != nil {
		return workerwire.WorkerClaim{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		if !emptyResponse(response) {
			return workerwire.WorkerClaim{}, false, errors.New("invalid empty worker response")
		}
		return workerwire.WorkerClaim{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return workerwire.WorkerClaim{}, false, decodeRemoteError(response)
	}
	if !singleHeader(response.Header, "Cache-Control", "no-store") {
		return workerwire.WorkerClaim{}, false, errors.New("invalid worker cache response")
	}
	data, err := readJSON(response)
	if err != nil {
		return workerwire.WorkerClaim{}, false, err
	}
	claim, err := workerwire.DecodeWorkerClaim(data)
	if err != nil || string(claim.WorkerID) != workerID || string(claim.SessionID) != string(request.SessionID) || int64(claim.Generation) != int64(request.Generation) {
		return workerwire.WorkerClaim{}, false, errors.New("invalid worker claim response")
	}
	return claim, true, nil
}

func (client *Client) Renew(ctx context.Context, workerID, claimID string, request workerwire.WorkerRenewRequest) (workerwire.WorkerClaim, error) {
	if !validWorkerID(workerID) || !claimIDPattern.MatchString(claimID) {
		return workerwire.WorkerClaim{}, errors.New("invalid worker renew target")
	}
	body, err := workerwire.EncodeWorkerRenewRequest(request)
	if err != nil {
		return workerwire.WorkerClaim{}, errors.New("invalid worker renew request")
	}
	response, err := client.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/claims/"+claimID+":renew", body, "")
	if err != nil {
		return workerwire.WorkerClaim{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return workerwire.WorkerClaim{}, decodeRemoteError(response)
	}
	if !singleHeader(response.Header, "Cache-Control", "no-store") {
		return workerwire.WorkerClaim{}, errors.New("invalid worker cache response")
	}
	data, err := readJSON(response)
	if err != nil {
		return workerwire.WorkerClaim{}, err
	}
	claim, err := workerwire.DecodeWorkerClaim(data)
	if err != nil || string(claim.WorkerID) != workerID || string(claim.ClaimID) != claimID || string(claim.LeaseToken) != string(request.LeaseToken) || int64(claim.FencingToken) != int64(request.FencingToken) {
		return workerwire.WorkerClaim{}, errors.New("invalid worker renew response")
	}
	return claim, nil
}

func (client *Client) Complete(ctx context.Context, workerID, claimID, idempotencyKey string, request workerwire.WorkerComplete) error {
	if !validWorkerID(workerID) || !claimIDPattern.MatchString(claimID) || !validIdempotencyKey(idempotencyKey) || string(request.ClaimID) != claimID {
		return errors.New("invalid worker completion target")
	}
	body, err := workerwire.EncodeWorkerComplete(request)
	if err != nil {
		return errors.New("invalid worker completion request")
	}
	response, err := client.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/claims/"+claimID+":complete", body, idempotencyKey)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return decodeRemoteError(response)
	}
	if !emptyResponse(response) {
		return errors.New("invalid empty worker response")
	}
	return nil
}

func (client *Client) Release(ctx context.Context, workerID, claimID string, request workerwire.WorkerReleaseRequest) error {
	if !validWorkerID(workerID) || !claimIDPattern.MatchString(claimID) {
		return errors.New("invalid worker release target")
	}
	body, err := workerwire.EncodeWorkerReleaseRequest(request)
	if err != nil {
		return errors.New("invalid worker release request")
	}
	response, err := client.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/claims/"+claimID+":release", body, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return decodeRemoteError(response)
	}
	if !emptyResponse(response) {
		return errors.New("invalid empty worker response")
	}
	return nil
}

func (client *Client) do(ctx context.Context, method, path string, body []byte, idempotencyKey string) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	credential, err := client.credential.Credential(ctx)
	if err != nil || !validBearer(credential) {
		return nil, errors.New("worker credential unavailable")
	}
	target := *client.base
	target.Path = path
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create worker request")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	transport := client.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if requestContext.Err() != nil {
			return nil, requestContext.Err()
		}
		return nil, errors.New("worker transport unavailable")
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("invalid worker response")
	}
	return response, nil
}

func decodeRemoteError(response *http.Response) error {
	data, err := readJSON(response)
	if err != nil {
		return err
	}
	wire, err := controlplane.DecodeAROPError(data)
	if err != nil {
		return errors.New("invalid worker error response")
	}
	if response.StatusCode == http.StatusUnauthorized {
		values := response.Header.Values("WWW-Authenticate")
		if len(values) != 1 || (values[0] != "Bearer" && !strings.HasPrefix(values[0], "Bearer ")) {
			return errors.New("invalid worker authentication response")
		}
	}
	var retryAfter *uint64
	if wire.RetryAfterSeconds != nil {
		value := uint64(*wire.RetryAfterSeconds)
		retryAfter = &value
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		values := response.Header.Values("Retry-After")
		if len(values) != 1 || retryAfter == nil || values[0] != strconv.FormatUint(*retryAfter, 10) {
			return errors.New("invalid worker retry response")
		}
	}
	return &RemoteError{StatusCode: response.StatusCode, Category: wire.Category, Code: wire.Code, Retryable: wire.Retryable, RetryAfterSeconds: retryAfter}
}

func readJSON(response *http.Response) ([]byte, error) {
	if !singleHeader(response.Header, "Content-Type", "application/json") {
		return nil, errors.New("invalid worker response content type")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxWorkerResponseBytes+1))
	if err != nil || len(data) > maxWorkerResponseBytes {
		return nil, errors.New("invalid worker response body")
	}
	return data, nil
}

func emptyResponse(response *http.Response) bool {
	if response.ContentLength > 0 || len(response.Header.Values("Content-Type")) != 0 {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1))
	return err == nil && len(data) == 0
}

func singleHeader(header http.Header, name, value string) bool {
	values := header.Values(name)
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), value)
}

func validWorkerID(value string) bool {
	return len(value) <= 128 && workerIDPattern.MatchString(value)
}

func validIdempotencyKey(value string) bool {
	if len(value) < 16 || len(value) > 200 {
		return false
	}
	for _, character := range []byte(value) {
		if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || strings.ContainsRune("._~-", rune(character))) {
			return false
		}
	}
	return true
}

func validBearer(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n,")
}
