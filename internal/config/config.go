package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	cenv "github.com/caarlos0/env/v11"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Endpoint      string `yaml:"endpoint"      env:"APOCI_ENDPOINT"`
	Name          string `yaml:"name"          env:"APOCI_NAME"`
	Listen        string `yaml:"listen"        env:"APOCI_LISTEN"`
	DataDir       string `yaml:"dataDir"       env:"APOCI_DATA_DIR"`
	KeyPath       string `yaml:"keyPath"       env:"APOCI_KEY_PATH"`
	LogLevel      string `yaml:"logLevel"      env:"APOCI_LOG_LEVEL"`
	LogFormat     string `yaml:"logFormat"     env:"APOCI_LOG_FORMAT"`
	ImmutableTags string `yaml:"immutableTags" env:"APOCI_IMMUTABLE_TAGS"`
	RegistryToken string `yaml:"registryToken" env:"APOCI_REGISTRY_TOKEN"`
	AdminToken    string `yaml:"adminToken"    env:"APOCI_ADMIN_TOKEN"`
	AccountDomain string `yaml:"accountDomain" env:"APOCI_ACCOUNT_DOMAIN"`

	Database Database `yaml:"database"   envPrefix:"APOCI_DB_"`
	Storage  Storage  `yaml:"storage"    envPrefix:"APOCI_STORAGE_"`
	TLS      *TLS     `yaml:"tls,omitempty"`

	Peering       Peering       `yaml:"peering"    envPrefix:"APOCI_PEERING_"`
	Federation    Federation    `yaml:"federation" envPrefix:"APOCI_FEDERATION_"`
	Limits        Limits        `yaml:"limits"     envPrefix:"APOCI_"`
	Metrics       Metrics       `yaml:"metrics"       envPrefix:"APOCI_METRICS_"`
	GC            GC            `yaml:"gc"            envPrefix:"APOCI_GC_"`
	Notifications Notifications `yaml:"notifications" envPrefix:"APOCI_NOTIFICATIONS_"`
	Upstreams     Upstreams     `yaml:"upstreams"     envPrefix:"APOCI_UPSTREAMS_"`

	Domain string `yaml:"-" env:"-"`
}

type Storage struct {
	Type string    `yaml:"type" env:"TYPE"` // "local" (default) or "s3"
	S3   S3Storage `yaml:"s3"   envPrefix:"S3_"`
}

type S3Storage struct {
	Bucket         string `yaml:"bucket"         env:"BUCKET"`
	Region         string `yaml:"region"         env:"REGION"`
	Endpoint       string `yaml:"endpoint"       env:"ENDPOINT"` // custom endpoint for S3-compatible stores (e.g. MinIO)
	AccessKey      string `yaml:"accessKey"      env:"ACCESS_KEY"`
	SecretKey      string `yaml:"secretKey"      env:"SECRET_KEY"`
	Prefix         string `yaml:"prefix"         env:"PREFIX"`           // key prefix inside the bucket
	ForcePathStyle bool   `yaml:"forcePathStyle" env:"FORCE_PATH_STYLE"` // required for MinIO and some compatible stores
	TempDir        string `yaml:"tempDir"        env:"TEMP_DIR"`         // upload staging dir; defaults to os.TempDir()
}

type Database struct {
	Driver       string `yaml:"driver"       env:"DRIVER"`         // "sqlite" (default) or "postgres"
	DSN          string `yaml:"dsn"          env:"DSN"`            // connection string; required for postgres, ignored for sqlite
	MaxOpenConns int    `yaml:"maxOpenConns" env:"MAX_OPEN_CONNS"` // max open connections (0 = driver default: 4 for sqlite, 25 for postgres)
	MaxIdleConns int    `yaml:"maxIdleConns" env:"MAX_IDLE_CONNS"` // max idle connections (0 = driver default: 4 for sqlite, 10 for postgres)
}

type TLS struct {
	Cert string `yaml:"cert" env:"APOCI_TLS_CERT"`
	Key  string `yaml:"key"  env:"APOCI_TLS_KEY"`
}

type Peering struct {
	HealthCheckInterval time.Duration `yaml:"healthCheckInterval" env:"HEALTH_CHECK_INTERVAL"`
	FetchTimeout        time.Duration `yaml:"fetchTimeout"        env:"FETCH_TIMEOUT"`
}

const (
	DefaultMaxManifestSize int64 = 10 * 1024 * 1024  // 10 MB
	DefaultMaxBlobSize     int64 = 512 * 1024 * 1024 // 512 MB
)

