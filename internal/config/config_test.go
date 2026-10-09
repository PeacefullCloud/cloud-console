package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	body := `
# A comment
CONSOLE_ADDR=127.0.0.1:8080

export INCUS_PROJECT="project one"
S3_BUCKET='my-bucket'
S3_PREFIX=peaceful-cloud
URL=https://example.com:8443/path?x=1&y=2
EMPTY=
  PADDED  =  spaced value
NOT_A_PAIR
=missing-key
BAD KEY=value
`

	got := parseEnvFile(body)

	want := map[string]string{
		"CONSOLE_ADDR":  "127.0.0.1:8080",
		"INCUS_PROJECT": "project one",
		"S3_BUCKET":     "my-bucket",
		"S3_PREFIX":     "peaceful-cloud",
		"URL":           "https://example.com:8443/path?x=1&y=2",
		"EMPTY":         "",
		"PADDED":        "spaced value",
	}

	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %#v", len(got), len(want), got)
	}

	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func TestParseEnvFileHandlesCRLF(t *testing.T) {
	got := parseEnvFile("A=1\r\nB=2\r\n# c\r\n")

	if got["A"] != "1" {
		t.Errorf("A = %q, want \"1\"", got["A"])
	}
	if got["B"] != "2" {
		t.Errorf("B = %q, want \"2\"", got["B"])
	}
}

