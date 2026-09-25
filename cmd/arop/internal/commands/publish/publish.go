package publish

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
)

const (
	bundleMediaType     = "application/vnd.arop.agent-version-bundle+zip"
	jsonMediaType       = "application/json"
	maxErrorBytes       = 1 << 20
	maxArchiveBytes     = 10 << 20
	maxArchiveEntries   = 256
	maxEntryBytes       = 4 << 20
	maxTotalBytes       = 50 << 20
	maxCompressionRatio = 100
)

var (
	agentIDPattern         = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	locationPattern        = regexp.MustCompile(`^/v1/agent-definitions/[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*/versions/(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	etagPattern            = regexp.MustCompile(`^"sha256:[0-9a-f]{64}"$`)
	idempotencyKeyPattern  = regexp.MustCompile(`^[!-~]{8,200}$`)
	bearerChallengePattern = regexp.MustCompile(`^Bearer(?: [A-Za-z][A-Za-z0-9_-]*="[ -!#-~]*")*$`)
	retryAfterPattern      = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
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

// Command publishes an immutable AgentVersion bundle through the P11 public
// HTTP contract. It deliberately has no storage or reference-control-plane
// dependency.
type Command struct {
	BaseURL    *url.URL
	Client     *http.Client
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
	if err := contextError(ctx); err != nil {
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

	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	bundle, identity, err := loadAndValidateBundle(ctx, options.BundlePath)
	if err != nil {
		return Result{}, err
	}
	if identity.agentID != options.AgentID {
		return Result{}, errors.New("publication bundle identity does not match requested agent")
	}
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}

	endpoint := *command.BaseURL
	basePath := strings.TrimSuffix(endpoint.Path, "/")
	endpoint.RawPath = ""
	endpoint.Path = basePath + "/v1/agent-definitions/" + options.AgentID + "/versions"
	endpoint.RawQuery = ""
	endpoint.Fragment = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(bundle))
	if err != nil {
		return Result{}, errors.New("create publication request: invalid endpoint")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", bundleMediaType)
	request.Header.Set("Accept", jsonMediaType)
	request.Header.Set("Idempotency-Key", options.IdempotencyKey)

	response, err := doWithoutRedirects(command.Client, request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctxErr := contextError(ctx); ctxErr != nil {
			return Result{}, ctxErr
		}
		return Result{}, errors.New("publication transport unavailable")
	}
	if response == nil || response.Body == nil {
		return Result{}, errors.New("invalid publication response")
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusCreated {
		return command.decodeSuccess(response, identity)
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
		return errors.New("write publication result: unavailable")
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

func (command Command) decodeSuccess(response *http.Response, identity bundleIdentity) (Result, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, 1))
	if err != nil {
		return Result{}, errors.New("invalid publication response: unreadable body")
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
	expectedLocation := "/v1/agent-definitions/" + identity.agentID + "/versions/" + identity.version
	if location != expectedLocation {
		return Result{}, errors.New("invalid publication response: Location does not match validated bundle")
	}
	if !etagPattern.MatchString(etag) {
		return Result{}, errors.New("invalid publication response: malformed strong ETag")
	}
	if etag != `"`+identity.digest+`"` {
		return Result{}, errors.New("invalid publication response: ETag does not match validated manifest")
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
	if len(response.Header.Values("Content-Type")) != 1 || response.Header.Get("Content-Type") != jsonMediaType {
		return errors.New("invalid publication error response: Content-Type must be application/json")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
	if err != nil {
		return errors.New("invalid publication error response: unreadable body")
	}
	if len(body) > maxErrorBytes {
		return errors.New("invalid publication error response: body too large")
	}
	wire, err := controlplane.DecodeAROPError(body)
	if err != nil {
		return errors.New("invalid publication error response: malformed typed error")
	}
	if err := validateStatusError(response.StatusCode, wire.Code, wire.Category, wire.Retryable); err != nil {
		return err
	}
	challenges := response.Header.Values("WWW-Authenticate")
	if response.StatusCode == http.StatusUnauthorized {
		if len(challenges) != 1 || !bearerChallengePattern.MatchString(challenges[0]) {
			return errors.New("invalid publication error response: malformed WWW-Authenticate")
		}
	} else if len(challenges) != 0 {
		return errors.New("invalid publication error response: unexpected WWW-Authenticate")
	}

	var retryAfter *int
	retryHeaders := response.Header.Values("Retry-After")
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		if len(retryHeaders) != 1 {
			return errors.New("invalid publication error response: Retry-After must occur exactly once")
		}
		value, err := strconv.Atoi(retryHeaders[0])
		if err != nil || !retryAfterPattern.MatchString(retryHeaders[0]) || value < 1 || value > 86400 {
			return errors.New("invalid publication error response: Retry-After must be 1..86400 seconds")
		}
		if wire.RetryAfterSeconds == nil || int(*wire.RetryAfterSeconds) != value {
			return errors.New("invalid publication error response: Retry-After disagrees with typed error")
		}
		retryAfter = &value
	} else if len(retryHeaders) != 0 || wire.RetryAfterSeconds != nil {
		return errors.New("invalid publication error response: unexpected Retry-After")
	}
	return &RemoteError{StatusCode: response.StatusCode, Wire: wire, RetryAfter: retryAfter}
}

type bundleIdentity struct {
	agentID string
	version string
	digest  string
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); errors.Is(err, context.Canceled) {
		return context.Canceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func doWithoutRedirects(client *http.Client, request *http.Request) (*http.Response, error) {
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(request)
}

func loadAndValidateBundle(ctx context.Context, path string) ([]byte, bundleIdentity, error) {
	if err := contextError(ctx); err != nil {
		return nil, bundleIdentity{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maxArchiveBytes {
		return nil, bundleIdentity{}, errors.New("publication bundle is unavailable or invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, bundleIdentity{}, errors.New("publication bundle is unavailable or invalid")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, bundleIdentity{}, errors.New("publication bundle is unavailable or invalid")
	}
	data, err := readBounded(ctx, file, maxArchiveBytes)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, bundleIdentity{}, ctxErr
		}
		return nil, bundleIdentity{}, errors.New("publication bundle is unavailable or invalid")
	}
	identity, err := validateBundleArchive(ctx, data)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, bundleIdentity{}, ctxErr
		}
		return nil, bundleIdentity{}, errors.New("publication bundle is invalid")
	}
	return data, identity, nil
}

func readBounded(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	var output bytes.Buffer
	buffer := make([]byte, 64<<10)
	for {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			if int64(output.Len()+count) > limit {
				return nil, errors.New("size limit")
			}
			_, _ = output.Write(buffer[:count])
		}
		if err == io.EOF {
			return output.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func validateBundleArchive(ctx context.Context, archive []byte) (bundleIdentity, error) {
	if err := verifyCentralDirectoryExact(archive); err != nil {
		return bundleIdentity{}, errors.New("zip directory")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxArchiveEntries {
		return bundleIdentity{}, errors.New("zip inventory")
	}
	temporaryBase, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return bundleIdentity{}, errors.New("temporary package")
	}
	root, err := os.MkdirTemp(temporaryBase, "arop-publish-bundle-")
	if err != nil {
		return bundleIdentity{}, errors.New("temporary package")
	}
	defer os.RemoveAll(root)
	seen, folded := map[string]bool{}, map[string]bool{}
	var total uint64
	manifestCount := 0
	for _, entry := range reader.File {
		if err := contextError(ctx); err != nil {
			return bundleIdentity{}, err
		}
		name := entry.Name
		lower := strings.ToLower(name)
		if seen[name] || folded[lower] || !portableArchivePath(name) || entry.Flags&1 != 0 || bytes.Contains(entry.Extra, []byte("HARDLINK\x00")) || entry.Mode()&os.ModeType != 0 || entry.Method != zip.Store && entry.Method != zip.Deflate {
			return bundleIdentity{}, errors.New("zip entry")
		}
		seen[name], folded[lower] = true, true
		if name == "agent-manifest.json" {
			manifestCount++
		}
		stream, err := entry.Open()
		if err != nil {
			return bundleIdentity{}, errors.New("zip entry")
		}
		payload, readErr := readBounded(ctx, stream, maxEntryBytes)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || uint64(len(payload)) != entry.UncompressedSize64 {
			return bundleIdentity{}, errors.New("zip entry")
		}
		total += uint64(len(payload))
		highRatio := entry.CompressedSize64 == 0 && len(payload) > 0 || entry.CompressedSize64 > 0 && entry.CompressedSize64 <= ^uint64(0)/maxCompressionRatio && uint64(len(payload)) > entry.CompressedSize64*maxCompressionRatio
		if total > maxTotalBytes || highRatio {
			return bundleIdentity{}, errors.New("zip limits")
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil || os.WriteFile(target, payload, 0o600) != nil {
			return bundleIdentity{}, errors.New("temporary package")
		}
	}
	if manifestCount != 1 {
		return bundleIdentity{}, errors.New("root manifest")
	}
	if err := manifest.ValidatePackageFile(root, "agent-manifest.json"); err != nil {
		return bundleIdentity{}, errors.New("manifest validation")
	}
	if err := contextError(ctx); err != nil {
		return bundleIdentity{}, err
	}
	digest, err := manifest.DigestPackageFile(root, "agent-manifest.json")
	if err != nil {
		return bundleIdentity{}, errors.New("manifest digest")
	}
	if err := contextError(ctx); err != nil {
		return bundleIdentity{}, err
	}
	document, err := os.ReadFile(filepath.Join(root, "agent-manifest.json"))
	if err != nil {
		return bundleIdentity{}, errors.New("manifest read")
	}
	decoded, err := controlplane.DecodeAgentManifest(document)
	if err != nil {
		return bundleIdentity{}, errors.New("manifest decode")
	}
	return bundleIdentity{agentID: string(decoded.IDentity.ID), version: string(decoded.IDentity.Version), digest: digest}, nil
}

type zipInterval struct{ start, end int }

func verifyCentralDirectoryExact(archive []byte) error {
	eocd := -1
	start := len(archive) - 22 - 65535
	if start < 0 {
		start = 0
	}
	for index := len(archive) - 22; index >= start; index-- {
		if index >= 0 && index+4 <= len(archive) && binary.LittleEndian.Uint32(archive[index:index+4]) == 0x06054b50 {
			eocd = index
			break
		}
	}
	if eocd < 0 || eocd+22 > len(archive) {
		return errors.New("end record")
	}
	commentLength := int(binary.LittleEndian.Uint16(archive[eocd+20 : eocd+22]))
	if eocd+22+commentLength != len(archive) || binary.LittleEndian.Uint16(archive[eocd+4:eocd+6]) != 0 || binary.LittleEndian.Uint16(archive[eocd+6:eocd+8]) != 0 {
		return errors.New("multi-disk or trailing")
	}
	countDisk := int(binary.LittleEndian.Uint16(archive[eocd+8 : eocd+10]))
	countTotal := int(binary.LittleEndian.Uint16(archive[eocd+10 : eocd+12]))
	centralSize := int(binary.LittleEndian.Uint32(archive[eocd+12 : eocd+16]))
	centralOffset := int(binary.LittleEndian.Uint32(archive[eocd+16 : eocd+20]))
	if countTotal == 0 || countTotal == 0xffff || countDisk != countTotal || centralOffset < 0 || centralSize < 0 || centralOffset+centralSize != eocd {
		return errors.New("central bounds")
	}
	position := centralOffset
	intervals := make([]zipInterval, 0, countTotal)
	offsets := map[int]bool{}
	for range countTotal {
		if position+46 > eocd || binary.LittleEndian.Uint32(archive[position:position+4]) != 0x02014b50 {
			return errors.New("central entry")
		}
		flags := binary.LittleEndian.Uint16(archive[position+8 : position+10])
		method := binary.LittleEndian.Uint16(archive[position+10 : position+12])
		crc := binary.LittleEndian.Uint32(archive[position+16 : position+20])
		compressed := binary.LittleEndian.Uint32(archive[position+20 : position+24])
		uncompressed := binary.LittleEndian.Uint32(archive[position+24 : position+28])
		nameLength := int(binary.LittleEndian.Uint16(archive[position+28 : position+30]))
		extraLength := int(binary.LittleEndian.Uint16(archive[position+30 : position+32]))
		commentLength := int(binary.LittleEndian.Uint16(archive[position+32 : position+34]))
		localOffset := int(binary.LittleEndian.Uint32(archive[position+42 : position+46]))
		centralEnd := position + 46 + nameLength + extraLength + commentLength
		if compressed == 0xffffffff || uncompressed == 0xffffffff || centralEnd > eocd || localOffset < 0 || localOffset+30 > centralOffset || offsets[localOffset] || binary.LittleEndian.Uint32(archive[localOffset:localOffset+4]) != 0x04034b50 {
			return errors.New("entry bounds")
		}
		offsets[localOffset] = true
		localFlags := binary.LittleEndian.Uint16(archive[localOffset+6 : localOffset+8])
		localMethod := binary.LittleEndian.Uint16(archive[localOffset+8 : localOffset+10])
		localNameLength := int(binary.LittleEndian.Uint16(archive[localOffset+26 : localOffset+28]))
		localExtraLength := int(binary.LittleEndian.Uint16(archive[localOffset+28 : localOffset+30]))
		localEnd := localOffset + 30 + localNameLength + localExtraLength
		if localEnd > centralOffset || flags != localFlags || method != localMethod || nameLength != localNameLength || !bytes.Equal(archive[position+46:position+46+nameLength], archive[localOffset+30:localOffset+30+localNameLength]) {
			return errors.New("metadata mismatch")
		}
		dataEnd64 := int64(localEnd) + int64(compressed)
		if dataEnd64 < int64(localEnd) || dataEnd64 > int64(centralOffset) {
			return errors.New("data bounds")
		}
		entryEnd := int(dataEnd64)
		if flags&8 == 0 {
			if crc != binary.LittleEndian.Uint32(archive[localOffset+14:localOffset+18]) || compressed != binary.LittleEndian.Uint32(archive[localOffset+18:localOffset+22]) || uncompressed != binary.LittleEndian.Uint32(archive[localOffset+22:localOffset+26]) {
				return errors.New("digest size mismatch")
			}
		} else {
			if binary.LittleEndian.Uint32(archive[localOffset+14:localOffset+18]) != 0 || binary.LittleEndian.Uint32(archive[localOffset+18:localOffset+22]) != 0 || binary.LittleEndian.Uint32(archive[localOffset+22:localOffset+26]) != 0 {
				return errors.New("descriptor local values")
			}
			descriptor := entryEnd
			descriptorEnd := -1
			if descriptor+12 <= centralOffset && crc == binary.LittleEndian.Uint32(archive[descriptor:descriptor+4]) && compressed == binary.LittleEndian.Uint32(archive[descriptor+4:descriptor+8]) && uncompressed == binary.LittleEndian.Uint32(archive[descriptor+8:descriptor+12]) {
				descriptorEnd = descriptor + 12
			} else if descriptor+16 <= centralOffset && binary.LittleEndian.Uint32(archive[descriptor:descriptor+4]) == 0x08074b50 && crc == binary.LittleEndian.Uint32(archive[descriptor+4:descriptor+8]) && compressed == binary.LittleEndian.Uint32(archive[descriptor+8:descriptor+12]) && uncompressed == binary.LittleEndian.Uint32(archive[descriptor+12:descriptor+16]) {
				descriptorEnd = descriptor + 16
			}
			if descriptorEnd < 0 {
				return errors.New("descriptor mismatch")
			}
			entryEnd = descriptorEnd
		}
		intervals = append(intervals, zipInterval{start: localOffset, end: entryEnd})
		position = centralEnd
	}
	if position != eocd {
		return errors.New("central trailing")
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	cursor := 0
	for _, interval := range intervals {
		if interval.start != cursor || interval.end <= interval.start || interval.end > centralOffset {
			return errors.New("overlap or gap")
		}
		cursor = interval.end
	}
	if cursor != centralOffset {
		return errors.New("local trailing")
	}
	return nil
}

func portableArchivePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || filepath.IsAbs(name) || strings.Contains(name, "\\") || !utf8.ValidString(name) || len(name) != len([]rune(name)) {
		return false
	}
	decoded, err := url.PathUnescape(name)
	if err != nil || decoded != name {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for index := range len(segment) {
			char := segment[index]
			if !(char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || strings.ContainsRune("._-~", rune(char))) {
				return false
			}
		}
	}
	return true
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
