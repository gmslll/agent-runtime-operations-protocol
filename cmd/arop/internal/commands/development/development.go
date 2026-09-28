// Package development implements the local-only AROP SQLite quickstart.
package development

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const configRelative = ".arop/quickstart.json"

type Config struct {
	SchemaVersion int    `json:"schema_version"`
	Listen        string `json:"listen"`
	Database      string `json:"database"`
	Backup        string `json:"backup_directory"`
	Manifest      string `json:"sample_manifest"`
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type DoctorReport struct {
	SchemaVersion int     `json:"schema_version"`
	Healthy       bool    `json:"healthy"`
	Checks        []Check `json:"checks"`
}

type Command struct {
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) string
	Client    *http.Client
}

func (command Command) Execute(ctx context.Context, arguments []string) error {
	if len(arguments) < 1 || len(arguments) > 2 {
		return errors.New("usage: arop init|dev|doctor [workspace]")
	}
	root := "."
	if len(arguments) == 2 {
		root = arguments[1]
	}
	switch arguments[0] {
	case "init":
		return command.init(root)
	case "doctor":
		return command.doctor(root)
	case "dev":
		return command.dev(ctx, root)
	default:
		return errors.New("usage: arop init|dev|doctor [workspace]")
	}
}

func (command Command) init(root string) error {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return err
	}
	state := filepath.Join(canonical, ".arop")
	if info, err := os.Lstat(state); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("quickstart state path is unsafe")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("inspect quickstart state")
	} else if err := os.Mkdir(state, 0o700); err != nil {
		return errors.New("create quickstart state")
	}
	config := Config{SchemaVersion: 1, Listen: "127.0.0.1:8080", Database: ".arop/control-plane.sqlite", Backup: ".arop/backups", Manifest: "agent-manifest.json"}
	data, _ := json.MarshalIndent(config, "", "  ")
	data = append(data, '\n')
	path := filepath.Join(canonical, filepath.FromSlash(configRelative))
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("quickstart config already exists with different content")
		}
	} else if !os.IsNotExist(readErr) {
		return errors.New("read quickstart config")
	} else if err := writeExclusive(path, data, 0o600); err != nil {
		return err
	}
	for _, relative := range []string{".arop/backups", ".arop/runtime"} {
		if err := os.MkdirAll(filepath.Join(canonical, filepath.FromSlash(relative)), 0o700); err != nil {
			return errors.New("create quickstart directory")
		}
	}
	manifest := []byte("{\n  \"protocol\": \"arop/v1\",\n  \"kind\": \"AgentManifest\",\n  \"identity\": {\"id\": \"quickstart.echo\", \"version\": \"1.0.0\", \"name\": \"Quickstart Echo\", \"summary\": \"Local quickstart sample\", \"owner\": {\"team\": \"local\"}},\n  \"skills\": [{\"id\": \"echo\", \"name\": \"Echo\", \"invoke_modes\": [\"params\"], \"input_schema\": {\"type\": \"object\"}, \"output_schema\": {\"type\": \"object\"}}],\n  \"execution\": {\"default_timeout_seconds\": 30, \"max_timeout_seconds\": 60, \"effects\": {\"level\": \"none\", \"idempotency\": \"supported\", \"human_confirmation\": false}, \"capabilities\": {}}\n}\n")
	manifestPath := filepath.Join(canonical, config.Manifest)
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		if err := writeExclusive(manifestPath, manifest, 0o600); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(defaultWriter(command.Stdout), "initialized %s\n", configRelative)
	return err
}

func (command Command) doctor(root string) error {
	report := inspect(root)
	encoder := json.NewEncoder(defaultWriter(command.Stdout))
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if !report.Healthy {
		return errors.New("quickstart doctor found failures")
	}
	return nil
}

