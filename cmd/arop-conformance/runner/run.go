package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"

	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const (
	maxFixtureBytes = 16 << 20
	maxDriverOutput = 1 << 20
	maxTargetBytes  = 64 << 20
)

func Run(ctx context.Context, config Config) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("conformance context is required")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if config.Root == "" {
		config.Root = "."
	}
	if config.Profile == "" || config.Target == "" {
		return Report{}, errors.New("conformance profile and target are required")
	}
	if config.TimeoutCeiling < 0 || config.TimeoutCeiling > time.Minute {
		return Report{}, errors.New("conformance timeout ceiling is invalid")
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	catalog, err := LoadCatalog(config.Root)
	if err != nil {
		return Report{}, err
	}
	scenarios, profileDigest, err := catalog.Resolve(config.Profile, config.ScenarioFilters)
	if err != nil {
		return Report{}, err
	}
	target, targetDigest, cleanupTarget, err := resolveTarget(catalog.root, config.Target)
	if err != nil {
		return Report{}, err
	}
	defer cleanupTarget()
	startedAt := clock().UTC()
	report := Report{SchemaVersion: 1, Protocol: DriverProtocol, Profile: config.Profile, ProfileSHA256: profileDigest, TargetSHA256: targetDigest, StartedAt: startedAt.Format(time.RFC3339Nano), Passed: true, Results: []ScenarioResult{}}
	outcomes := map[string]string{}
	for _, selected := range scenarios {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		blocked := false
		for _, dependency := range selected.DependsOn {
			if outcome := outcomes[dependency]; outcome != "pass" {
				blocked = true
				break
			}
		}
		if blocked {
			result := ScenarioResult{ID: selected.ID, Title: selected.Title, Required: selected.Required, Outcome: "fail", Message: "dependency did not pass", ScenarioSHA256: selected.Digest, FixturePath: selected.Fixture.Path, FixtureSHA256: selected.Fixture.SHA256}
			report.Results = append(report.Results, result)
			report.FailedCount++
			report.Passed = false
			outcomes[selected.ID] = "fail"
			continue
		}
		result, err := executeScenario(ctx, catalog.root, target, selected, config.TimeoutCeiling)
		if err != nil {
			return Report{}, err
		}
		report.Results = append(report.Results, result)
		outcomes[selected.ID] = result.Outcome
		switch result.Outcome {
		case "pass":
			report.PassedCount++
		case "skip":
			report.SkippedCount++
		default:
			report.FailedCount++
			report.Passed = false
		}
	}
	report.CompletedAt = clock().UTC().Format(time.RFC3339Nano)
	return report, nil
}