func TestUnquoteEnv(t *testing.T) {
	cases := map[string]string{
		`"quoted"`:     "quoted",
		`'single'`:     "single",
		`unquoted`:     "unquoted",
		`"mismatched'`: `"mismatched'`,
		`"`:            `"`,
		``:             ``,
	}

	for in, want := range cases {
		if got := unquoteEnv(in); got != want {
			t.Errorf("unquoteEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.env")

	body := "CONSOLE_ADDR=127.0.0.1:9000\n" +
		"INCUS_PROJECT=\"project one\"\n" +
		"S3_BUCKET='from-file'\n" +
		"CONSOLE_METRICS_INTERVAL=15\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	// The keys the loader will touch must be clean before and after.
	for _, key := range []string{"CONSOLE_ADDR", "INCUS_PROJECT", "S3_BUCKET", "CONSOLE_METRICS_INTERVAL"} {
		key := key
		t.Cleanup(func() { _ = os.Unsetenv(key) })
		_ = os.Unsetenv(key)
	}

	// A real environment variable must take precedence over the file.
	t.Setenv("S3_BUCKET", "from-real-env")
	t.Setenv("CONSOLE_ENV_FILE", path)

	loaded, err := loadEnvFile()
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if loaded != path {
		t.Errorf("loaded = %q, want %q", loaded, path)
	}

	if got := os.Getenv("CONSOLE_ADDR"); got != "127.0.0.1:9000" {
		t.Errorf("CONSOLE_ADDR = %q, want \"127.0.0.1:9000\"", got)
	}
	if got := os.Getenv("INCUS_PROJECT"); got != "project one" {
		t.Errorf("INCUS_PROJECT = %q, want \"project one\" (quotes stripped)", got)
	}
	if got := os.Getenv("S3_BUCKET"); got != "from-real-env" {
		t.Errorf("S3_BUCKET = %q, want the real environment to win", got)
	}
	if got := os.Getenv("CONSOLE_METRICS_INTERVAL"); got != "15" {
		t.Errorf("CONSOLE_METRICS_INTERVAL = %q, want \"15\"", got)
	}
}

func TestLoadEnvFileMissingDefaultIsFine(t *testing.T) {
	// No .env in an empty working directory must not be an error.
	t.Chdir(t.TempDir())
	t.Setenv("CONSOLE_ENV_FILE", "")

	loaded, err := loadEnvFile()
	if err != nil {
		t.Fatalf("a missing default .env should be ignored, got: %v", err)
	}
	if loaded != "" {
		t.Errorf("loaded = %q, want no file", loaded)
	}
}

func TestLoadEnvFileMissingExplicitIsAnError(t *testing.T) {
	t.Setenv("CONSOLE_ENV_FILE", filepath.Join(t.TempDir(), "does-not-exist.env"))

	if _, err := loadEnvFile(); err == nil {
		t.Fatal("an explicitly requested env file that is missing should be an error")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	// A clean working directory and no env file: every default must apply.
	t.Chdir(t.TempDir())

	// Blank out everything the assertions below depend on, so a developer's
	// own shell environment cannot influence the result.
	for _, key := range []string{
		"CONSOLE_ENV_FILE", "CONSOLE_DATA_DIR", "CONSOLE_ADDR", "CONSOLE_BASE_URL", "CONSOLE_SECURE_COOKIES", "INCUS_SOCKET",
		"S3_REGION", "S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY",
		"CONSOLE_METRICS_INTERVAL", "CONSOLE_METRICS_RETENTION_HOURS",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CONSOLE_DATA_DIR", filepath.Join(t.TempDir(), "data"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want loopback by default", cfg.Addr)
	}
	if cfg.SecureCookies {
		t.Error("an http base URL must not force Secure cookies")
	}
	if cfg.IncusSocket != DefaultIncusSocket {
		t.Errorf("IncusSocket = %q, want %q", cfg.IncusSocket, DefaultIncusSocket)
	}
	if cfg.S3Region != "us-east-1" {
		t.Errorf("S3Region = %q", cfg.S3Region)
	}
	if cfg.EnvFile != "" {
		t.Errorf("EnvFile = %q, want empty", cfg.EnvFile)
	}
	if cfg.S3Configured() {
		t.Error("S3 should not be considered configured without credentials")
	}
	// Values below their minimum must be clamped up.
	if cfg.MetricsIntervalSeconds < 10 {
		t.Errorf("MetricsIntervalSeconds = %d, want at least 10", cfg.MetricsIntervalSeconds)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies(" 10.0.0.0/8, 192.0.2.7 ,::1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "::1/128"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}

	for _, empty := range []string{"", "none", " NONE "} {
		if got, err := ParseTrustedProxies(empty); err != nil || len(got) != 0 {
			t.Errorf("%q: got %v, %v; want no proxies", empty, got, err)
		}
	}

	if _, err := ParseTrustedProxies("10.0.0.0/8,example.com"); err == nil {
		t.Error("a hostname must be rejected")
	}
}

func TestSecureCookiesFollowHTTPSBaseURL(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CONSOLE_ENV_FILE", "")
	t.Setenv("CONSOLE_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("CONSOLE_BASE_URL", "https://console.example.com")
	t.Setenv("CONSOLE_SECURE_COOKIES", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SecureCookies {
		t.Error("an https base URL should turn Secure cookies on by default")
	}

	t.Setenv("CONSOLE_SECURE_COOKIES", "false")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecureCookies {
		t.Error("an explicit false must win")
	}
	if len(cfg.Warnings()) == 0 {
		t.Error("https with insecure cookies should warn")
	}
}

func TestWarnings(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want int
	}{
		{"loopback behind proxy", Config{Addr: "127.0.0.1:8080", BaseURL: "https://c.example.com", SecureCookies: true}, 0},
		{"plain http on loopback", Config{Addr: "127.0.0.1:8080", BaseURL: "http://localhost:8080"}, 0},
		{"localhost name", Config{Addr: "localhost:8080", BaseURL: "http://localhost:8080"}, 0},
		{"ipv6 loopback", Config{Addr: "[::1]:8080", BaseURL: "http://localhost:8080"}, 0},
		{"all interfaces over http", Config{Addr: ":8080", BaseURL: "http://localhost:8080"}, 1},
		{"public ip over http", Config{Addr: "0.0.0.0:8080", BaseURL: "http://x"}, 1},
		{"https without secure cookies", Config{Addr: "127.0.0.1:8080", BaseURL: "https://c.example.com"}, 1},
	}
	for _, tc := range tests {
		if got := len(tc.cfg.Warnings()); got != tc.want {
			t.Errorf("%s: %d warnings %v, want %d", tc.name, got, tc.cfg.Warnings(), tc.want)
		}
	}
}

func TestSnapshotScheduleDefaultsAndClamping(t *testing.T) {
	t.Setenv("CONSOLE_DATA_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SnapshotIntervalHours != 0 || cfg.SnapshotKeep != 7 {
		t.Errorf("defaults = %d/%d, want off/7", cfg.SnapshotIntervalHours, cfg.SnapshotKeep)
	}

	t.Setenv("CONSOLE_SNAPSHOT_INTERVAL_HOURS", "-5")
	t.Setenv("CONSOLE_SNAPSHOT_KEEP", "0")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SnapshotIntervalHours != 0 || cfg.SnapshotKeep != 1 {
		t.Errorf("clamped = %d/%d, want 0/1", cfg.SnapshotIntervalHours, cfg.SnapshotKeep)
	}
}
