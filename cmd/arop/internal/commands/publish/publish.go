package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
)

const (
	bundleMediaType = "application/vnd.arop.agent-version-bundle+zip"
	jsonMediaType   = "application/json"
	maxErrorBytes   = 1 << 20
)

var (
	agentIDPattern        = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	locationPattern       = regexp.MustCompile(`^/v1/agent-definitions/[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*/versions/(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	etagPattern           = regexp.MustCompile(`^"sha256:[0-9a-f]{64}"$`)
	idempotencyKeyPattern = regexp.MustCompile(`^[!-~]{8,200}$`)
)

// CredentialSource supplies a Control Plane bearer credential at request time.
// Implementations must not log, persist, or otherwise expose the returned value.
type CredentialSource interface {
	Credential(context.Context) (string, error)
}

// CredentialSourceFunc adapts a function into a CredentialSource.
type CredentialSourceFunc func(context.Context) (string, error)

func (source CredentialSourceFunc) Credential(ctx context.Context) (string, error) {
	return source(ctx)
}

// HTTPDoer is the public net/http seam used by the publish command.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Command publishes an immutable AgentVersion bundle through the P11 public
// HTTP contract. It deliberately has no storage or reference-control-plane
// dependency.
type Command struct {
	BaseURL    *url.URL
	Client     HTTPDoer
	Credential CredentialSource
	Stdout     io.Writer
}

type Options struct {
	AgentID        string
	BundlePath     string
	IdempotencyKey string
}

type Result struct {
	Location string
	ETag     string
}

// RemoteError is a strict typed P11 error response. Error intentionally omits
// the server message and details so potentially sensitive values cannot reach
// CLI stderr through the returned error string.
type RemoteError struct {
	StatusCode int
	Wire       controlplane.AROPError
	RetryAfter *int
}

func (failure *RemoteError) Error() string {
	return fmt.Sprintf("publication failed: status=%d code=%s", failure.StatusCode, failure.Wire.Code)
}

func (command Command) Run(ctx context.Context, options Options) (Result, error) {
	if err := command.validate(options); err != nil {
		return Result{}, err
	}

	credential, err := command.Credential.Credential(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return Result{}, fmt.Errorf("load publication credential: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("load publication credential: %w", context.DeadlineExceeded)
		}
		return Result{}, errors.New("load publication credential: unavailable")
	}
	if credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return Result{}, errors.New("load publication credential: invalid bearer credential")
	}

	bundle, err := os.Open(options.BundlePath)
	if err != nil {
		return Result{}, fmt.Errorf("open publication bundle: %w", err)
	}
	defer bundle.Close()

	endpoint := *command.BaseURL
	basePath := strings.TrimSuffix(endpoint.Path, "/")
	endpoint.RawPath = ""
	endpoint.Path = basePath + "/v1/agent-definitions/" + options.AgentID + "/versions"
	endpoint.RawQuery = ""
	endpoint.Fragment = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bundle)
	if err != nil {
		return Result{}, fmt.Errorf("create publication request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", bundleMediaType)
	request.Header.Set("Accept", jsonMediaType)
	request.Header.Set("Idempotency-Key", options.IdempotencyKey)

	response, err := command.Client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("send publication request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusCreated {
		return command.decodeSuccess(response, options.AgentID)
	}
	return Result{}, decodeRemoteError(response)
}

func (command Command) Execute(ctx context.Context, options Options) error {
	result, err := command.Run(ctx, options)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(command.Stdout, "published location=%s etag=%s\n", result.Location, result.ETag)
	if err != nil {
		return fmt.Errorf("write publication result: %w", err)
	}
	return nil
}

func (command Command) validate(options Options) error {
	if command.BaseURL == nil || command.BaseURL.Scheme == "" || command.BaseURL.Host == "" || command.BaseURL.User != nil || command.BaseURL.RawQuery != "" || command.BaseURL.Fragment != "" {
		return errors.New("publication base URL must be an absolute URL without credentials, query, or fragment")
	}
	if command.BaseURL.Scheme != "http" && command.BaseURL.Scheme != "https" {
		return errors.New("publication base URL must use http or https")
	}
	if command.Client == nil {
		return errors.New("publication HTTP client is required")
	}
	if command.Credential == nil {
		return errors.New("publication credential source is required")
	}
	if command.Stdout == nil {
		return errors.New("publication stdout is required")
	}
	if !agentIDPattern.MatchString(options.AgentID) {
		return errors.New("publication agent ID is invalid")
	}
	if options.BundlePath == "" {
		return errors.New("publication bundle path is required")
	}
	if !idempotencyKeyPattern.MatchString(options.IdempotencyKey) {
		return errors.New("publication idempotency key must contain 8..200 visible ASCII characters")
	}
	return nil
}

func (command Command) decodeSuccess(response *http.Response, agentID string) (Result, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, 1))
	if err != nil {
		return Result{}, fmt.Errorf("read publication response: %w", err)
	}
	if len(body) != 0 {
		return Result{}, errors.New("invalid publication response: 201 body must be empty")
	}
	if len(response.Header.Values("Location")) != 1 || len(response.Header.Values("ETag")) != 1 {
		return Result{}, errors.New("invalid publication response: Location and ETag must occur exactly once")
	}
	location := response.Header.Get("Location")
	etag := response.Header.Get("ETag")
	if !locationPattern.MatchString(location) {
		return Result{}, errors.New("invalid publication response: malformed relative Location")
	}
	if !strings.HasPrefix(location, "/v1/agent-definitions/"+agentID+"/versions/") {
		return Result{}, errors.New("invalid publication response: Location agent does not match request")
	}
	if !etagPattern.MatchString(etag) {
		return Result{}, errors.New("invalid publication response: malformed strong ETag")
	}
	return Result{Location: location, ETag: etag}, nil
}

func decodeRemoteError(response *http.Response) error {
	expected := map[int]bool{
		http.StatusBadRequest:            true,
		http.StatusUnauthorized:          true,
		http.StatusForbidden:             true,
		http.StatusConflict:              true,
		http.StatusRequestEntityTooLarge: true,
		http.StatusUnsupportedMediaType:  true,
		http.StatusTooManyRequests:       true,
		http.StatusServiceUnavailable:    true,
	}
	if !expected[response.StatusCode] {
		return fmt.Errorf("unexpected publication response status %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != jsonMediaType {
		return errors.New("invalid publication error response: Content-Type must be application/json")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
	if err != nil {
		return fmt.Errorf("read publication error response: %w", err)
	}
	if len(body) > maxErrorBytes {
		return errors.New("invalid publication error response: body too large")
	}
	wire, err := controlplane.DecodeAROPError(body)
	if err != nil {
		return fmt.Errorf("invalid publication error response: %w", err)
	}
	if err := validateStatusError(response.StatusCode, wire.Code, wire.Category, wire.Retryable); err != nil {
		return err
	}
	if response.StatusCode == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") == "" {
		return errors.New("invalid publication error response: missing WWW-Authenticate")
	}

	var retryAfter *int
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		value, err := strconv.Atoi(response.Header.Get("Retry-After"))
		if err != nil || value < 1 || value > 86400 {
			return errors.New("invalid publication error response: Retry-After must be 1..86400 seconds")
		}
		if wire.RetryAfterSeconds == nil || int(*wire.RetryAfterSeconds) != value {
			return errors.New("invalid publication error response: Retry-After disagrees with typed error")
		}
		retryAfter = &value
	}
	return &RemoteError{StatusCode: response.StatusCode, Wire: wire, RetryAfter: retryAfter}
}

func validateStatusError(status int, code, category string, retryable bool) error {
	type expectedError struct {
		code, category string
		retryable      bool
	}
	expected := map[int]expectedError{
		400: {"INVALID_PUBLICATION_REQUEST", "validation", false},
		401: {"AUTHENTICATION_REQUIRED", "authentication", false},
		403: {"PUBLICATION_FORBIDDEN", "authorization", false},
		409: {"AGENT_VERSION_CONFLICT", "conflict", false},
		413: {"BUNDLE_TOO_LARGE", "capacity", false},
		415: {"UNSUPPORTED_MEDIA_TYPE", "validation", false},
		429: {"RATE_LIMITED", "capacity", true},
		503: {"DEPENDENCY_UNAVAILABLE", "dependency", true},
	}[status]
	if expected.code != code || expected.category != category || expected.retryable != retryable {
		return errors.New("invalid publication error response: status and typed error disagree")
	}
	return nil
}

// RedactedError renders an error for CLI stderr without reflecting remote
// message/details or locally supplied credentials.
func RedactedError(err error) string {
	if err == nil {
		return ""
	}
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "publication canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "publication timed out"
	}
	return err.Error()
}