type Federation struct {
	AutoAccept        string   `yaml:"autoAccept"        env:"AUTO_ACCEPT"`                      // "none" (default), "mutual", "all"
	AllowInsecureHTTP bool     `yaml:"allowInsecureHTTP" env:"ALLOW_INSECURE_HTTP"`              // allow plain HTTP federation (testing only)
	AllowedDomains    []string `yaml:"allowedDomains"    env:"ALLOWED_DOMAINS" envSeparator:","` // always auto-accept from these domains
	BlockedDomains    []string `yaml:"blockedDomains"    env:"BLOCKED_DOMAINS" envSeparator:","` // silently drop all activities from these domains
	BlockedActors     []string `yaml:"blockedActors"     env:"BLOCKED_ACTORS"  envSeparator:","` // silently drop all activities from these actor URLs
}

type Limits struct {
	MaxManifestSize int64 `yaml:"maxManifestSize" env:"MAX_MANIFEST_SIZE"`
	MaxBlobSize     int64 `yaml:"maxBlobSize"     env:"MAX_BLOB_SIZE"`
}

type Metrics struct {
	Enabled bool   `yaml:"enabled" env:"ENABLED"`
	Listen  string `yaml:"listen"  env:"LISTEN"`
	Token   string `yaml:"token"   env:"TOKEN"`
}

type Notifications struct {
	URLs   []string `yaml:"urls"   env:"URLS"   envSeparator:","`
	Events []string `yaml:"events" env:"EVENTS" envSeparator:","`
}

type GC struct {
	Enabled          *bool         `yaml:"enabled"          env:"ENABLED"`
	Interval         time.Duration `yaml:"interval"         env:"INTERVAL"`
	StalePeerBlobAge time.Duration `yaml:"stalePeerBlobAge" env:"STALE_PEER_BLOB_AGE"`
	OrphanBatchSize  int           `yaml:"orphanBatchSize"  env:"ORPHAN_BATCH_SIZE"`
}

// Upstream configures an external OCI registry for pull-through caching.
type Upstream struct {
	Name     string `yaml:"name"     env:"NAME"`     // registry name, e.g. "docker.io"
	Endpoint string `yaml:"endpoint" env:"ENDPOINT"` // registry URL, e.g. "https://registry-1.docker.io"
	Auth     string `yaml:"auth"     env:"AUTH"`     // "none", "basic", or "token"
	Username string `yaml:"username" env:"USERNAME"` // for basic/token auth
	Password string `yaml:"password" env:"PASSWORD"` // for basic/token auth
}

// String returns a string representation with the password redacted.
func (u Upstream) String() string {
	pass := ""
	if u.Password != "" {
		pass = "[REDACTED]"
	}
	return fmt.Sprintf("Upstream{Name:%s Endpoint:%s Auth:%s Username:%s Password:%s}",
		u.Name, u.Endpoint, u.Auth, u.Username, pass)
}

