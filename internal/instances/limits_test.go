package instances

import (
	"errors"
	"testing"

	incusapi "github.com/lxc/incus/v6/shared/api"
)

func ptr(n int) *int { return &n }

func baseInstance() *incusapi.Instance {
	inst := &incusapi.Instance{}
	inst.Profiles = []string{"default"}
	inst.Config = map[string]string{"limits.cpu": "2", "limits.memory": "2048MiB", "user.note": "keep"}
	inst.Devices = map[string]map[string]string{}
	inst.ExpandedDevices = map[string]map[string]string{
		"root": {"type": "disk", "path": "/", "pool": "tank", "size": "10GiB"},
	}
	return inst
}

func TestLimitsBlankFieldsChangeNothing(t *testing.T) {
	put, err := limitsPut(baseInstance(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if put.Config["limits.cpu"] != "2" || put.Config["limits.memory"] != "2048MiB" {
		t.Errorf("limits were altered by an empty update: %v", put.Config)
	}
	if _, ok := put.Devices["root"]; ok {
		t.Error("root device touched by an empty update")
	}
}

func TestLimitsExplicitZeroRemovesLimit(t *testing.T) {
	put, err := limitsPut(baseInstance(), Limits{CPU: ptr(0), MemoryMB: ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := put.Config["limits.cpu"]; ok {
		t.Error("cpu limit not removed")
	}
	if _, ok := put.Config["limits.memory"]; ok {
		t.Error("memory limit not removed")
	}
	if put.Config["user.note"] != "keep" {
		t.Error("unrelated config lost")
	}
}

func TestLimitsGrowsProfileRootDiskWithItsPool(t *testing.T) {
	put, err := limitsPut(baseInstance(), Limits{DiskGB: ptr(20)})
	if err != nil {
		t.Fatal(err)
	}
	root := put.Devices["root"]
	if root["pool"] != "tank" || root["path"] != "/" || root["type"] != "disk" {
		t.Errorf("root device lost the profile's pool or path: %v", root)
	}
	if root["size"] != "20GiB" {
		t.Errorf("size = %q", root["size"])
	}
}

func TestLimitsRefusesShrinkAndAcceptsDisplayedValue(t *testing.T) {
	if _, err := limitsPut(baseInstance(), Limits{DiskGB: ptr(5)}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("shrink err = %v", err)
	}

	inst := baseInstance()
	inst.ExpandedDevices["root"]["size"] = "10.5GiB"
	put, err := limitsPut(inst, Limits{DiskGB: ptr(10)})
	if err != nil {
		t.Fatalf("resubmitting the rounded-down value failed: %v", err)
	}
	if _, ok := put.Devices["root"]; ok {
		t.Error("an unchanged disk size rewrote the root device")
	}
}

func TestLimitsNoRootDevice(t *testing.T) {
	inst := baseInstance()
	inst.ExpandedDevices = nil
	if _, err := limitsPut(inst, Limits{DiskGB: ptr(20)}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("err = %v", err)
	}
}

func TestLimitsValidate(t *testing.T) {
	for _, l := range []Limits{{CPU: ptr(-1)}, {CPU: ptr(MaxCPU + 1)}, {MemoryMB: ptr(-5)}, {MemoryMB: ptr(32)}, {DiskGB: ptr(MaxDiskGB + 1)}} {
		if err := l.validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%+v accepted", l)
		}
	}
	if err := (Limits{CPU: ptr(4), MemoryMB: ptr(1024), DiskGB: ptr(10)}).validate(); err != nil {
		t.Error(err)
	}
}

func TestValidateNameRejectsReservedNames(t *testing.T) {
	for _, reserved := range []string{"console", "incus"} {
		if err := ValidateName(reserved); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalidName", reserved, err)
		}
	}
	if err := ValidateName("web-app-1"); err != nil {
		t.Errorf("ValidateName(web-app-1) = %v", err)
	}
}

func TestValidateSSHKeyRejectsNewlinesAndControlChars(t *testing.T) {
	validKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILM+H8j0 user@example"
	if err := ValidateSSHKey(validKey); err != nil {
		t.Fatalf("expected valid key, got %v", err)
	}
	for _, bad := range []string{
		validKey + "\ncommand=\"/bin/sh\" " + validKey,
		validKey + "\r" + validKey,
		validKey + "\x00",
	} {
		if err := ValidateSSHKey(bad); err == nil {
			t.Errorf("expected multi-line/control-char SSH key to be rejected")
		}
	}
}

func TestValidVolumeMountPath(t *testing.T) {
	for _, okPath := range []string{"/mnt/data", "/srv/storage", "/data/db", "/opt/app"} {
		if _, err := validVolumeMountPath(okPath); err != nil {
			t.Errorf("validVolumeMountPath(%q) unexpected error: %v", okPath, err)
		}
	}
	for _, badPath := range []string{"", "/", "/etc", "/etc/passwd", "/var/lib", "/root/data", "/mnt", "/mnt/../etc/ shadow", "relative/path"} {
		if _, err := validVolumeMountPath(badPath); err == nil {
			t.Errorf("validVolumeMountPath(%q) expected error, got nil", badPath)
		}
	}
}

