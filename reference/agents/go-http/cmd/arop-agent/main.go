package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	sqlitestore "github.com/gmslll/agent-runtime-operations-protocol/reference/agents/go-http/internal/storage/sqlite"
	sqlitemigration "github.com/gmslll/agent-runtime-operations-protocol/reference/agents/go-http/migrations/sqlite"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/provider"
)

type options struct {
	listen, database, issuer, audience, endpoint, deployment, instance, transport string
	keyFile, tlsCertificate, tlsKey                                               string
	generation                                                                    uint64
	insecureLoopback                                                              bool
}

func main() {
	if err := run(); err != nil {
		slog.Error("agent runtime stopped", "reason", stableReason(err))
		os.Exit(1)
	}
}

func run() error {
	configuration := parseFlags()
	if err := configuration.validate(); err != nil {
		return err
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := sqlitestore.Open(root, configuration.database, sqlitemigration.Initial)
	if err != nil {
		return errors.New("open durable provider store")
	}
	defer store.Close()
	keys, err := loadKeys(configuration.keyFile)
	if err != nil {
		return err
	}
	verifier, err := provider.NewES256Verifier(provider.ES256VerifierConfig{
		Keys: keys, Issuer: configuration.issuer, Audience: configuration.audience, Endpoint: configuration.endpoint,
		DeploymentID: configuration.deployment, InstanceID: configuration.instance, Generation: configuration.generation, TransportProfile: configuration.transport,
	})
	if err != nil {
		return errors.New("configure run token verifier")
	}
	runtime, err := provider.NewRuntime(provider.Config{Store: store, Verifier: verifier, Handler: provider.HandlerFunc(execute)})
	if err != nil {
		return errors.New("configure provider runtime")
	}
	if err = runtime.Recover(root, 1000); err != nil {
		return errors.New("recover durable provider attempts")
	}
	server := &http.Server{
		Addr: configuration.listen, Handler: runtime, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	result := make(chan error, 1)
	go func() {
		if configuration.insecureLoopback {
			result <- server.ListenAndServe()
			return
		}
		result <- server.ListenAndServeTLS(configuration.tlsCertificate, configuration.tlsKey)
	}()
	select {
	case <-root.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case err = <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("serve provider runtime")
	}
}

func execute(ctx context.Context, execution *provider.Execution, request runwire.RunRequest) (runwire.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return runwire.RunResult{}, err
	}
	content := []runwire.AROPV1ContentPart{{Text: &runwire.AROPV1ContentPartText{Type: "text", Text: "accepted"}}}
	if request.Effects.Write != nil || request.Effects.Irreversible != nil {
		effectID := ""
		if request.Effects.Write != nil {
			effectID = string(request.Effects.Write.EffectID)
		} else {
			effectID = string(request.Effects.Irreversible.EffectID)
		}
		if _, err := execution.Effect(ctx, effectID, json.RawMessage(`{"operation":"reference.accept"}`), func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"accepted":true}`), nil
		}); err != nil {
			return runwire.RunResult{}, err
		}
	}
	encoded, _ := json.Marshal(content)
	digest := sha256.Sum256(encoded)
	now := time.Now().UTC()
	return runwire.RunResult{
		SchemaVersion: 1, RunID: runwire.RunId(execution.RunID()), State: "succeeded", CompletedAt: runwire.DateTime(now.Format(time.RFC3339Nano)),
		Snapshot: &runwire.Snapshot{Revision: 1, Digest: runwire.Sha256Digest("sha256:" + hex.EncodeToString(digest[:])), Content: content},
		Usage:    runwire.Usage{InputTokens: 0, OutputTokens: 0, DurationMs: 0},
	}, nil
}

func parseFlags() options {
	var value options
	flag.StringVar(&value.listen, "listen", "127.0.0.1:8443", "listen address")
	flag.StringVar(&value.database, "database", "", "absolute SQLite path")
	flag.StringVar(&value.issuer, "issuer", "", "Run Token issuer")
	flag.StringVar(&value.audience, "audience", "", "Run Token audience")
	flag.StringVar(&value.endpoint, "endpoint", "", "registered https runtime endpoint")
	flag.StringVar(&value.deployment, "deployment-id", "", "deployment identifier")
	flag.StringVar(&value.instance, "instance-id", "", "runtime instance identifier")
	flag.StringVar(&value.transport, "transport-profile", "direct", "direct or proxy")
	flag.Uint64Var(&value.generation, "generation", 0, "fencing generation")
	flag.StringVar(&value.keyFile, "verification-keys", "", "JSON public verification key file")
	flag.StringVar(&value.tlsCertificate, "tls-cert", "", "TLS certificate PEM")
	flag.StringVar(&value.tlsKey, "tls-key", "", "TLS private key PEM")
	flag.BoolVar(&value.insecureLoopback, "insecure-loopback", false, "allow cleartext only on loopback for local conformance")
	flag.Parse()
	return value
}

func (value options) validate() error {
	if value.database == "" || !filepath.IsAbs(value.database) || value.issuer == "" || value.audience == "" || value.endpoint == "" || value.deployment == "" || value.instance == "" || value.generation == 0 || value.keyFile == "" {
		return errors.New("required runtime configuration is missing")
	}
	if value.insecureLoopback {
		host, _, err := net.SplitHostPort(value.listen)
		address := net.ParseIP(host)
		if err != nil || address == nil || !address.IsLoopback() || value.tlsCertificate != "" || value.tlsKey != "" {
			return errors.New("insecure mode requires a cleartext loopback listener")
		}
	} else if value.tlsCertificate == "" || value.tlsKey == "" {
		return errors.New("TLS certificate and key are required")
	}
	return nil
}

type keyDocument struct {
	Keys []struct {
		KeyID       string `json:"kid"`
		X           string `json:"x"`
		Y           string `json:"y"`
		NotBefore   string `json:"not_before"`
		SignUntil   string `json:"sign_until"`
		VerifyUntil string `json:"verify_until"`
	} `json:"keys"`
}

func loadKeys(path string) (provider.StaticKeys, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
		return nil, errors.New("invalid verification key file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read verification key file")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document keyDocument
	if err = decoder.Decode(&document); err != nil || len(document.Keys) == 0 || len(document.Keys) > 32 {
		return nil, errors.New("decode verification key file")
	}
	keys := provider.StaticKeys{}
	for _, item := range document.Keys {
		notBefore, firstErr := time.Parse(time.RFC3339Nano, item.NotBefore)
		signUntil, secondErr := time.Parse(time.RFC3339Nano, item.SignUntil)
		verifyUntil, thirdErr := time.Parse(time.RFC3339Nano, item.VerifyUntil)
		if firstErr != nil || secondErr != nil || thirdErr != nil || item.KeyID == "" || !base64URL(item.X) || !base64URL(item.Y) {
			return nil, errors.New("invalid verification key metadata")
		}
		if _, exists := keys[item.KeyID]; exists {
			return nil, errors.New("duplicate verification key")
		}
		keys[item.KeyID] = provider.VerificationKey{KeyID: item.KeyID, X: item.X, Y: item.Y, NotBefore: notBefore, SignUntil: signUntil, VerifyUntil: verifyUntil}
	}
	return keys, nil
}

func base64URL(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == 32
}

func stableReason(err error) string { return fmt.Sprintf("%T", err) }
