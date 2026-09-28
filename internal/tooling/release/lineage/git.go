// Package lineage verifies report execution provenance and cross-commit lineage.
package lineage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	releaseevidence "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

type Strategy string

const (
	StrategyCurrent        Strategy = "current"
	StrategyIsolatedReplay Strategy = "isolated_replay"
	StrategyTrustedCI      Strategy = "trusted_ci"
)

type CIProvenance struct {
	SchemaVersion     int    `json:"schema_version"`
	ReportSHA256      string `json:"report_sha256"`
	SourceCommit      string `json:"source_commit"`
	SourceTree        string `json:"source_tree"`
	CheckerPath       string `json:"checker_path"`
	CheckerSHA256     string `json:"checker_sha256"`
	InputsSHA256      string `json:"inputs_sha256"`
	RuntimeInputsHash string `json:"runtime_inputs_sha256"`
	Command           string `json:"command"`
	JobWorkflowRef    string `json:"job_workflow_ref"`
	JobWorkflowSHA    string `json:"job_workflow_sha"`
	WorkflowSHA256    string `json:"workflow_sha256"`
	WorkflowLockPath  string `json:"workflow_lock_path"`
	WorkflowLockHash  string `json:"workflow_lock_sha256"`
	ArtifactSHA256    string `json:"artifact_sha256"`
	Repository        string `json:"repository"`
	Event             string `json:"event"`
}

type Candidate struct {
	Phase             string
	ReportPath        string
	Strategy          Strategy
	ProvenancePayload []byte
	Envelope          *releaseevidence.Envelope
	Trust             *releaseevidence.VerifiedTrust
	Expected          releaseevidence.ExpectedBindings
}

type VerifiedCandidate struct {
	Phase           string   `json:"phase"`
	Strategy        Strategy `json:"strategy"`
	ReportPath      string   `json:"report_path"`
	ReportSHA256    string   `json:"report_sha256"`
	ClaimedCommit   string   `json:"claimed_commit"`
	ClaimedTree     string   `json:"claimed_tree"`
	CheckerPath     string   `json:"checker_path"`
	CheckerSHA256   string   `json:"checker_sha256"`
	InputsSHA256    string   `json:"inputs_sha256"`
	RuntimeInputs   string   `json:"runtime_inputs_sha256"`
	Command         string   `json:"command"`
	ProvenanceHash  string   `json:"provenance_sha256,omitempty"`
	VerificationRef string   `json:"verification_ref,omitempty"`
}

var (
	phasePattern   = regexp.MustCompile(`^P(?:0[1-9]|[1-9][0-9])$`)
	makePattern    = regexp.MustCompile(`^make ([a-z0-9][a-z0-9-]*)$`)
	workflowRefPat = regexp.MustCompile(`^([^/]+/[^/]+)/(\.github/workflows/[A-Za-z0-9._/-]+)@[^[:space:]]+$`)
)

func VerifyCandidate(root string, candidate Candidate, now time.Time) (VerifiedCandidate, error) {
	if !phasePattern.MatchString(candidate.Phase) {
		return VerifiedCandidate{}, errors.New("phase must be canonical PNN")
	}
	root, err := canonicalRoot(root)
	if err != nil {
		return VerifiedCandidate{}, err
	}
	raw, parsed, err := loadReport(root, candidate.ReportPath)
	if err != nil {
		return VerifiedCandidate{}, err
	}
	if !parsed.Success || parsed.Provenance.Git.Dirty || len(parsed.Provenance.Git.DirtyEntries) != 0 {
		return VerifiedCandidate{}, errors.New("release aggregation accepts only clean successful reports")
	}
	tree, err := git(root, "rev-parse", parsed.Provenance.Git.Head+"^{tree}")
	if err != nil {
		return VerifiedCandidate{}, errors.New("claimed commit or tree is unavailable")
	}
	verified := VerifiedCandidate{
		Phase: candidate.Phase, Strategy: candidate.Strategy, ReportPath: filepath.ToSlash(filepath.Clean(candidate.ReportPath)),
		ReportSHA256: sha(raw), ClaimedCommit: parsed.Provenance.Git.Head, ClaimedTree: tree,
		CheckerPath: parsed.Provenance.Checker.Path, CheckerSHA256: parsed.Provenance.Checker.SHA256,
		InputsSHA256: parsed.Provenance.Inputs.SHA256, RuntimeInputs: parsed.Provenance.RuntimeInputs.SHA256, Command: parsed.Provenance.Command,
	}
	switch candidate.Strategy {
	case StrategyCurrent:
		if _, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: candidate.ReportPath}); err != nil {
			return VerifiedCandidate{}, fmt.Errorf("current report verification: %w", err)
		} else if mode != "current-worktree" {
			return VerifiedCandidate{}, errors.New("current strategy did not verify current worktree")
		}
		verified.VerificationRef = "current-worktree"
	case StrategyIsolatedReplay:
		replayDigest, err := replay(root, candidate.ReportPath, parsed)
		if err != nil {
			return VerifiedCandidate{}, err
		}
		verified.VerificationRef = replayDigest
	case StrategyTrustedCI:
		provenanceDigest, envelopeDigest, err := verifyCI(root, raw, parsed, tree, candidate, now)
		if err != nil {
			return VerifiedCandidate{}, err
		}
		verified.ProvenanceHash, verified.VerificationRef = provenanceDigest, envelopeDigest
	default:
		return VerifiedCandidate{}, errors.New("unsupported report verification strategy")
	}
	return verified, nil
}

