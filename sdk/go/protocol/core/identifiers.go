package core

import (
	"fmt"
	"regexp"
	"strings"
)

type ResourceKind string

const (
	ResourceSession      ResourceKind = "session"
	ResourceDeployment   ResourceKind = "deployment"
	ResourceLease        ResourceKind = "lease"
	ResourceRun          ResourceKind = "run"
	ResourceAttempt      ResourceKind = "attempt"
	ResourceEvent        ResourceKind = "event"
	ResourceAsset        ResourceKind = "asset"
	ResourceConversation ResourceKind = "conversation"
)

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var semanticVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var capabilityIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:\.[a-z0-9]+(?:-[a-z0-9]+)*)+\.v[1-9][0-9]*$`)
var extensionIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(?:\.[a-z0-9]+(?:-[a-z0-9]+)*){2,}\.v[1-9][0-9]*$`)

func ValidateSlug(value string) error {
	if len(value) == 0 || len(value) > 128 || !slugPattern.MatchString(value) {
		return fmt.Errorf("invalid AROP slug %q", value)
	}
	return nil
}

func ValidateAgentID(value string) error    { return validateNamedSlug("agent", value) }
func ValidateSkillID(value string) error    { return validateNamedSlug("skill", value) }
func ValidateServiceID(value string) error  { return validateNamedSlug("service", value) }
func ValidateInstanceID(value string) error { return validateNamedSlug("instance", value) }

func validateNamedSlug(kind, value string) error {
	if err := ValidateSlug(value); err != nil {
		return fmt.Errorf("invalid %s identifier: %w", kind, err)
	}
	return nil
}

func ValidateUUIDv7(value string) error {
	if !uuidV7Pattern.MatchString(value) {
		return fmt.Errorf("invalid UUIDv7 %q", value)
	}
	return nil
}

func ValidateResourceID(kind ResourceKind, value string) error {
	prefixes := map[ResourceKind]string{
		ResourceSession: "ses_", ResourceDeployment: "dep_", ResourceLease: "lease_",
		ResourceRun: "run_", ResourceAttempt: "att_", ResourceEvent: "evt_",
		ResourceAsset: "asset_", ResourceConversation: "conv_",
	}
	prefix, ok := prefixes[kind]
	if !ok {
		return fmt.Errorf("unknown resource kind %q", kind)
	}
	if !strings.HasPrefix(value, prefix) || ValidateUUIDv7(strings.TrimPrefix(value, prefix)) != nil {
		return fmt.Errorf("invalid %s identifier %q", kind, value)
	}
	return nil
}

func ValidateSemanticVersion(value string) error {
	if len(value) == 0 || len(value) > 128 || !semanticVersionPattern.MatchString(value) {
		return fmt.Errorf("invalid semantic version %q", value)
	}
	return nil
}

func ValidateSHA256Digest(value string) error {
	if !sha256DigestPattern.MatchString(value) {
		return fmt.Errorf("invalid SHA-256 digest %q", value)
	}
	return nil
}

func ValidateCapabilityID(value string) error {
	if len(value) == 0 || len(value) > 200 || !capabilityIDPattern.MatchString(value) {
		return fmt.Errorf("invalid capability identifier %q", value)
	}
	return nil
}

func ValidateExtensionID(value string) error {
	if len(value) == 0 || len(value) > 200 || !extensionIDPattern.MatchString(value) {
		return fmt.Errorf("invalid extension identifier %q", value)
	}
	return nil
}

func ValidateEffectID(value string) error {
	if len(value) < 8 || len(value) > 200 || !strings.HasPrefix(value, "eff_") {
		return fmt.Errorf("invalid effect identifier %q", value)
	}
	for _, r := range strings.TrimPrefix(value, "eff_") {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("._:-", r) {
			return fmt.Errorf("invalid effect identifier %q", value)
		}
	}
	return nil
}
