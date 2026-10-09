// Package config holds the console's runtime configuration, loaded from the
// environment with sensible defaults.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultIncusSocket is where the Incus daemon listens on a standard
// installation.
const DefaultIncusSocket = "/var/lib/incus/unix.socket"

// Config is the fully resolved application configuration.
type Config struct {
	// HTTP
	Addr          string
	BaseURL       string
	SecureCookies bool

	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is
	// believed. From any other peer the header is ignored, since a client can
	// write whatever it likes into it.
	TrustedProxies []netip.Prefix

	// Storage
	DataDir    string
	DBPath     string
	SessionTTL int // hours

	// Incus
	IncusSocket  string
	IncusProject string

	// Caddy
	CaddyConfigPath string
	CaddyReloadCmd  string
	CaddyAdminURL   string

	// Backups (S3-compatible target)
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3Region    string
	S3Prefix    string
	S3UseSSL    bool

	// Bootstrap
	AdminUser     string
	AdminPassword string

	// SSOKey encrypts OIDC client secrets at rest. It is derived from
	// CONSOLE_SSO_KEY: 64 hex characters (32 bytes) or any passphrase
	// (SHA-256). Empty means provider secrets cannot be saved.
	SSOKey []byte

	// SecretKey seals TOTP seeds at rest (CONSOLE_SECRET_KEY). Empty means a
	// generated key file in the data directory is used.
	SecretKey string

	// MetricsToken enables GET /metrics (Prometheus) for a scraper that
	// presents it as a bearer token. Empty leaves the endpoint off.
	MetricsToken string
	// AlertWebhookURL receives a JSON post when a background job fails.
	AlertWebhookURL string

	// Behaviour
	MetricsIntervalSeconds int
	MetricsRetentionHours  int
	MaxInstances           int

	// SnapshotIntervalHours is how often every instance gets an automatic
	// snapshot; 0 turns the scheduler off. SnapshotKeep is how many automatic
	// snapshots are retained per instance.
	SnapshotIntervalHours int
	SnapshotKeep          int

	// EnvFile is the .env file that was loaded, if any.
	EnvFile string
}

// Load reads configuration from the environment.
//
// A .env file is loaded first when one is present, so a single file can
// describe a deployment. Real environment variables always win over the file,
// which keeps `CONSOLE_ADDR=... ./cloud-console` and systemd overrides working.
func Load() (*Config, error) {
	envFile, err := loadEnvFile()
	if err != nil {
		return nil, err
	}

	dataDir := env("CONSOLE_DATA_DIR", "./data")

	baseURL := env("CONSOLE_BASE_URL", "http://localhost:8080")

	trusted, err := ParseTrustedProxies(env("CONSOLE_TRUSTED_PROXIES", DefaultTrustedProxies))
	if err != nil {
		return nil, fmt.Errorf("CONSOLE_TRUSTED_PROXIES: %w", err)
	}

	c := &Config{
		Addr:          env("CONSOLE_ADDR", DefaultAddr),
		BaseURL:       baseURL,
		SecureCookies: envBool("CONSOLE_SECURE_COOKIES", strings.HasPrefix(strings.ToLower(baseURL), "https://")),

		TrustedProxies: trusted,

		DataDir:    dataDir,
		DBPath:     env("CONSOLE_DB_PATH", filepath.Join(dataDir, "console.db")),
		SessionTTL: envInt("CONSOLE_SESSION_TTL_HOURS", 24*7),

		IncusSocket:  env("INCUS_SOCKET", DefaultIncusSocket),
		IncusProject: env("INCUS_PROJECT", ""),

		CaddyConfigPath: env("CADDY_CONFIG_PATH", ""),
		CaddyReloadCmd:  env("CADDY_RELOAD_CMD", ""),
		CaddyAdminURL:   env("CADDY_ADMIN_URL", ""),

		S3Endpoint:  env("S3_ENDPOINT", ""),
		S3AccessKey: env("S3_ACCESS_KEY", ""),
		S3SecretKey: env("S3_SECRET_KEY", ""),
		S3Bucket:    env("S3_BUCKET", ""),
		S3Region:    env("S3_REGION", "us-east-1"),
		S3Prefix:    env("S3_PREFIX", "peaceful-cloud"),
		S3UseSSL:    envBool("S3_USE_SSL", true),

		AdminUser:     env("CONSOLE_ADMIN_USER", "admin"),
		AdminPassword: env("CONSOLE_ADMIN_PASSWORD", ""),

		SSOKey:          ssoKey(env("CONSOLE_SSO_KEY", "")),
		SecretKey:       env("CONSOLE_SECRET_KEY", ""),
		MetricsToken:    env("CONSOLE_METRICS_TOKEN", ""),
		AlertWebhookURL: env("CONSOLE_ALERT_WEBHOOK_URL", ""),

		MetricsIntervalSeconds: envInt("CONSOLE_METRICS_INTERVAL", 60),
		MetricsRetentionHours:  envInt("CONSOLE_METRICS_RETENTION_HOURS", 24*30),
		MaxInstances:           envInt("CONSOLE_MAX_INSTANCES", 100),
		SnapshotIntervalHours:  envInt("CONSOLE_SNAPSHOT_INTERVAL_HOURS", 0),
		SnapshotKeep:           envInt("CONSOLE_SNAPSHOT_KEEP", 7),

		EnvFile: envFile,
	}

	if c.SessionTTL <= 0 {
		c.SessionTTL = 24 * 7
	}
	if c.MetricsIntervalSeconds < 10 {
		c.MetricsIntervalSeconds = 10
	}
	if c.SnapshotIntervalHours < 0 {
		c.SnapshotIntervalHours = 0
	}
	if c.SnapshotKeep < 1 {
		c.SnapshotKeep = 1
	}
	if c.MetricsRetentionHours < 1 {
		c.MetricsRetentionHours = 1
	}

	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return c, nil
}

