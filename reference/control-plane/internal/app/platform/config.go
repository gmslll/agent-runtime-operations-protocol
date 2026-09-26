package platform

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeDevelopmentMemory Mode = "development-memory"
	ModeSQLite            Mode = "sqlite"
	ModePostgres          Mode = "postgres"
)

type Config struct {
	Mode                  Mode
	DatabaseDSN           string
	MigrationRoot         string
	BackupDirectory       string
	AssetTokenKeyFile     string
	AssetTokenKeyID       string
	DispatchIssuer        string
	ListenAddress         string
	AllowNonLoopback      bool
	ReadHeaderTimeout     time.Duration
	ReadTimeout           time.Duration
	WriteTimeout          time.Duration
	IdleTimeout           time.Duration
	RequestTimeout        time.Duration
	ShutdownTimeout       time.Duration
	StorageStartupTimeout time.Duration
	MigrationTimeout      time.Duration
	MaxHeaderBytes        int
	MaxBodyBytes          int64
	AuditCapacity         int
	TraceCapacity         int
}

func DefaultConfig() Config {
	return Config{
		Mode: ModeDevelopmentMemory, ListenAddress: "127.0.0.1:8080",
		DispatchIssuer:    "https://control-plane.invalid",
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second,
		RequestTimeout: 15 * time.Second, ShutdownTimeout: 10 * time.Second,
		StorageStartupTimeout: 15 * time.Second, MigrationTimeout: 2 * time.Minute,
		MaxHeaderBytes: 1 << 20, MaxBodyBytes: 1 << 20,
		AuditCapacity: 4096, TraceCapacity: 4096,
	}
}

type configBinding struct {
	environment string
	flag        string
}

var configBindings = []configBinding{
	{"AROP_CP_MODE", "mode"}, {"AROP_CP_LISTEN", "listen"},
	{"AROP_CP_DATABASE_DSN", "database-dsn"}, {"AROP_CP_MIGRATION_ROOT", "migration-root"},
	{"AROP_CP_BACKUP_DIRECTORY", "backup-directory"},
	{"AROP_CP_ASSET_TOKEN_KEY_FILE", "asset-token-key-file"}, {"AROP_CP_ASSET_TOKEN_KEY_ID", "asset-token-key-id"},
	{"AROP_CP_DISPATCH_ISSUER", "dispatch-issuer"},
	{"AROP_CP_ALLOW_NON_LOOPBACK", "allow-non-loopback"},
	{"AROP_CP_READ_HEADER_TIMEOUT", "read-header-timeout"},
	{"AROP_CP_READ_TIMEOUT", "read-timeout"}, {"AROP_CP_WRITE_TIMEOUT", "write-timeout"},
	{"AROP_CP_IDLE_TIMEOUT", "idle-timeout"}, {"AROP_CP_REQUEST_TIMEOUT", "request-timeout"},
	{"AROP_CP_SHUTDOWN_TIMEOUT", "shutdown-timeout"},
	{"AROP_CP_STORAGE_STARTUP_TIMEOUT", "storage-startup-timeout"},
	{"AROP_CP_MIGRATION_TIMEOUT", "migration-timeout"},
	{"AROP_CP_MAX_HEADER_BYTES", "max-header-bytes"}, {"AROP_CP_MAX_BODY_BYTES", "max-body-bytes"},
	{"AROP_CP_AUDIT_CAPACITY", "audit-capacity"}, {"AROP_CP_TRACE_CAPACITY", "trace-capacity"},
}

