package register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	registrywire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/registry"
	registrysdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/registry"
)

const maxConfigBytes = 1 << 20

type Command struct {
	Client registrysdk.Client
	Stdout io.Writer
}

type Options struct {
	InstanceID     string
	ConfigPath     string
	IdempotencyKey string
}

type config struct {
	SchemaVersion int                    `json:"schema_version"`
	SessionID     string                 `json:"session_id"`
	ServiceID     string                 `json:"service_id"`
	Environment   string                 `json:"environment"`
	Endpoint      registrywire.Endpoint  `json:"endpoint"`
	Bindings      []registrywire.Binding `json:"bindings"`
	Runtime       registrywire.Runtime   `json:"runtime"`
}

func (command Command) Execute(ctx context.Context, options Options) error {
	if command.Stdout == nil {
		return errors.New("registry stdout is required")
	}
	configuration, err := readConfig(options.ConfigPath)
	if err != nil {
		return err
	}
	result, err := command.Client.Register(ctx, registrysdk.RegisterRequest{InstanceID: options.InstanceID, SessionID: configuration.SessionID, ServiceID: configuration.ServiceID, Environment: configuration.Environment, Endpoint: configuration.Endpoint, Bindings: configuration.Bindings, Runtime: configuration.Runtime, IdempotencyKey: options.IdempotencyKey})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(command.Stdout, "registered instance=%s session=%s generation=%d lease=%s ttl=%ds keepalive=%ds replay=%t\n", result.Instance.InstanceID, result.Instance.SessionID, result.Instance.Generation, result.Instance.LeaseID, result.LeaseTTLSeconds, result.KeepaliveIntervalSeconds, result.Replay)
	if err != nil {
		return errors.New("write registry result: unavailable")
	}
	return nil
}

func readConfig(path string) (config, error) {
	var value config
	if path == "" {
		return value, errors.New("registry config path is required")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maxConfigBytes {
		return value, errors.New("registry config is unavailable or invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return value, errors.New("registry config is unavailable or invalid")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return value, errors.New("registry config is unavailable or invalid")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return value, errors.New("registry config is unavailable or invalid")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || after.Mode()&os.ModeSymlink != 0 || after.Size() != int64(len(data)) {
		return value, errors.New("registry config changed while being read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return config{}, errors.New("registry config is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || value.SchemaVersion != 1 {
		return config{}, errors.New("registry config is invalid")
	}
	return value, nil
}