func (command Command) dev(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	report := inspect(root)
	if !report.Healthy {
		return errors.New("quickstart doctor must pass before dev")
	}
	canonical, _ := canonicalRoot(root)
	config, err := loadConfig(canonical)
	if err != nil {
		return err
	}
	lookup := command.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	binary := lookup("AROP_CONTROL_PLANE_BINARY")
	migrations := lookup("AROP_MIGRATION_ROOT")
	if binary == "" || migrations == "" {
		return errors.New("AROP_CONTROL_PLANE_BINARY and AROP_MIGRATION_ROOT are required")
	}
	binary, err = regularAbsolute(binary)
	if err != nil {
		return errors.New("invalid Control Plane binary")
	}
	migrations, err = canonicalDirectory(migrations)
	if err != nil {
		return errors.New("invalid migration root")
	}
	runtime := filepath.Join(canonical, ".arop", "runtime")
	keyPath := filepath.Join(runtime, "asset-token.key")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return errors.New("create ephemeral quickstart key")
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		clear(key)
		return errors.New("write ephemeral quickstart key")
	}
	clear(key)
	defer os.Remove(keyPath)
	arguments := []string{
		"--mode=sqlite", "--database-dsn=" + filepath.Join(canonical, filepath.FromSlash(config.Database)),
		"--migration-root=" + migrations, "--backup-directory=" + filepath.Join(canonical, filepath.FromSlash(config.Backup)),
		"--asset-token-key-file=" + keyPath, "--asset-token-key-id=atk_quickstart_local",
		"--dispatch-issuer=https://quickstart.invalid", "--listen=" + config.Listen,
		"--storage-startup-timeout=45s", "--migration-timeout=2m", "--shutdown-timeout=10s",
	}
	process := exec.Command(binary, arguments...)
	process.Dir = canonical
	process.Env = cleanChildEnvironment()
	process.Stdout, process.Stderr = io.Discard, io.Discard
	configureProcess(process)
	if err := process.Start(); err != nil {
		return errors.New("start quickstart Control Plane")
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	stop := func() {
		stopProcess(process, done, 12*time.Second)
	}
	if err := command.waitReady(ctx, config.Listen, done); err != nil {
		stop()
		return err
	}
	fmt.Fprintf(defaultWriter(command.Stdout), "{\"ready\":true,\"endpoint\":\"http://%s\"}\n", config.Listen)
	select {
	case <-ctx.Done():
		stop()
		return nil
	case <-done:
		return errors.New("quickstart Control Plane exited")
	}
}

func (command Command) waitReady(ctx context.Context, listen string, done <-chan error) error {
	client := command.Client
	if client == nil {
		client = &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errors.New("quickstart Control Plane exited before readiness")
		case <-deadline.C:
			return errors.New("quickstart readiness timeout")
		case <-ticker.C:
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listen+"/v1/health/ready", nil)
			response, err := client.Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

func inspect(root string) DoctorReport {
	checks := []Check{}
	add := func(name string, err error, detail string) {
		if err != nil {
			detail = err.Error()
		}
		checks = append(checks, Check{Name: name, Passed: err == nil, Detail: detail})
	}
	canonical, err := canonicalRoot(root)
	add("workspace", err, "workspace is a canonical directory")
	var config Config
	if err == nil {
		config, err = loadConfig(canonical)
	}
	add("config", err, "quickstart config is strict, versioned, and secret-free")
	if err == nil {
		host, _, splitErr := net.SplitHostPort(config.Listen)
		address := net.ParseIP(host)
		if splitErr != nil || address == nil || !address.IsLoopback() {
			err = errors.New("listen address must be loopback")
		}
	}
	add("loopback", err, "development listener is loopback-only")
	if err == nil {
		for _, relative := range []string{config.Database, config.Backup, config.Manifest} {
			if !safeRelative(relative) {
				err = errors.New("quickstart path is not workspace-relative")
				break
			}
		}
	}
	add("paths", err, "database, backup, and manifest remain inside the workspace")
	if err == nil {
		info, statErr := os.Lstat(filepath.Join(canonical, filepath.FromSlash(config.Manifest)))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			err = errors.New("sample manifest is missing or unsafe")
		}
	}
	add("sample-manifest", err, "sample manifest is a regular local file")
	healthy := true
	for _, check := range checks {
		healthy = healthy && check.Passed
	}
	return DoctorReport{SchemaVersion: 1, Healthy: healthy, Checks: checks}
}

func loadConfig(root string) (Config, error) {
	path := filepath.Join(root, filepath.FromSlash(configRelative))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 64<<10 {
		return Config{}, errors.New("quickstart config is missing or unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errors.New("read quickstart config")
	}
	if bytes.Contains(bytes.ToLower(data), []byte("password")) || bytes.Contains(bytes.ToLower(data), []byte("token")) || bytes.Contains(bytes.ToLower(data), []byte("secret")) {
		return Config{}, errors.New("quickstart config contains a credential-like field")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil || config.SchemaVersion != 1 || config.Listen == "" || config.Database == "" || config.Backup == "" || config.Manifest == "" {
		return Config{}, errors.New("invalid quickstart config")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Config{}, errors.New("quickstart config has trailing JSON")
	}
	return config, nil
}

func canonicalRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("invalid workspace")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("workspace does not exist")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace is not a directory")
	}
	return canonical, nil
}

func canonicalDirectory(path string) (string, error) {
	value, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(value)
	if err != nil || !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return value, nil
}

func regularAbsolute(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path is not absolute")
	}
	value, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(value)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("path is not executable")
	}
	return value, nil
}

func safeRelative(value string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != filepath.FromSlash(value) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(value), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("create quickstart file")
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return errors.New("write quickstart file")
	}
	return file.Sync()
}

func cleanChildEnvironment() []string {
	values := []string{"HOME=", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	if path := os.Getenv("PATH"); path != "" {
		values = append(values, "PATH="+path)
	}
	return values
}

func defaultWriter(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}