func ParseConfig(args, environment []string) (Config, error) {
	config := DefaultConfig()
	knownEnvironment := map[string]configBinding{}
	for _, binding := range configBindings {
		knownEnvironment[binding.environment] = binding
	}
	environmentValues := map[string]string{}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || !strings.HasPrefix(key, "AROP_CP_") {
			continue
		}
		if _, known := knownEnvironment[key]; !known {
			return Config{}, fmt.Errorf("unknown Control Plane environment key %s", key)
		}
		if _, duplicate := environmentValues[key]; duplicate {
			return Config{}, fmt.Errorf("duplicate Control Plane environment key %s", key)
		}
		environmentValues[key] = value
	}
	flagOccurrences, err := scanFlags(args)
	if err != nil {
		return Config{}, err
	}
	for _, binding := range configBindings {
		if _, fromEnvironment := environmentValues[binding.environment]; fromEnvironment && flagOccurrences[binding.flag] != 0 {
			return Config{}, fmt.Errorf("configuration key %s was supplied by both environment and flag", binding.environment)
		}
	}
	if err := applyEnvironment(&config, environmentValues); err != nil {
		return Config{}, err
	}
	flags := flag.NewFlagSet("aropd", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := string(config.Mode)
	flags.StringVar(&mode, "mode", mode, "runtime mode")
	flags.StringVar(&config.DatabaseDSN, "database-dsn", config.DatabaseDSN, "database DSN or absolute SQLite path")
	flags.StringVar(&config.MigrationRoot, "migration-root", config.MigrationRoot, "absolute migration catalog root")
	flags.StringVar(&config.BackupDirectory, "backup-directory", config.BackupDirectory, "private durable backup directory")
	flags.StringVar(&config.AssetTokenKeyFile, "asset-token-key-file", config.AssetTokenKeyFile, "absolute private 32-byte asset token key file")
	flags.StringVar(&config.AssetTokenKeyID, "asset-token-key-id", config.AssetTokenKeyID, "asset token key identifier")
	flags.StringVar(&config.DispatchIssuer, "dispatch-issuer", config.DispatchIssuer, "canonical HTTPS issuer origin for dispatch tickets")
	flags.StringVar(&config.ListenAddress, "listen", config.ListenAddress, "HTTP listen address")
	flags.BoolVar(&config.AllowNonLoopback, "allow-non-loopback", config.AllowNonLoopback, "allow an explicit non-loopback bind")
	flags.DurationVar(&config.ReadHeaderTimeout, "read-header-timeout", config.ReadHeaderTimeout, "HTTP read-header timeout")
	flags.DurationVar(&config.ReadTimeout, "read-timeout", config.ReadTimeout, "HTTP read timeout")
	flags.DurationVar(&config.WriteTimeout, "write-timeout", config.WriteTimeout, "HTTP write timeout")
	flags.DurationVar(&config.IdleTimeout, "idle-timeout", config.IdleTimeout, "HTTP idle timeout")
	flags.DurationVar(&config.RequestTimeout, "request-timeout", config.RequestTimeout, "per-request timeout")
	flags.DurationVar(&config.ShutdownTimeout, "shutdown-timeout", config.ShutdownTimeout, "graceful shutdown timeout")
	flags.DurationVar(&config.StorageStartupTimeout, "storage-startup-timeout", config.StorageStartupTimeout, "storage startup timeout")
	flags.DurationVar(&config.MigrationTimeout, "migration-timeout", config.MigrationTimeout, "storage migration timeout")
	flags.IntVar(&config.MaxHeaderBytes, "max-header-bytes", config.MaxHeaderBytes, "maximum header bytes")
	flags.Int64Var(&config.MaxBodyBytes, "max-body-bytes", config.MaxBodyBytes, "maximum request body bytes")
	flags.IntVar(&config.AuditCapacity, "audit-capacity", config.AuditCapacity, "ephemeral audit capacity")
	flags.IntVar(&config.TraceCapacity, "trace-capacity", config.TraceCapacity, "ephemeral trace capacity")
	if err := flags.Parse(args); err != nil {
		return Config{}, errors.New("invalid Control Plane flag")
	}
	if flags.NArg() != 0 {
		return Config{}, errors.New("positional arguments are not supported")
	}
	config.Mode = Mode(mode)
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) Validate() error {
	if (config.AssetTokenKeyFile == "") != (config.AssetTokenKeyID == "") {
		return errors.New("asset token key file and key id must be configured together")
	}
	if config.AssetTokenKeyFile != "" && (!filepath.IsAbs(config.AssetTokenKeyFile) || filepath.Clean(config.AssetTokenKeyFile) != config.AssetTokenKeyFile) {
		return errors.New("asset token key file must be an absolute clean path")
	}
	if config.AssetTokenKeyID != "" {
		matched, _ := regexp.MatchString(`^atk_[A-Za-z0-9._-]{1,60}$`, config.AssetTokenKeyID)
		if !matched {
			return errors.New("asset token key id is invalid")
		}
	}
	issuer, err := url.Parse(config.DispatchIssuer)
	if err != nil || len(config.DispatchIssuer) > 2048 || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.Path != "" || issuer.RawQuery != "" || issuer.Fragment != "" || strings.HasSuffix(config.DispatchIssuer, "/") {
		return errors.New("dispatch issuer must be a canonical HTTPS origin")
	}
	if config.Mode != ModeDevelopmentMemory && config.Mode != ModeSQLite && config.Mode != ModePostgres {
		return errors.New("unsupported Control Plane mode")
	}
	switch config.Mode {
	case ModeDevelopmentMemory:
		if config.DatabaseDSN != "" || config.MigrationRoot != "" || config.BackupDirectory != "" {
			return errors.New("development-memory mode rejects database configuration")
		}
	case ModeSQLite:
		if config.DatabaseDSN == "" || !filepath.IsAbs(config.DatabaseDSN) || filepath.Clean(config.DatabaseDSN) != config.DatabaseDSN {
			return errors.New("sqlite mode requires an absolute clean database path")
		}
		if err := validateMigrationRoot(config.MigrationRoot); err != nil {
			return err
		}
		if err := validateBackupDirectory(config.BackupDirectory); err != nil {
			return err
		}
	case ModePostgres:
		parsed, err := url.Parse(config.DatabaseDSN)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" {
			return errors.New("postgres mode requires an explicit postgres URL")
		}
		if err := validateMigrationRoot(config.MigrationRoot); err != nil {
			return err
		}
		if err := validateBackupDirectory(config.BackupDirectory); err != nil {
			return err
		}
	}
	host, portText, err := net.SplitHostPort(config.ListenAddress)
	if err != nil || host == "" || portText == "" {
		return errors.New("listen address must be an explicit host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("listen port is invalid")
	}
	ip := net.ParseIP(host)
	if ip == nil && host != "localhost" {
		return errors.New("listen host must be localhost or an IP literal")
	}
	loopback := host == "localhost" || ip != nil && ip.IsLoopback()
	if !loopback && !config.AllowNonLoopback {
		return errors.New("non-loopback listen requires explicit opt-in")
	}
	for name, duration := range map[string]time.Duration{
		"read-header-timeout": config.ReadHeaderTimeout, "read-timeout": config.ReadTimeout,
		"write-timeout": config.WriteTimeout, "idle-timeout": config.IdleTimeout,
		"request-timeout": config.RequestTimeout, "shutdown-timeout": config.ShutdownTimeout,
		"storage-startup-timeout": config.StorageStartupTimeout, "migration-timeout": config.MigrationTimeout,
	} {
		if duration < time.Millisecond || duration > time.Hour {
			return fmt.Errorf("%s must be within 1ms..1h", name)
		}
	}
	if config.MaxHeaderBytes < 1024 || config.MaxHeaderBytes > 16<<20 {
		return errors.New("max-header-bytes is outside the safe range")
	}
	if config.MaxBodyBytes < 1 || config.MaxBodyBytes > 128<<20 {
		return errors.New("max-body-bytes is outside the safe range")
	}
	if config.AuditCapacity < 1 || config.AuditCapacity > 1_000_000 || config.TraceCapacity < 1 || config.TraceCapacity > 1_000_000 {
		return errors.New("observability capacity is outside the safe range")
	}
	if config.AuditCapacity != config.TraceCapacity {
		return errors.New("audit and trace capacities must be equal")
	}
	return nil
}

