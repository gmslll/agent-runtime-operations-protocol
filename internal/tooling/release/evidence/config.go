package evidence

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const ExternalConfigSchema = "spec/schemas/external-config-evidence.schema.json"

type ExternalConfig struct {
	SchemaVersion      int               `json:"schema_version"`
	OIDCIssuer         string            `json:"oidc_issuer"`
	Repository         string            `json:"repository"`
	WorkflowPath       string            `json:"workflow_path"`
	Environment        string            `json:"environment"`
	WorkflowLockSHA256 string            `json:"workflow_lock_sha256"`
	PublicNamespace    string            `json:"public_namespace"`
	Registries         map[string]string `json:"registries"`
}

func ValidateExternalConfig(root string, data []byte, expectedWorkflowLockDigest string) (ExternalConfig, error) {
	var value ExternalConfig
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return value, err
	}
	if err := schema.ValidateFile(root, ExternalConfigSchema, parsed); err != nil {
		return value, err
	}
	if err := strictDecode(data, &value); err != nil {
		return value, err
	}
	if value.SchemaVersion != 1 || value.OIDCIssuer != "https://token.actions.githubusercontent.com" || value.Repository != "InfiniteStatesInc/agent-runtime-operations-protocol" || value.WorkflowPath != ".github/workflows/release.yml" || value.Environment != "arop-release" || value.WorkflowLockSHA256 != expectedWorkflowLockDigest || !isDigest(value.WorkflowLockSHA256) {
		return value, errors.New("external release configuration identity is invalid")
	}
	want := []string{"go", "npm", "oci", "pypi"}
	keys := make([]string, 0, len(value.Registries))
	for key := range value.Registries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !equalStrings(keys, want) {
		return value, errors.New("external registry inventory is not exact")
	}
	for name, raw := range value.Registries {
		parsedURL, err := url.Parse(raw)
		if err != nil || parsedURL.Scheme != "https" || parsedURL.Hostname() == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Path != "" && parsedURL.Path != "/" || parsedURL.Port() != "" {
			return value, fmt.Errorf("%s registry URL is not a credential-free HTTPS origin", name)
		}
		host := strings.ToLower(parsedURL.Hostname())
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
			return value, fmt.Errorf("%s registry host is not an explicit public DNS name", name)
		}
	}
	if containsSecretSentinel(data) {
		return value, errors.New("external release configuration contains a secret-bearing field or value")
	}
	return value, nil
}

func containsSecretSentinel(data []byte) bool {
	lower := bytes.ToLower(data)
	for _, value := range []string{"password", "private_key", "private key", "client_secret", "access_token", "refresh_token", "bearer ", "authorization", "cookie", "-----begin"} {
		if bytes.Contains(lower, []byte(value)) {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
