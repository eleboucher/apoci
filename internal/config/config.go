package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Endpoint      string `yaml:"endpoint"`
	Name          string `yaml:"name"`
	Listen        string `yaml:"listen"`
	DataDir       string `yaml:"dataDir"`
	KeyPath       string `yaml:"keyPath"`
	LogLevel      string `yaml:"logLevel"`
	LogFormat     string `yaml:"logFormat"`
	Region        string `yaml:"region"`
	ImmutableTags string `yaml:"immutableTags"`
	RegistryToken string `yaml:"registryToken"`
	AccountDomain string `yaml:"accountDomain"`

	Database Database `yaml:"database"`
	TLS      *TLS     `yaml:"tls,omitempty"`

	Peering    Peering    `yaml:"peering"`
	Federation Federation `yaml:"federation"`
	Limits     Limits     `yaml:"limits"`
	Metrics    Metrics    `yaml:"metrics"`

	Domain string `yaml:"-"`
}

type Database struct {
	Driver       string `yaml:"driver"`       // "sqlite" (default) or "postgres"
	DSN          string `yaml:"dsn"`          // connection string; required for postgres, ignored for sqlite
	MaxOpenConns int    `yaml:"maxOpenConns"` // max open connections (0 = driver default: 4 for sqlite, 25 for postgres)
	MaxIdleConns int    `yaml:"maxIdleConns"` // max idle connections (0 = driver default: 4 for sqlite, 10 for postgres)
}

type TLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type Peering struct {
	HealthCheckInterval time.Duration `yaml:"healthCheckInterval"`
	FetchTimeout        time.Duration `yaml:"fetchTimeout"`
}

const (
	DefaultMaxManifestSize int64 = 10 * 1024 * 1024  // 10 MB
	DefaultMaxBlobSize     int64 = 512 * 1024 * 1024 // 512 MB
)

type Federation struct {
	AutoAccept     string   `yaml:"autoAccept"`     // "none" (default), "mutual", "all"
	AllowedDomains []string `yaml:"allowedDomains"` // always auto-accept from these domains
	BlockedDomains []string `yaml:"blockedDomains"` // silently drop all activities from these domains
	BlockedActors  []string `yaml:"blockedActors"`  // silently drop all activities from these actor URLs
}

type Limits struct {
	MaxManifestSize int64 `yaml:"maxManifestSize"`
	MaxBlobSize     int64 `yaml:"maxBlobSize"`
}

type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	Token   string `yaml:"token"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // config path is provided by operator
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
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
		cfg.DataDir = "/apoci/storage"
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
	if cfg.Peering.HealthCheckInterval == 0 {
		cfg.Peering.HealthCheckInterval = 30 * time.Second
	}
	if cfg.Peering.FetchTimeout == 0 {
		cfg.Peering.FetchTimeout = 60 * time.Second
	}
	if cfg.Limits.MaxManifestSize == 0 {
		cfg.Limits.MaxManifestSize = DefaultMaxManifestSize
	}
	if cfg.Limits.MaxBlobSize == 0 {
		cfg.Limits.MaxBlobSize = DefaultMaxBlobSize
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
	if cfg.RegistryToken == "" {
		token, err := loadOrGenerateToken(filepath.Join(cfg.DataDir, "registry.token"))
		if err != nil {
			return fmt.Errorf("setting up registry token: %w", err)
		}
		cfg.RegistryToken = token
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

	validDrivers := map[string]bool{"sqlite": true, "postgres": true}
	if !validDrivers[cfg.Database.Driver] {
		return fmt.Errorf("database.driver must be 'sqlite' or 'postgres'")
	}
	if cfg.Database.Driver == "postgres" && cfg.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required when driver is 'postgres'")
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[cfg.LogLevel] {
		return fmt.Errorf("logLevel must be one of: debug, info, warn, error")
	}

	validFormats := map[string]bool{"json": true, "text": true}
	if !validFormats[cfg.LogFormat] {
		return fmt.Errorf("logFormat must be 'json' or 'text'")
	}

	validAutoAccept := map[string]bool{"none": true, "mutual": true, "all": true}
	if !validAutoAccept[cfg.Federation.AutoAccept] {
		return fmt.Errorf("federation.autoAccept must be 'none', 'mutual', or 'all'")
	}

	if cfg.ImmutableTags != "" {
		if _, err := regexp.Compile(cfg.ImmutableTags); err != nil {
			return fmt.Errorf("invalid immutableTags regex: %w", err)
		}
	}

	if cfg.AccountDomain != cfg.Domain {
		if strings.Contains(cfg.AccountDomain, "/") || strings.Contains(cfg.AccountDomain, ":") {
			return fmt.Errorf("accountDomain must be a bare hostname (no scheme, port, or path)")
		}
	}

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

	return nil
}