func scanFlags(args []string) (map[string]int, error) {
	known := map[string]bool{}
	boolean := map[string]bool{"allow-non-loopback": true}
	for _, binding := range configBindings {
		known[binding.flag] = true
	}
	seen := map[string]int{}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "--") || argument == "--" {
			return nil, errors.New("only explicit long flags are supported")
		}
		nameValue := strings.TrimPrefix(argument, "--")
		name, _, hasInlineValue := strings.Cut(nameValue, "=")
		if !known[name] {
			return nil, fmt.Errorf("unknown Control Plane flag --%s", name)
		}
		seen[name]++
		if seen[name] > 1 {
			return nil, fmt.Errorf("duplicate Control Plane flag --%s", name)
		}
		if !hasInlineValue && !boolean[name] {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return nil, fmt.Errorf("missing value for Control Plane flag --%s", name)
			}
			index++
		}
	}
	return seen, nil
}

func applyEnvironment(config *Config, values map[string]string) error {
	setDuration := func(key string, target *time.Duration) error {
		if value, ok := values[key]; ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("invalid duration for %s", key)
			}
			*target = parsed
		}
		return nil
	}
	if value, ok := values["AROP_CP_MODE"]; ok {
		config.Mode = Mode(value)
	}
	if value, ok := values["AROP_CP_DATABASE_DSN"]; ok {
		config.DatabaseDSN = value
	}
	if value, ok := values["AROP_CP_MIGRATION_ROOT"]; ok {
		config.MigrationRoot = value
	}
	if value, ok := values["AROP_CP_BACKUP_DIRECTORY"]; ok {
		config.BackupDirectory = value
	}
	if value, ok := values["AROP_CP_LISTEN"]; ok {
		config.ListenAddress = value
	}
	if value, ok := values["AROP_CP_DISPATCH_ISSUER"]; ok {
		config.DispatchIssuer = value
	}
	if value, ok := values["AROP_CP_ALLOW_NON_LOOPBACK"]; ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("invalid boolean for AROP_CP_ALLOW_NON_LOOPBACK")
		}
		config.AllowNonLoopback = parsed
	}
	for key, target := range map[string]*time.Duration{
		"AROP_CP_READ_HEADER_TIMEOUT": &config.ReadHeaderTimeout, "AROP_CP_READ_TIMEOUT": &config.ReadTimeout,
		"AROP_CP_WRITE_TIMEOUT": &config.WriteTimeout, "AROP_CP_IDLE_TIMEOUT": &config.IdleTimeout,
		"AROP_CP_REQUEST_TIMEOUT": &config.RequestTimeout, "AROP_CP_SHUTDOWN_TIMEOUT": &config.ShutdownTimeout,
		"AROP_CP_STORAGE_STARTUP_TIMEOUT": &config.StorageStartupTimeout, "AROP_CP_MIGRATION_TIMEOUT": &config.MigrationTimeout,
	} {
		if err := setDuration(key, target); err != nil {
			return err
		}
	}
	setInt := func(key string, target *int) error {
		if value, ok := values[key]; ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid integer for %s", key)
			}
			*target = parsed
		}
		return nil
	}
	if err := setInt("AROP_CP_MAX_HEADER_BYTES", &config.MaxHeaderBytes); err != nil {
		return err
	}
	if err := setInt("AROP_CP_AUDIT_CAPACITY", &config.AuditCapacity); err != nil {
		return err
	}
	if err := setInt("AROP_CP_TRACE_CAPACITY", &config.TraceCapacity); err != nil {
		return err
	}
	if value, ok := values["AROP_CP_MAX_BODY_BYTES"]; ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return errors.New("invalid integer for AROP_CP_MAX_BODY_BYTES")
		}
		config.MaxBodyBytes = parsed
	}
	return nil
}

func validateMigrationRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("durable mode requires an absolute clean migration root")
	}
	return nil
}

func validateBackupDirectory(directory string) error {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("durable mode requires an absolute clean backup directory")
	}
	return nil
}
