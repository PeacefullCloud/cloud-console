package web

import (
	"testing"

	"github.com/peaceful/cloud-console/internal/instances"
)

func TestParseSizeValue(t *testing.T) {
	cpu, mem, disk, ok := ParseSizeValue("2|4096|60")
	if !ok || cpu != 2 || mem != 4096 || disk != 60 {
		t.Fatalf("ParseSizeValue = %d,%d,%d,%v", cpu, mem, disk, ok)
	}

	for _, bad := range []string{"", "2", "2|4096", "a|b|c"} {
		if _, _, _, ok := ParseSizeValue(bad); ok {
			t.Errorf("ParseSizeValue(%q) should have failed", bad)
		}
	}
}

func TestDefaultSizesAreValid(t *testing.T) {
	sizes := DefaultSizes()
	if len(sizes) == 0 {
		t.Fatal("no default sizes")
	}

	seen := map[string]bool{}
	for _, s := range sizes {
		if seen[s.ID] {
			t.Errorf("duplicate size id %q", s.ID)
		}
		seen[s.ID] = true

		if s.CPU <= 0 || s.MemoryMB <= 0 || s.DiskGB <= 0 {
			t.Errorf("size %s has non-positive resources: %+v", s.ID, s)
		}

		// Every preset must survive a round trip through the form value.
		cpu, mem, disk, ok := ParseSizeValue(s.Value())
		if !ok || cpu != s.CPU || mem != s.MemoryMB || disk != s.DiskGB {
			t.Errorf("size %s did not round trip: %q", s.ID, s.Value())
		}
	}
}

func TestNormaliseTabAndWindow(t *testing.T) {
	if got := normaliseTab("metrics"); got != "metrics" {
		t.Errorf("normaliseTab(metrics) = %q", got)
	}
	// An unknown tab must fall back to overview rather than 404.
	if got := normaliseTab("../../etc/passwd"); got != "overview" {
		t.Errorf("normaliseTab(invalid) = %q, want overview", got)
	}
	if got := normaliseWindow("week"); got != "week" {
		t.Errorf("normaliseWindow(week) = %q", got)
	}
	if got := normaliseWindow("nonsense"); got != "hour" {
		t.Errorf("normaliseWindow(nonsense) = %q, want hour", got)
	}
}