func verifyCI(root string, raw []byte, parsed *report.Report, tree string, candidate Candidate, now time.Time) (string, string, error) {
	if candidate.Envelope == nil || candidate.Trust == nil || len(candidate.ProvenancePayload) == 0 {
		return "", "", errors.New("trusted_ci requires provenance payload, detached envelope and verified trust")
	}
	var payload CIProvenance
	if err := decodeStrict(candidate.ProvenancePayload, &payload); err != nil {
		return "", "", fmt.Errorf("CI provenance: %w", err)
	}
	if payload.SchemaVersion != 1 || payload.ReportSHA256 != sha(raw) || payload.ArtifactSHA256 != sha(raw) || payload.SourceCommit != parsed.Provenance.Git.Head || payload.SourceTree != tree || payload.CheckerPath != parsed.Provenance.Checker.Path || payload.CheckerSHA256 != parsed.Provenance.Checker.SHA256 || payload.InputsSHA256 != parsed.Provenance.Inputs.SHA256 || payload.RuntimeInputsHash != parsed.Provenance.RuntimeInputs.SHA256 || payload.Command != parsed.Provenance.Command {
		return "", "", errors.New("CI provenance does not exactly bind report, commit, tree, checker, command and inputs")
	}
	match := workflowRefPat.FindStringSubmatch(payload.JobWorkflowRef)
	if len(match) != 3 || match[1] != payload.Repository || len(payload.JobWorkflowSHA) != 40 {
		return "", "", errors.New("CI job_workflow_ref/repository/SHA is invalid")
	}
	workflow, err := gitBytes(root, "show", payload.JobWorkflowSHA+":"+match[2])
	if err != nil || "sha256:"+sha(workflow) != payload.WorkflowSHA256 {
		return "", "", errors.New("CI workflow blob digest does not match job_workflow_sha")
	}
	if payload.WorkflowLockPath == "" {
		return "", "", errors.New("CI workflow lock path is required")
	}
	lock, err := gitBytes(root, "show", payload.SourceCommit+":"+payload.WorkflowLockPath)
	if err != nil || "sha256:"+sha(lock) != payload.WorkflowLockHash {
		return "", "", errors.New("CI workflow lock digest does not match source commit")
	}
	if candidate.Expected.Kind != "ci_report_provenance" || candidate.Expected.Role != "independent_reviewer" {
		return "", "", errors.New("CI provenance must use the signed independent_reviewer policy")
	}
	verifiedEnvelope, err := releaseevidence.VerifyEnvelope(*candidate.Envelope, candidate.ProvenancePayload, *candidate.Trust, candidate.Expected, now)
	if err != nil {
		return "", "", fmt.Errorf("CI detached provenance: %w", err)
	}
	if candidate.Envelope.Subject.Commit != payload.SourceCommit || candidate.Envelope.Subject.Tree != payload.SourceTree {
		return "", "", errors.New("CI envelope subject differs from provenance source")
	}
	identity := candidate.Envelope.Verification.Identity
	if identity == nil || identity.Repository != payload.Repository || identity.WorkflowRef != payload.JobWorkflowRef || identity.WorkflowSHA != payload.JobWorkflowSHA || identity.Event != payload.Event {
		return "", "", errors.New("CI Sigstore identity does not exactly bind job workflow and event")
	}
	return "sha256:" + sha(candidate.ProvenancePayload), verifiedEnvelope.EnvelopeSHA256, nil
}