// Upstreams holds configuration for upstream registry proxying.
type Upstreams struct {
	Enabled      bool          `yaml:"enabled"      env:"ENABLED"`
	FetchTimeout time.Duration `yaml:"fetchTimeout" env:"FETCH_TIMEOUT"`
	Registries   []Upstream    `yaml:"registries"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(path) //nolint:gosec // config path is provided by operator
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		// No config file — will rely on env vars and defaults.
	} else {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	// Env vars override YAML values.
	if err := cenv.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parsing environment variables: %w", err)
	}

	// Handle TLS env vars — the pointer may be nil from YAML.
	if cfg.TLS == nil {
		if os.Getenv("APOCI_TLS_CERT") != "" || os.Getenv("APOCI_TLS_KEY") != "" {
			cfg.TLS = &TLS{}
			if err := cenv.Parse(cfg.TLS); err != nil {
				return nil, fmt.Errorf("parsing TLS environment variables: %w", err)
			}
		}
	}

	if err := applyDefaults(cfg); err != nil {
		return nil, err
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) error {
	if err := applyServerDefaults(cfg); err != nil {
		return err
	}
	applyPeeringDefaults(cfg)
	applyLimitsDefaults(cfg)
	applyGCDefaults(cfg)
	applyUpstreamDefaults(cfg)
	return applyTokenDefaults(cfg)
}

func applyServerDefaults(cfg *Config) error {
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil {
			return fmt.Errorf("invalid endpoint URL: %w", err)
		}
		cfg.Domain = u.Hostname()
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Domain
	}
	if cfg.Listen == "" {
		cfg.Listen = ":5000"
	}
	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("determining home directory: %w", err)
		}
		cfg.DataDir = filepath.Join(home, ".apoci")
	}
	if cfg.KeyPath == "" {
		cfg.KeyPath = filepath.Join(cfg.DataDir, "ap.key")
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = "json"
	}
	if cfg.Storage.Type == "" {
		cfg.Storage.Type = "local"
	}
	if cfg.Database.Driver == "" {
		cfg.Database.Driver = "sqlite"
	}
	if cfg.Metrics.Listen == "" {
		cfg.Metrics.Listen = ":9090"
	}
	if cfg.Federation.AutoAccept == "" {
		cfg.Federation.AutoAccept = "none"
	}
	if cfg.AccountDomain == "" {
		cfg.AccountDomain = cfg.Domain
	}
	if cfg.ImmutableTags == "" {
		cfg.ImmutableTags = `^v[0-9]`
	}
	return nil
}

func applyPeeringDefaults(cfg *Config) {
	if cfg.Peering.HealthCheckInterval == 0 {
		cfg.Peering.HealthCheckInterval = 30 * time.Second
	}
	if cfg.Peering.FetchTimeout == 0 {
		cfg.Peering.FetchTimeout = 60 * time.Second
	}
}

func applyLimitsDefaults(cfg *Config) {
	if cfg.Limits.MaxManifestSize == 0 {
		cfg.Limits.MaxManifestSize = DefaultMaxManifestSize
	}
	if cfg.Limits.MaxBlobSize == 0 {
		cfg.Limits.MaxBlobSize = DefaultMaxBlobSize
	}
}

func applyGCDefaults(cfg *Config) {
	if cfg.GC.Enabled == nil {
		t := true
		cfg.GC.Enabled = &t
	}
	if cfg.GC.Interval == 0 {
		cfg.GC.Interval = 6 * time.Hour
	}
	if cfg.GC.StalePeerBlobAge == 0 {
		cfg.GC.StalePeerBlobAge = 30 * 24 * time.Hour
	}
	if cfg.GC.OrphanBatchSize == 0 {
		cfg.GC.OrphanBatchSize = 500
	}
}

func applyUpstreamDefaults(cfg *Config) {
	if cfg.Upstreams.FetchTimeout == 0 {
		cfg.Upstreams.FetchTimeout = 60 * time.Second
	}
	for i := range cfg.Upstreams.Registries {
		if cfg.Upstreams.Registries[i].Auth == "" {
			cfg.Upstreams.Registries[i].Auth = "none"
		}
	}
}

func applyTokenDefaults(cfg *Config) error {
	if cfg.RegistryToken == "" {
		token, err := loadOrGenerateToken(filepath.Join(cfg.DataDir, "registry.token"))
		if err != nil {
			return fmt.Errorf("setting up registry token: %w", err)
		}
		cfg.RegistryToken = token
	}
	if cfg.AdminToken == "" {
		token, err := loadOrGenerateToken(filepath.Join(cfg.DataDir, "admin.token"))
		if err != nil {
			return fmt.Errorf("setting up admin token: %w", err)
		}
		cfg.AdminToken = token
	}
	return nil
}

func loadOrGenerateToken(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-controlled path
	if err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("creating directory for token: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	token := hex.EncodeToString(buf)

	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing token file: %w", err)
	}

	return token, nil
}

func validate(cfg *Config) error {
	if err := validateEndpoint(cfg); err != nil {
		return err
	}
	if err := validateStorage(cfg); err != nil {
		return err
	}
	if err := validateDatabase(cfg); err != nil {
		return err
	}
	if err := validateLogging(cfg); err != nil {
		return err
	}
	if err := validateFederation(cfg); err != nil {
		return err
	}
	if err := validateRegex(cfg); err != nil {
		return err
	}
	if err := validateAccountDomain(cfg); err != nil {
		return err
	}
	if err := validateNonNegative(cfg); err != nil {
		return err
	}
	if err := validateNotificationEvents(cfg); err != nil {
		return err
	}
	if err := validateUpstreams(cfg); err != nil {
		return err
	}
	return nil
}

func validateEndpoint(cfg *Config) error {
	if cfg.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if cfg.Domain == "" {
		return fmt.Errorf("could not derive domain from endpoint")
	}
	endpointScheme := strings.ToLower(strings.SplitN(cfg.Endpoint, "://", 2)[0])
	if endpointScheme != "https" && endpointScheme != "http" {
		return fmt.Errorf("endpoint scheme must be 'https' or 'http', got %q", endpointScheme)
	}
	return nil
}

func validateStorage(cfg *Config) error {
	validStorageTypes := map[string]bool{"local": true, "s3": true}
	if !validStorageTypes[cfg.Storage.Type] {
		return fmt.Errorf("storage.type must be 'local' or 's3'")
	}
	if cfg.Storage.Type == "s3" {
		if cfg.Storage.S3.Bucket == "" {
			return fmt.Errorf("storage.s3.bucket is required when storage.type is 's3'")
		}
		if cfg.Storage.S3.Region == "" && cfg.Storage.S3.Endpoint == "" {
			return fmt.Errorf("storage.s3.region is required when no custom endpoint is configured")
		}
		if cfg.Storage.S3.Endpoint != "" {
			if _, err := url.ParseRequestURI(cfg.Storage.S3.Endpoint); err != nil {
				return fmt.Errorf("storage.s3.endpoint is not a valid URL: %w", err)
			}
		}
	}
	return nil
}

func validateDatabase(cfg *Config) error {
	validDrivers := map[string]bool{"sqlite": true, "postgres": true}
	if !validDrivers[cfg.Database.Driver] {
		return fmt.Errorf("database.driver must be 'sqlite' or 'postgres'")
	}
	if cfg.Database.Driver == "postgres" && cfg.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required when driver is 'postgres'")
	}
	return nil
}

func validateLogging(cfg *Config) error {
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[cfg.LogLevel] {
		return fmt.Errorf("logLevel must be one of: debug, info, warn, error")
	}
	validFormats := map[string]bool{"json": true, "text": true}
	if !validFormats[cfg.LogFormat] {
		return fmt.Errorf("logFormat must be 'json' or 'text'")
	}
	return nil
}

func validateFederation(cfg *Config) error {
	validAutoAccept := map[string]bool{"none": true, "mutual": true, "all": true}
	if !validAutoAccept[cfg.Federation.AutoAccept] {
		return fmt.Errorf("federation.autoAccept must be 'none', 'mutual', or 'all'")
	}
	return nil
}

func validateRegex(cfg *Config) error {
	if cfg.ImmutableTags != "" {
		if _, err := regexp.Compile(cfg.ImmutableTags); err != nil {
			return fmt.Errorf("invalid immutableTags regex: %w", err)
		}
	}
	return nil
}

func validateAccountDomain(cfg *Config) error {
	if cfg.AccountDomain != cfg.Domain {
		if strings.Contains(cfg.AccountDomain, "/") || strings.Contains(cfg.AccountDomain, ":") {
			return fmt.Errorf("accountDomain must be a bare hostname (no scheme, port, or path)")
		}
	}
	return nil
}

func validateNonNegative(cfg *Config) error {
	if cfg.Limits.MaxManifestSize < 0 {
		return fmt.Errorf("limits.maxManifestSize must not be negative")
	}
	if cfg.Limits.MaxBlobSize < 0 {
		return fmt.Errorf("limits.maxBlobSize must not be negative")
	}
	if cfg.Peering.HealthCheckInterval < 0 {
		return fmt.Errorf("peering.healthCheckInterval must not be negative")
	}
	if cfg.Peering.FetchTimeout < 0 {
		return fmt.Errorf("peering.fetchTimeout must not be negative")
	}
	if cfg.GC.Interval < 0 {
		return fmt.Errorf("gc.interval must not be negative")
	}
	if cfg.GC.StalePeerBlobAge < 0 {
		return fmt.Errorf("gc.stalePeerBlobAge must not be negative")
	}
	if cfg.GC.OrphanBatchSize < 0 {
		return fmt.Errorf("gc.orphanBatchSize must not be negative")
	}
	return nil
}

func validateNotificationEvents(cfg *Config) error {
	validNotificationEvents := map[string]bool{
		"peer_health":         true,
		"follow_request":      true,
		"replication_failure": true,
		"gc_error":            true,
	}
	for _, e := range cfg.Notifications.Events {
		if !validNotificationEvents[e] {
			return fmt.Errorf("notifications.events: unknown event %q", e)
		}
	}
	return nil
}

func validateUpstreams(cfg *Config) error {
	if !cfg.Upstreams.Enabled {
		return nil
	}
	validAuthTypes := map[string]bool{"none": true, "basic": true, "token": true}
	seen := make(map[string]bool)
	for i, u := range cfg.Upstreams.Registries {
		if u.Name == "" {
			return fmt.Errorf("upstreams.registries[%d].name is required", i)
		}
		if u.Endpoint == "" {
			return fmt.Errorf("upstreams.registries[%d].endpoint is required", i)
		}
		if _, err := url.ParseRequestURI(u.Endpoint); err != nil {
			return fmt.Errorf("upstreams.registries[%d].endpoint is not a valid URL: %w", i, err)
		}
		if !validAuthTypes[u.Auth] {
			return fmt.Errorf("upstreams.registries[%d].auth must be 'none', 'basic', or 'token'", i)
		}
		if u.Auth == "basic" && (u.Username == "" || u.Password == "") {
			return fmt.Errorf("upstreams.registries[%d] requires username and password for basic auth", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("upstreams.registries: duplicate registry name %q", u.Name)
		}
		seen[u.Name] = true
	}
	if cfg.Upstreams.FetchTimeout < 0 {
		return fmt.Errorf("upstreams.fetchTimeout must not be negative")
	}
	return nil
}