func TestSafeNext(t *testing.T) {
	allowed := []string{"/instances", "/instances?tab=metrics", ""}
	for _, in := range allowed {
		if got := safeNext(in); got != in {
			t.Errorf("safeNext(%q) = %q, want the input unchanged", in, got)
		}
	}

	// Open redirects and absolute URLs must be rejected.
	rejected := []string{"//evil.example.com", "https://evil.example.com", "http://evil.example.com/x", "javascript:alert(1)"}
	for _, in := range rejected {
		if got := safeNext(in); got != "" {
			t.Errorf("safeNext(%q) = %q, want an empty string", in, got)
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[string]string{
		"Running": "status-running",
		"Stopped": "status-stopped",
		"Frozen":  "status-stopped",
		"Error":   "status-error",
		"":        "status-stopped",
		"Unknown": "status-pending",
	}
	for in, want := range cases {
		if got := statusClass(in); got != want {
			t.Errorf("statusClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsageClass(t *testing.T) {
	if got := usageClass(10); got != "" {
		t.Errorf("usageClass(10) = %q, want empty", got)
	}
	if got := usageClass(80); got != "warn" {
		t.Errorf("usageClass(80) = %q, want warn", got)
	}
	if got := usageClass(95); got != "danger" {
		t.Errorf("usageClass(95) = %q, want danger", got)
	}
}

func TestPercentHelpersClamp(t *testing.T) {
	if got := memPercent(0, 0); got != 0 {
		t.Errorf("memPercent(0,0) = %v", got)
	}
	if got := memPercent(8<<30, 4<<30); got != 100 {
		t.Errorf("memPercent should clamp to 100, got %v", got)
	}
	if got := diskPercent(1, 0); got != 0 {
		t.Errorf("diskPercent with no total = %v, want 0", got)
	}
	if got := poolPercent(1, 2); got != 50 {
		t.Errorf("poolPercent(1,2) = %v, want 50", got)
	}
}

func TestMaskSecret(t *testing.T) {
	if got := maskSecret(""); got != "—" {
		t.Errorf("maskSecret(\"\") = %q", got)
	}
	if got := maskSecret("short"); got == "short" {
		t.Error("maskSecret leaked a short secret")
	}
	long := maskSecret("AKIAIOSFODNN7EXAMPLE")
	if long == "AKIAIOSFODNN7EXAMPLE" {
		t.Error("maskSecret leaked a long secret")
	}
}

func TestJobTitle(t *testing.T) {
	if got := jobTitle("backup.create"); got != "Create backup" {
		t.Errorf("jobTitle(backup.create) = %q", got)
	}
	if got := jobTitle("unknown.thing"); got == "" {
		t.Error("jobTitle must always return something")
	}
}

func TestInitials(t *testing.T) {
	cases := map[string]string{
		"admin":    "AD",
		"a":        "A",
		"ops.team": "OT",
		"jane doe": "JD",
		"":         "?",
	}
	for in, want := range cases {
		if got := initials(in); got != want {
			t.Errorf("initials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOSClassAndInitial(t *testing.T) {
	cases := []struct {
		label   string
		class   string
		initial string
	}{
		{"Ubuntu 24.04 LTS", "os-ubuntu", "U"},
		{"Debian 13", "os-debian", "D"},
		{"AlmaLinux 9", "os-almalinux", "A"},
		{"Alpine 3.20", "os-alpine", "A"},
		{"WordPress", "os-wordpress", "W"},
		{"Something else", "os-custom", "S"},
	}

	for _, tc := range cases {
		if got := osClass(tc.label); got != tc.class {
			t.Errorf("osClass(%q) = %q, want %q", tc.label, got, tc.class)
		}
		if got := osInitial(tc.label); got != tc.initial {
			t.Errorf("osInitial(%q) = %q, want %q", tc.label, got, tc.initial)
		}
	}
}

func TestCreateRequestValidation(t *testing.T) {
	valid := func() instances.CreateRequest {
		return instances.CreateRequest{
			Name:     "my-site",
			ImageID:  "ubuntu-24.04",
			CPU:      2,
			MemoryMB: 2048,
			DiskGB:   40,
		}
	}

	validReq := valid()
	if err := validReq.Validate(); err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}

	bad := map[string]func(*instances.CreateRequest){
		"empty name":      func(r *instances.CreateRequest) { r.Name = "" },
		"uppercase name":  func(r *instances.CreateRequest) { r.Name = "My_Site" },
		"leading hyphen":  func(r *instances.CreateRequest) { r.Name = "-site" },
		"reserved prefix": func(r *instances.CreateRequest) { r.Name = "console-thing" },
		"too few cpus":    func(r *instances.CreateRequest) { r.CPU = 0 },
		"too much ram":    func(r *instances.CreateRequest) { r.MemoryMB = 5_000_000 },
		"tiny disk":       func(r *instances.CreateRequest) { r.DiskGB = 1 },
		"unknown image":   func(r *instances.CreateRequest) { r.ImageID = "windows" },
		"bad domain":      func(r *instances.CreateRequest) { r.Domain = "not a domain" },
		"no dot domain":   func(r *instances.CreateRequest) { r.Domain = "localhost" },
	}

	for name, mutate := range bad {
		req := valid()
		mutate(&req)
		if err := req.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}

	// A well-formed domain must be accepted and normalised to lowercase.
	req := valid()
	req.Domain = "Example.COM"
	if err := req.Validate(); err != nil {
		t.Fatalf("a valid domain was rejected: %v", err)
	}
	if req.Domain != "example.com" {
		t.Errorf("domain = %q, want it lowercased", req.Domain)
	}
}