func executeScenario(parent context.Context, root, target string, selected resolvedScenario, timeoutCeiling time.Duration) (ScenarioResult, error) {
	fixture, err := readRegular(root, selected.Fixture.Path, maxFixtureBytes)
	if err != nil {
		return ScenarioResult{}, fmt.Errorf("scenario %s fixture: %w", selected.ID, err)
	}
	actualDigest := digest(fixture)
	if actualDigest != selected.Fixture.SHA256 {
		return ScenarioResult{}, fmt.Errorf("scenario %s fixture digest=%s want %s", selected.ID, actualDigest, selected.Fixture.SHA256)
	}
	invocation := Invocation{Protocol: DriverProtocol, ScenarioID: selected.ID, Request: cloneRequest(selected.Request), Fixture: InvocationFixture{Path: selected.Fixture.Path, SHA256: actualDigest, MediaType: selected.Fixture.MediaType, Bytes: int64(len(fixture)), DataBase64: base64.StdEncoding.EncodeToString(fixture)}}
	input, err := json.Marshal(invocation)
	if err != nil {
		return ScenarioResult{}, errors.New("encode conformance invocation")
	}
	timeout := time.Duration(selected.TimeoutMS) * time.Millisecond
	if timeoutCeiling > 0 && timeoutCeiling < timeout {
		timeout = timeoutCeiling
	}
	if timeout <= 0 {
		return ScenarioResult{}, errors.New("scenario timeout ceiling is invalid")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	stdout := &boundedBuffer{limit: maxDriverOutput}
	stderr := &boundedBuffer{limit: maxDriverOutput}
	command := exec.CommandContext(ctx, target)
	command.Dir = root
	command.Env = targetEnvironment()
	command.Stdin = bytes.NewReader(input)
	command.Stdout = stdout
	command.Stderr = stderr
	started := time.Now()
	commandErr := command.Run()
	duration := time.Since(started).Milliseconds()
	result := ScenarioResult{ID: selected.ID, Title: selected.Title, Required: selected.Required, Outcome: "fail", DurationMS: duration, ScenarioSHA256: selected.Digest, FixturePath: selected.Fixture.Path, FixtureSHA256: actualDigest, FixtureBytes: int64(len(fixture))}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Message = "target timed out"
		return result, nil
	}
	if errors.Is(ctx.Err(), context.Canceled) && parent.Err() != nil {
		return ScenarioResult{}, parent.Err()
	}
	if commandErr != nil || stdout.overflow || stderr.overflow {
		result.Message = "target execution failed"
		return result, nil
	}
	var response DriverResponse
	if err := protocolcore.DecodeAuthoring(stdout.Bytes(), &response); err != nil {
		result.Message = "target response is invalid"
		return result, nil
	}
	if response.Protocol != DriverProtocol || response.ScenarioID != selected.ID || !utf8.ValidString(response.Message) || utf8.RuneCountInString(response.Message) > 2048 {
		result.Message = "target response identity is invalid"
		return result, nil
	}
	switch response.Outcome {
	case "pass":
		result.Outcome = "pass"
	case "fail":
		result.Message = "target reported failure"
	case "skip":
		if selected.Required {
			result.Message = "required scenario cannot be skipped"
		} else {
			result.Outcome = "skip"
			result.Message = "target reported skip"
		}
	default:
		result.Message = "target response outcome is invalid"
	}
	return result, nil
}

func resolveTarget(root, value string) (string, string, func(), error) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", "", nil, errors.New("conformance target must be an existing non-symlink path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Size() <= 0 || info.Size() > maxTargetBytes {
		return "", "", nil, errors.New("conformance target must be a bounded executable regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", nil, errors.New("open conformance target")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTargetBytes+1))
	if err != nil || int64(len(data)) > maxTargetBytes {
		return "", "", nil, errors.New("read conformance target")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || after.Mode() != info.Mode() || after.Size() != info.Size() {
		return "", "", nil, errors.New("conformance target changed while reading")
	}
	scratch, err := os.MkdirTemp("", "arop-conformance-target-")
	if err != nil {
		return "", "", nil, errors.New("create private target snapshot")
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }
	if err := os.Chmod(scratch, 0o700); err != nil {
		cleanup()
		return "", "", nil, errors.New("secure private target snapshot")
	}
	snapshot := filepath.Join(scratch, "driver")
	if err := os.WriteFile(snapshot, data, 0o500); err != nil {
		cleanup()
		return "", "", nil, errors.New("write private target snapshot")
	}
	return snapshot, digest(data), cleanup, nil
}

func targetEnvironment() []string {
	values := []string{"AROP_CONFORMANCE_OFFLINE=1", "HOME=", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	for _, key := range []string{"PATH", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			values = append(values, key+"="+value)
		}
	}
	return values
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (writer *boundedBuffer) Write(data []byte) (int, error) {
	remaining := writer.limit - writer.buffer.Len()
	if remaining <= 0 {
		writer.overflow = true
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = writer.buffer.Write(data[:remaining])
		writer.overflow = true
		return len(data), nil
	}
	_, _ = writer.buffer.Write(data)
	return len(data), nil
}

func (writer *boundedBuffer) Bytes() []byte { return writer.buffer.Bytes() }
