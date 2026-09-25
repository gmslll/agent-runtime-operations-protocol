package assets

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

const (
	testTenant = "acme"
	testPrn    = "prn_018f3b2a-7c31-7a11-8abc-1234567890ab"
	testCred   = "cred_018f3b2a-7c31-7a11-8abc-1234567890ab"
	testRun    = "run_018f3b2a-7c31-7a11-8abc-1234567890ab"
	testAsset  = "asset_018f3b2a-7c31-7a11-8abc-1234567890ab"
	testDigest = "sha256:a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"
)

func TestIssueGrantFailClosedWithoutAuthorizer(t *testing.T) {
	deps := testDeps(t, allowAll{})
	deps.Authorizer = nil
	if _, err := New(deps); err == nil {
		t.Fatal("expected missing authorizer to fail closed")
	}
}

func TestIssueGrantDeniedByAuthorizer(t *testing.T) {
	service := testService(t, denyAll{})
	_, err := service.IssueGrant(context.Background(), testIssue(OperationUpload))
	requireReason(t, err, ReasonGrantForbidden)
}

func TestGrantLifecycleQuarantineReadyAndNetwork(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	clock := &frozenClock{now: now}
	store := newMemStore()
	resolver := &scriptedResolver{answers: map[string][][]net.IP{
		"objects.example": {[]net.IP{net.ParseIP("203.0.113.10")}, []net.IP{net.ParseIP("10.1.2.3")}},
		"cdn.example":     {[]net.IP{net.ParseIP("203.0.113.20")}},
	}}
	deps := testDeps(t, allowAll{})
	deps.Clock, deps.Assets, deps.Grants, deps.Resolver = clock, store, store, resolver
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	request := testIssue(OperationUpload)
	request.NotBefore = now
	request.ExpiresAt = now.Add(5 * time.Minute)
	request.MaxUses = 3
	issued, err := service.IssueGrant(context.Background(), request)
	if err != nil || issued.Token == "" {
		t.Fatalf("issue: %v token empty=%v", err, issued.Token == "")
	}
	replay, err := service.IssueGrant(context.Background(), request)
	if err != nil || !replay.Replay || replay.Token != issued.Token || replay.Grant.GrantID != issued.Grant.GrantID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	conflict := request
	conflict.Digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	_, err = service.IssueGrant(context.Background(), conflict)
	requireReason(t, err, ReasonIdempotencyConflict)

	receipt := UploadReceipt{
		Metadata: testMetadata(),
		GrantID:  issued.Grant.GrantID, GrantToken: issued.Token, Content: []byte("hello world\n"),
	}
	ready, err := service.ReceiveUpload(context.Background(), receipt)
	if err != nil || ready.State != StateReady {
		t.Fatalf("ready: %+v %v", ready, err)
	}
	download := request
	download.Operation = OperationDownload
	download.IdempotencyKey = "download-key"
	download.MaxUses = 3
	_, err = service.Connect(context.Background(), ConnectRequest{Metadata: testMetadata(), Caller: request.Caller, GrantToken: issued.Token, URL: "https://objects.example/put"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Connect(context.Background(), ConnectRequest{Metadata: testMetadata(), Caller: request.Caller, GrantToken: issued.Token, URL: "https://objects.example/put"}, nil)
	requireReason(t, err, ReasonNetworkDenied)
	issuedDownload, err := service.IssueGrant(context.Background(), download)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := service.Connect(context.Background(), ConnectRequest{
		Metadata: testMetadata(), Caller: request.Caller, GrantToken: issuedDownload.Token, URL: "https://cdn.example/get",
	}, []string{"https://cdn.example/redirected"})
	if err != nil || len(chain) != 2 {
		t.Fatalf("download connect: %v hops=%d", err, len(chain))
	}
	_, err = service.Connect(context.Background(), ConnectRequest{
		Metadata: testMetadata(), Caller: request.Caller, GrantToken: issuedDownload.Token, URL: "https://cdn.example/get",
	}, []string{"http://cdn.example/insecure"})
	requireReason(t, err, ReasonNetworkDenied)
	downloaded, err := service.Download(context.Background(), DownloadReceipt{Metadata: testMetadata(), GrantID: issuedDownload.Grant.GrantID, GrantToken: issuedDownload.Token})
	if err != nil || string(downloaded.Content) != "hello world\n" || downloaded.MediaType != "application/pdf" {
		t.Fatalf("download: %+v %v", downloaded, err)
	}

	if err := service.RevokeGrant(context.Background(), RevokeRequest{Metadata: testMetadata(), Caller: request.Caller, GrantID: issuedDownload.Grant.GrantID}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Connect(context.Background(), ConnectRequest{Metadata: testMetadata(), Caller: request.Caller, GrantToken: issuedDownload.Token, URL: "https://cdn.example/get"}, nil)
	requireReason(t, err, ReasonGrantRevoked)
}

func TestGrantWindowAndUses(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	clock := &frozenClock{now: now}
	store := newMemStore()
	deps := testDeps(t, allowAll{})
	deps.Clock, deps.Assets, deps.Grants, deps.Resolver = clock, store, store, staticResolver{net.ParseIP("203.0.113.9")}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	request := testIssue(OperationUpload)
	request.NotBefore = now.Add(time.Minute)
	request.ExpiresAt = now.Add(2 * time.Minute)
	request.MaxUses = 1
	issued, err := service.IssueGrant(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ReceiveUpload(context.Background(), UploadReceipt{
		Metadata: testMetadata(), GrantID: issued.Grant.GrantID, GrantToken: issued.Token, Content: []byte("hello world\n"),
	})
	requireReason(t, err, ReasonGrantNotYetValid)
	clock.now = now.Add(3 * time.Minute)
	_, err = service.ReceiveUpload(context.Background(), UploadReceipt{
		Metadata: testMetadata(), GrantID: issued.Grant.GrantID, GrantToken: issued.Token, Content: []byte("hello world\n"),
	})
	requireReason(t, err, ReasonGrantExpired)

	clock.now = now.Add(90 * time.Second)
	if _, err := service.ReceiveUpload(context.Background(), UploadReceipt{
		Metadata: testMetadata(), GrantID: issued.Grant.GrantID, GrantToken: issued.Token, Content: []byte("hello world\n"),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Connect(context.Background(), ConnectRequest{Metadata: testMetadata(), Caller: request.Caller, GrantToken: issued.Token, URL: "https://objects.example/put"}, nil)
	requireReason(t, err, ReasonGrantUsesExhausted)

	long := request
	long.IdempotencyKey = "too-long-ttl"
	long.NotBefore = now
	long.ExpiresAt = now.Add(MaxGrantTTL + time.Second)
	_, err = service.IssueGrant(context.Background(), long)
	requireReason(t, err, ReasonInvalidRequest)
}

func TestIssueGrantNeverDowngradesReadyAssetAndReplayRequiresUsableGrant(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	clock := &frozenClock{now: now}
	store := newMemStore()
	deps := testDeps(t, allowAll{})
	deps.Clock, deps.Assets, deps.Grants = clock, store, store
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	request := testIssue(OperationUpload)
	request.NotBefore, request.ExpiresAt, request.MaxUses = now, now.Add(time.Minute), 1
	issued, err := service.IssueGrant(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ReceiveUpload(context.Background(), UploadReceipt{Metadata: testMetadata(), GrantID: issued.Grant.GrantID, GrantToken: issued.Token, Content: []byte("hello world\n")}); err != nil {
		t.Fatal(err)
	}
	reissue := request
	reissue.IdempotencyKey = "ready-reissue-key"
	if _, err = service.IssueGrant(context.Background(), reissue); err == nil {
		t.Fatal("ready asset was downgraded by a new upload grant")
	}
	ready, err := store.Get(context.Background(), request.Caller.TenantID, request.AssetID)
	if err != nil || ready.State != StateReady || string(ready.Content) != "hello world\n" {
		t.Fatalf("ready asset changed: %+v %v", ready, err)
	}
	if _, err = service.IssueGrant(context.Background(), request); err == nil {
		t.Fatal("exhausted idempotent grant was replayed")
	} else {
		requireReason(t, err, ReasonGrantUsesExhausted)
	}

	revocable := request
	revocable.AssetID = ""
	revocable.IdempotencyKey = "revoked-replay-key"
	revocable.MaxUses = 2
	revokedGrant, err := service.IssueGrant(context.Background(), revocable)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RevokeGrant(context.Background(), RevokeRequest{Metadata: testMetadata(), Caller: request.Caller, GrantID: revokedGrant.Grant.GrantID}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.IssueGrant(context.Background(), revocable); err == nil {
		t.Fatal("revoked idempotent grant was replayed")
	} else {
		requireReason(t, err, ReasonGrantRevoked)
	}

	expiring := request
	expiring.AssetID = ""
	expiring.IdempotencyKey = "expired-replay-key"
	expiredGrant, err := service.IssueGrant(context.Background(), expiring)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = expiredGrant.Grant.ExpiresAt
	if _, err = service.IssueGrant(context.Background(), expiring); err == nil {
		t.Fatal("expired idempotent grant was replayed")
	} else {
		requireReason(t, err, ReasonGrantExpired)
	}
}

func TestNetworkClassification(t *testing.T) {
	cases := []struct {
		ip      string
		allowed bool
	}{
		{"203.0.113.10", true},
		{"8.8.8.8", true},
		{"127.0.0.1", false},
		{"10.0.0.8", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"169.254.1.1", false},
		{"100.100.100.200", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"fd00:ec2::254", false},
	}
	for _, tc := range cases {
		class := ClassifyIP(net.ParseIP(tc.ip))
		if class.Allowed() != tc.allowed {
			t.Fatalf("%s class %s allowed=%v", tc.ip, class, class.Allowed())
		}
	}
	resolver := staticResolver{net.ParseIP("203.0.113.10")}
	if _, err := EvaluateEndpoint(context.Background(), resolver, "https://user:pass@objects.example/x"); err == nil {
		t.Fatal("userinfo must be denied")
	}
	if _, err := EvaluateEndpoint(context.Background(), resolver, "https://127.0.0.1/x"); err == nil {
		t.Fatal("literal loopback must be denied")
	}
	if _, err := EvaluateRedirects(context.Background(), resolver, "https://objects.example/a", []string{"https://a", "https://b", "https://c", "https://d"}); err == nil {
		t.Fatal("redirect limit must fail closed")
	}
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, RunGrantRequest) error { return nil }

type denyAll struct{}

func (denyAll) Authorize(context.Context, RunGrantRequest) error {
	return NewError(CategoryAuthorization, ReasonGrantForbidden)
}

type frozenClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *frozenClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

type staticResolver []net.IP

func (resolver staticResolver) LookupIP(context.Context, string) ([]net.IP, error) {
	return append([]net.IP(nil), resolver...), nil
}

type scriptedResolver struct {
	mu      sync.Mutex
	answers map[string][][]net.IP
}

func (resolver *scriptedResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	queue := resolver.answers[host]
	if len(queue) == 0 {
		return nil, NewError(CategoryNetwork, ReasonNetworkDenied)
	}
	answer := queue[0]
	if len(queue) > 1 {
		resolver.answers[host] = queue[1:]
	}
	return answer, nil
}

type memStore struct {
	mu     sync.Mutex
	assets map[string]Asset
	grants map[string]Grant
	opaque map[string]string
	idem   map[string]string
}

func newMemStore() *memStore {
	return &memStore{
		assets: map[string]Asset{},
		grants: map[string]Grant{},
		opaque: map[string]string{},
		idem:   map[string]string{},
	}
}

func (store *memStore) Put(_ context.Context, asset Asset) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	asset.Content = append([]byte(nil), asset.Content...)
	store.assets[asset.Binding.TenantID+"/"+asset.Binding.AssetID] = asset
	return nil
}

func (store *memStore) Get(_ context.Context, tenantID, assetID string) (Asset, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	asset, ok := store.assets[tenantID+"/"+assetID]
	if !ok {
		return Asset{}, NewError(CategoryNotFound, ReasonNotFound)
	}
	asset.Content = append([]byte(nil), asset.Content...)
	return asset, nil
}

func (store *memStore) Save(_ context.Context, grant Grant) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	idemKey := grant.Binding.TenantID + "/" + grant.IdempotencyKeyDigest
	if _, exists := store.idem[idemKey]; exists {
		return NewError(CategoryConflict, ReasonIdempotencyConflict)
	}
	store.grants[grant.Binding.TenantID+"/"+grant.GrantID] = grant
	store.opaque[grant.OpaqueDigest] = grant.Binding.TenantID + "/" + grant.GrantID
	store.idem[idemKey] = grant.GrantID
	return nil
}

func (store *memStore) GetGrant(_ context.Context, tenantID, grantID string) (Grant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	grant, ok := store.grants[tenantID+"/"+grantID]
	if !ok {
		return Grant{}, NewError(CategoryNotFound, ReasonNotFound)
	}
	return grant, nil
}

func (store *memStore) FindByOpaqueDigest(_ context.Context, digest string) (Grant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key, ok := store.opaque[digest]
	if !ok {
		return Grant{}, NewError(CategoryNotFound, ReasonNotFound)
	}
	tenantID, grantID, ok := strings.Cut(key, "/")
	if !ok {
		return Grant{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return store.grants[tenantID+"/"+grantID], nil
}

func (store *memStore) FindByIdempotency(_ context.Context, tenantID, keyDigest string) (Grant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	grantID, ok := store.idem[tenantID+"/"+keyDigest]
	if !ok {
		return Grant{}, NewError(CategoryNotFound, ReasonNotFound)
	}
	return store.grants[tenantID+"/"+grantID], nil
}

func (store *memStore) Consume(_ context.Context, tenantID, grantID string, now time.Time) (Grant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	grant, ok := store.grants[tenantID+"/"+grantID]
	if !ok {
		return Grant{}, NewError(CategoryNotFound, ReasonNotFound)
	}
	if err := grant.UsableAt(now); err != nil {
		return Grant{}, err
	}
	grant.Uses++
	store.grants[tenantID+"/"+grantID] = grant
	return grant, nil
}

func (store *memStore) Revoke(_ context.Context, tenantID, grantID string, _ time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	grant, ok := store.grants[tenantID+"/"+grantID]
	if !ok {
		return NewError(CategoryNotFound, ReasonNotFound)
	}
	grant.Revoked = true
	store.grants[tenantID+"/"+grantID] = grant
	return nil
}

func testDeps(t *testing.T, authorizer RunGrantAuthorizer) Dependencies {
	t.Helper()
	return Dependencies{
		Clock:      &frozenClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)},
		IDs:        testIDs{},
		Faults:     noFaults{},
		UoW:        directUoW{},
		Audit:      validatingAudit{},
		Authorizer: authorizer,
		Assets:     newMemStore(),
		Grants:     newMemStore(),
		Resolver:   staticResolver{net.ParseIP("203.0.113.10")},
		Tokens:     testTokenIssuer(t),
	}
}

func testTokenIssuer(t *testing.T) TokenIssuer {
	t.Helper()
	issuer, err := NewHMACTokenIssuer("atk_test", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return issuer
}

func testService(t *testing.T, authorizer RunGrantAuthorizer) *Service {
	t.Helper()
	service, err := New(testDeps(t, authorizer))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testIssue(operation Operation) IssueRequest {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	return IssueRequest{
		Metadata:       testMetadata(),
		Caller:         Caller{TenantID: testTenant, PrincipalID: testPrn, CredentialID: testCred},
		RunID:          testRun,
		AssetID:        testAsset,
		Operation:      operation,
		Name:           "asset.pdf",
		MediaType:      "application/pdf",
		SizeBytes:      12,
		Digest:         testDigest,
		IdempotencyKey: "idem-key-1",
		NotBefore:      now,
		ExpiresAt:      now.Add(time.Minute),
		MaxUses:        2,
	}
}

func testMetadata() platform.RequestMetadata {
	return platform.RequestMetadata{
		RequestID:  "req_018f3b2a-7c31-7a11-8abc-1234567890ab",
		TraceID:    "0123456789abcdef0123456789abcdef",
		SpanID:     "0123456789abcdef",
		TraceFlags: "00",
	}
}

type testIDs struct{}

func (testIDs) NewID(_ context.Context, kind platformports.IDKind) (string, error) {
	if kind != platformports.IDAudit {
		return "", NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return "aud_018f3b2a-7c31-7a11-8abc-1234567890ab", nil
}

type noFaults struct{}

func (noFaults) Check(context.Context, platformports.Checkpoint) error { return nil }

type directUoW struct{}

func (directUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	return callback(ctx)
}

type validatingAudit struct{}

func (validatingAudit) AppendObservation(_ context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	return observability.ValidateObservationPair(audit, span)
}

func requireReason(t *testing.T, err error, reason ErrorReason) {
	t.Helper()
	typed, ok := AsError(err)
	if !ok || typed.Reason != reason {
		t.Fatalf("got %v want %s", err, reason)
	}
}
