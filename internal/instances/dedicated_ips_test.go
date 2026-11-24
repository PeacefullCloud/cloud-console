package instances

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/database"
)

func newTestInstancesService(t *testing.T) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"), log)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	app := &core.App{
		DB:  db,
		Cfg: &config.Config{},
		Log: log,
	}
	return New(app)
}

func TestDedicatedIPPoolLifecycleAndValidation(t *testing.T) {
	svc := newTestInstancesService(t)
	ctx := context.Background()

	badCases := []struct {
		name   string
		pub    string
		listen string
	}{
		{"empty", "", ""},
		{"garbage", "not-an-ip", ""},
		{"loopback", "127.0.0.1", ""},
		{"unspecified", "0.0.0.0", ""},
		{"link-local", "169.254.1.1", ""},
		{"family mismatch", "203.0.113.10", "2001:db8::1"},
		{"bad listen", "203.0.113.10", "127.0.0.2"},
	}
	for _, tc := range badCases {
		if _, err := svc.AddDedicatedIP(ctx, tc.pub, tc.listen, "incusbr0", ""); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: got err=%v, want ErrInvalidSpec", tc.name, err)
		}
	}

	// Valid bare-metal IP where listen == public normalizes listen to "".
	ip1, err := svc.AddDedicatedIP(ctx, "203.0.113.50", "203.0.113.50", "", "Hetzner #1")
	if err != nil {
		t.Fatalf("add ip1: %v", err)
	}
	if ip1.ListenIP != "" || ip1.EffectiveListenIP() != "203.0.113.50" || ip1.Network != "incusbr0" {
		t.Fatalf("unexpected ip1: %+v", ip1)
	}

	// Valid AWS-style NAT IP where Elastic IP maps to a secondary private IP.
	ip2, err := svc.AddDedicatedIP(ctx, "198.51.100.20", "172.31.1.10", "incusbr0", "AWS EIP")
	if err != nil {
		t.Fatalf("add ip2: %v", err)
	}
	if ip2.EffectiveListenIP() != "172.31.1.10" {
		t.Fatalf("EffectiveListenIP = %q, want 172.31.1.10", ip2.EffectiveListenIP())
	}

	// Duplicate public or listen IP is refused.
	if _, err := svc.AddDedicatedIP(ctx, "203.0.113.50", "", "incusbr0", ""); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("duplicate public IP err=%v, want ErrInvalidSpec", err)
	}
	if _, err := svc.AddDedicatedIP(ctx, "198.51.100.21", "172.31.1.10", "incusbr0", ""); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("duplicate listen IP err=%v, want ErrInvalidSpec", err)
	}

	// Simulate assignment at DB layer: RemoveDedicatedIP must refuse while assigned.
	if err := svc.app.DB.AssignDedicatedIP(ip1.ID, "web-1", "10.48.36.55", true); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDedicatedIP(ctx, ip1.ID); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("delete while assigned err=%v, want ErrInvalidSpec", err)
	}

	// Rename updates DB assignment.
	if err := svc.app.DB.RenameDedicatedIPs("web-1", "web-2"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.app.DB.ListDedicatedIPsForInstance("web-2")
	if err != nil || len(got) != 1 || got[0].PublicIP != "203.0.113.50" {
		t.Fatalf("renamed lookup = %+v, err=%v", got, err)
	}

	// Unassign and delete succeed.
	if _, err := svc.UnassignDedicatedIP(ctx, ip1.ID); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if err := svc.RemoveDedicatedIP(ctx, ip1.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestFindInstanceNetworkTarget(t *testing.T) {
	full := &incusapi.InstanceFull{
		Instance: incusapi.Instance{
			Name: "vm1",
			ExpandedConfig: map[string]string{
				"volatile.eth0.hwaddr": "10:66:6a:e4:5f:bd",
			},
			ExpandedDevices: map[string]map[string]string{
				"eth0": {"type": "nic", "network": "incusbr0"},
			},
		},
		State: &incusapi.InstanceState{
			Network: map[string]incusapi.InstanceStateNetwork{
				"enp5s0": {
					Hwaddr: "10:66:6a:e4:5f:bd",
					Addresses: []incusapi.InstanceStateNetworkAddress{
						{Family: "inet", Address: "10.48.36.55", Scope: "global"},
						{Family: "inet6", Address: "fd42:1af7:b328:716c:1266:6aff:fee4:5fbd", Scope: "global"},
						{Family: "inet6", Address: "fe80::1266:6aff:fee4:5fbd", Scope: "link"},
					},
				},
			},
		},
	}

	nic, v4, err := findInstanceNetworkTarget(full, "incusbr0", true)
	if err != nil || nic != "eth0" || v4 != "10.48.36.55" {
		t.Fatalf("v4 target = (%q, %q, %v)", nic, v4, err)
	}

	nic, v6, err := findInstanceNetworkTarget(full, "incusbr0", false)
	if err != nil || nic != "eth0" || v6 != "fd42:1af7:b328:716c:1266:6aff:fee4:5fbd" {
		t.Fatalf("v6 target = (%q, %q, %v)", nic, v6, err)
	}

	// Stopped instance falls back to pinned static address on the NIC.
	full.State = nil
	full.ExpandedDevices["eth0"]["ipv4.address"] = "10.48.36.99"
	_, staticV4, err := findInstanceNetworkTarget(full, "incusbr0", true)
	if err != nil || staticV4 != "10.48.36.99" {
		t.Fatalf("static fallback = (%q, %v)", staticV4, err)
	}
}