// DefaultAddr keeps a fresh install off the network: the console is meant to
// sit behind Caddy or another proxy on the same host.
const DefaultAddr = "127.0.0.1:8080"

// Warnings lists risky settings worth reporting once at startup.
func (c *Config) Warnings() []string {
	var out []string

	https := strings.HasPrefix(strings.ToLower(c.BaseURL), "https://")
	if https && !c.SecureCookies {
		out = append(out, "CONSOLE_BASE_URL is https but CONSOLE_SECURE_COOKIES=false: session cookies will be sent over plain HTTP too")
	}
	if !https && !isLoopbackAddr(c.Addr) {
		out = append(out, "the console listens on "+c.Addr+" without HTTPS: passwords and session cookies cross the network in clear text. Bind to 127.0.0.1 behind Caddy, or serve it over HTTPS")
	}
	if c.MetricsToken != "" && len(c.MetricsToken) < 16 {
		out = append(out, "CONSOLE_METRICS_TOKEN is shorter than 16 characters: use a long random value (openssl rand -hex 24)")
	}
	return out
}

// isLoopbackAddr reports whether a listen address only accepts local
// connections. A bare ":port" listens on every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// DefaultTrustedProxies trusts a proxy on the same host, which is how the
// console's own Caddy reaches it.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128"

// ParseTrustedProxies reads a comma-separated list of IP addresses and CIDR
// ranges. "none" (or an empty list) trusts no proxy.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" || strings.EqualFold(item, "none") {
			continue
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR range", item)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// S3Configured reports whether an S3-compatible target is fully configured.
func (c *Config) S3Configured() bool {
	return c.S3Endpoint != "" && c.S3AccessKey != "" && c.S3SecretKey != "" && c.S3Bucket != ""
}

// CaddyConfigured reports whether the console should manage a Caddyfile.
func (c *Config) CaddyConfigured() bool {
	return c.CaddyConfigPath != "" || c.CaddyAdminURL != ""
}

// SSOKeySet reports whether OIDC client-secret encryption is available.
func (c *Config) SSOKeySet() bool { return len(c.SSOKey) == 32 }

// ssoKey derives a 32-byte encryption key from CONSOLE_SSO_KEY: 64 hex
// characters are used directly, any other non-empty value is SHA-256 hashed.
func ssoKey(raw string) []byte {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// loadEnvFile reads .env (or CONSOLE_ENV_FILE) into the process environment.
//
// It returns the path that was used, or "" when no file was loaded. A missing
// default .env is not an error; a missing file that was asked for explicitly is.
func loadEnvFile() (string, error) {
	path := strings.TrimSpace(os.Getenv("CONSOLE_ENV_FILE"))
	explicit := path != ""
	if !explicit {
		path = ".env"
	}

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return "", nil
		}
		return "", fmt.Errorf("read env file %s: %w", path, err)
	}

	for key, value := range parseEnvFile(string(body)) {
		// Never override a variable that is already set: the real environment
		// wins so systemd units and one-off overrides behave as expected.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return "", fmt.Errorf("set %s from %s: %w", key, path, err)
		}
	}

	return path, nil
}

// parseEnvFile parses dotenv-style KEY=value lines.
//
// It accepts blank lines, `#` comments and an optional `export ` prefix, and
// strips one pair of matching surrounding quotes. Variable interpolation is
// deliberately not supported: the console's values are all literal.
func parseEnvFile(body string) map[string]string {
	out := map[string]string{}

	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}

		out[key] = unquoteEnv(strings.TrimSpace(value))
	}

	return out
}

// unquoteEnv removes one pair of matching quotes around a value.
func unquoteEnv(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