func replay(root, reportPath string, expected *report.Report) (digest string, resultErr error) {
	match := makePattern.FindStringSubmatch(expected.Provenance.Command)
	if len(match) != 2 {
		return "", errors.New("isolated replay permits only an exact single make target")
	}
	temp, err := os.MkdirTemp("", "arop-lineage-replay-")
	if err != nil {
		return "", err
	}
	_ = os.Remove(temp)
	added := false
	defer func() {
		if added {
			_, removeErr := git(root, "worktree", "remove", "--force", temp)
			if resultErr == nil && removeErr != nil {
				resultErr = fmt.Errorf("remove isolated worktree: %w", removeErr)
			}
		}
		_ = os.RemoveAll(temp)
	}()
	if _, err := git(root, "worktree", "add", "--detach", temp, expected.Provenance.Git.Head); err != nil {
		return "", fmt.Errorf("create isolated worktree: %w", err)
	}
	added = true
	command := exec.Command("make", match[1])
	command.Dir = temp
	command.Env = cleanEnvironment(os.Environ())
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("isolated checker replay failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	rel, err := safeRelative(root, reportPath)
	if err != nil {
		return "", err
	}
	replayed, mode, err := report.Verify(report.VerifyOptions{Root: temp, ReportPath: rel})
	if err != nil || mode != "current-worktree" {
		return "", fmt.Errorf("replayed report verification failed: %w", err)
	}
	if !replayed.Success || replayed.Provenance.Git.Head != expected.Provenance.Git.Head || replayed.Provenance.Checker != expected.Provenance.Checker || replayed.Provenance.Inputs.SHA256 != expected.Provenance.Inputs.SHA256 || replayed.Provenance.RuntimeInputs.SHA256 != expected.Provenance.RuntimeInputs.SHA256 || replayed.Provenance.Command != expected.Provenance.Command || !sameOutcomes(replayed.Checks, expected.Checks) {
		return "", errors.New("isolated replay result differs from historical report")
	}
	data, err := os.ReadFile(filepath.Join(temp, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	return "sha256:" + sha(data), nil
}

func sameOutcomes(left, right []report.Check) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].Passed != right[i].Passed {
			return false
		}
	}
	return true
}

func loadReport(root, path string) ([]byte, *report.Report, error) {
	rel, err := safeRelative(root, path)
	if err != nil {
		return nil, nil, err
	}
	abs, err := structuredfile.RequireInsideFile(root, filepath.Join(root, filepath.FromSlash(rel)), "report")
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, err
	}
	var parsed report.Report
	if err := decodeStrict(raw, &parsed); err != nil {
		return nil, nil, err
	}
	return raw, &parsed, nil
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON must contain exactly one value")
	}
	return nil
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func safeRelative(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("path must be repository-relative")
	}
	return structuredfile.SafeRelative(root, filepath.ToSlash(filepath.Clean(path)))
}

func git(root string, arguments ...string) (string, error) {
	data, err := gitBytes(root, arguments...)
	return strings.TrimSpace(string(data)), err
}

func gitBytes(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = cleanEnvironment(os.Environ())
	data, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func cleanEnvironment(environment []string) []string {
	blocked := map[string]bool{"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true, "GIT_CONFIG_COUNT": true, "GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GOFLAGS": true, "GOENV": true, "GOWORK": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true}
	result := make([]string, 0, len(environment)+5)
	for _, item := range environment {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if !blocked[strings.ToUpper(key)] && !strings.HasPrefix(strings.ToUpper(key), "GIT_CONFIG_KEY_") && !strings.HasPrefix(strings.ToUpper(key), "GIT_CONFIG_VALUE_") {
			result = append(result, item)
		}
	}
	return append(result, "GIT_CONFIG_NOSYSTEM=1", "GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local")
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sortedCandidates(values []VerifiedCandidate) []VerifiedCandidate {
	copyValue := append([]VerifiedCandidate{}, values...)
	sort.Slice(copyValue, func(i, j int) bool { return copyValue[i].Phase < copyValue[j].Phase })
	return copyValue
}
