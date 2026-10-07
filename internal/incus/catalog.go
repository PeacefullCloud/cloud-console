package incus

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/lxc/incus/v6/shared/api"
)

// Image describes an image offered by the console's "Create instance" wizard.
type Image struct {
	ID          string // stable identifier used in URLs and stored in SQLite
	Label       string // display name, e.g. "Ubuntu 24.04 LTS"
	Category    string // os | app
	Description string
	Kind        string // default instance type: container | virtual-machine

	// SupportsVM reports whether the image can back a virtual machine as
	// well as a container. The remote image server resolves the same alias
	// to the matching variant for the requested instance type.
	SupportsVM bool

	// PackageManager selects the SSH server install command (apt | dnf |
	// apk). SSHUnit is the systemd service name; empty means OpenRC.
	PackageManager string
	SSHUnit        string

	// Incus image source.
	Alias    string // e.g. ubuntu/24.04 (local alias or simplestreams alias)
	Server   string // remote simplestreams server; empty means "local only"
	Protocol string // simplestreams | incus; empty means "local only"

	// Presentation
	Icon  string // css class suffix, e.g. "ubuntu"
	Group string // grouping header in the wizard, e.g. "Linux operating system"
}

// DefaultCatalog is the small, deliberately boring image list from the V1 plan.
//
// WordPress is expected to exist as a prepared local Incus image; until it does,
// the wizard reports that the alias is missing instead of failing silently.
func DefaultCatalog() []Image {
	return []Image{
		{
			ID: "ubuntu-24.04", Label: "Ubuntu 24.04 LTS", Category: "os", Kind: "container",
			Description: "Ubuntu 24.04 LTS.",
			Alias:       "ubuntu/24.04", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "ubuntu", Group: "Linux operating system", SupportsVM: true, PackageManager: "apt", SSHUnit: "ssh",
		},
		{
			ID: "ubuntu-22.04", Label: "Ubuntu 22.04 LTS", Category: "os", Kind: "container",
			Description: "Ubuntu 22.04 LTS.",
			Alias:       "ubuntu/22.04", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "ubuntu", Group: "Linux operating system", SupportsVM: true, PackageManager: "apt", SSHUnit: "ssh",
		},
		{
			ID: "debian-13", Label: "Debian 13", Category: "os", Kind: "container",
			Description: "Debian 13 (trixie).",
			Alias:       "debian/13", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "debian", Group: "Linux operating system", SupportsVM: true, PackageManager: "apt", SSHUnit: "ssh",
		},
		{
			ID: "debian-12", Label: "Debian 12", Category: "os", Kind: "container",
			Description: "Debian 12 (bookworm).",
			Alias:       "debian/12", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "debian", Group: "Linux operating system", SupportsVM: true, PackageManager: "apt", SSHUnit: "ssh",
		},
		{
			ID: "almalinux-9", Label: "AlmaLinux 9", Category: "os", Kind: "container",
			Description: "AlmaLinux 9.",
			Alias:       "almalinux/9", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "almalinux", Group: "Linux operating system", SupportsVM: true, PackageManager: "dnf", SSHUnit: "sshd",
		},
		{
			ID: "rocky-9", Label: "Rocky Linux 9", Category: "os", Kind: "container",
			Description: "Rocky Linux 9, stable RHEL-compatible.",
			Alias:       "rockylinux/9", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "rocky", Group: "Linux operating system", SupportsVM: true, PackageManager: "dnf", SSHUnit: "sshd",
		},
		{
			ID: "alpine-3.20", Label: "Alpine 3.20", Category: "os", Kind: "container",
			Description: "Minimal Alpine Linux.",
			Alias:       "alpine/3.20", Server: "https://images.linuxcontainers.org", Protocol: "simplestreams",
			Icon: "alpine", Group: "Linux operating system", SupportsVM: true, PackageManager: "apk", SSHUnit: "",
		},
		{
			ID: "wordpress", Label: "WordPress", Category: "app", Kind: "container",
			Description: "Prepared image with Caddy, PHP-FPM, WordPress and MariaDB. Build it locally first.",
			Alias:       "wordpress-php8.3", Icon: "wordpress", Group: "Applications",
			PackageManager: "apt", SSHUnit: "ssh",
		},
	}
}

// LookupImage finds a catalog entry by ID or alias.
func LookupImage(id string) (Image, bool) {
	for _, img := range DefaultCatalog() {
		if img.ID == id || img.Alias == id {
			return img, true
		}
	}
	return Image{}, false
}

// Source converts a catalog entry into an Incus instance source.
func (i Image) Source() api.InstanceSource {
	src := api.InstanceSource{Type: "image", Alias: i.Alias}
	if i.Server != "" {
		src.Server = i.Server
		src.Protocol = i.Protocol
	}
	return src
}

// InstanceKind normalises an Incus instance type into "container" or
// "virtual-machine".
func InstanceKind(inst api.Instance) string {
	if api.InstanceType(inst.Type) == api.InstanceTypeVM {
		return "virtual-machine"
	}
	return "container"
}

// ParseBytes understands Incus size strings such as "4GB", "512MiB", "50%".
//
// Percentages return 0 because they depend on the host's capacity.
func ParseBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, "%") {
		return 0
	}

	units := []struct {
		suffix string
		factor int64
	}{
		{"EiB", 1 << 60}, {"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"EB", 1e18}, {"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"bit", 1}, {"B", 1},
	}

	for _, u := range units {
		if strings.HasSuffix(strings.ToLower(s), strings.ToLower(u.suffix)) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0
			}
			return int64(f * float64(u.factor))
		}
	}

	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

// FormatBytes renders a byte count with a binary unit suffix.
func FormatBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[idx])
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}

// FormatGB renders a byte count as a rounded gigabyte figure.
func FormatGB(n int64) string {
	if n <= 0 {
		return "0 GB"
	}
	if n >= 1<<30 {
		return fmt.Sprintf("%.0f GB", math.Round(float64(n)/float64(1<<30)))
	}
	return FormatBytes(n)
}

// ParseCPUs reads an Incus CPU limit ("2", "0-1", "2,3") and returns the core
// count. An empty or unparseable value returns 0, meaning "unlimited".
func ParseCPUs(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	total := 0
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, errA := strconv.Atoi(strings.TrimSpace(lo))
			b, errB := strconv.Atoi(strings.TrimSpace(hi))
			if errA == nil && errB == nil && b >= a {
				total += b - a + 1
				continue
			}
			return 0
		}
		if v, err := strconv.Atoi(part); err == nil {
			// "cpuset" style values list individual cores.
			total++
			_ = v
			continue
		}
		return 0
	}
	if total == 1 {
		// A single numeric value is a core count, not a core list.
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return total
}

// InstanceCPU returns the configured vCPU count (0 when unlimited).
func InstanceCPU(inst api.Instance) int {
	return ParseCPUs(inst.Config["limits.cpu"])
}

// InstanceMemoryBytes returns the configured memory limit in bytes (0 when
// unlimited).
func InstanceMemoryBytes(inst api.Instance) int64 {
	return ParseBytes(inst.Config["limits.memory"])
}

// InstanceDiskBytes returns the configured root disk size in bytes (0 when the
// pool default applies).
func InstanceDiskBytes(inst api.Instance) int64 {
	for _, devices := range []api.DevicesMap{inst.ExpandedDevices, inst.Devices} {
		if devices == nil {
			continue
		}
		root, ok := devices["root"]
		if !ok {
			continue
		}
		if size := ParseBytes(root["size"]); size > 0 {
			return size
		}
	}
	return 0
}

// InstanceStoragePool returns the storage pool backing the root device.
func InstanceStoragePool(inst api.Instance) string {
	for _, devices := range []api.DevicesMap{inst.ExpandedDevices, inst.Devices} {
		if devices == nil {
			continue
		}
		if root, ok := devices["root"]; ok && root["pool"] != "" {
			return root["pool"]
		}
	}
	return ""
}

// InstanceIP returns the instance's primary IPv4 address, preferring the
// default interface.
func InstanceIP(full *api.InstanceFull) string {
	if full == nil || full.State == nil {
		return ""
	}

	preferred := []string{"eth0", "enp5s0", "ens3", "enp1s0"}
	for _, name := range preferred {
		if addr := firstGlobalIPv4(full.State.Network[name]); addr != "" {
			return addr
		}
	}

	// Deterministic fallback across the remaining interfaces.
	names := make([]string, 0, len(full.State.Network))
	for name := range full.State.Network {
		names = append(names, name)
	}
	sortStrings(names)

	for _, name := range names {
		if addr := firstGlobalIPv4(full.State.Network[name]); addr != "" {
			return addr
		}
	}
	return ""
}

// InstanceIPs returns every global address an instance holds.
func InstanceIPs(full *api.InstanceFull) []string {
	if full == nil || full.State == nil {
		return nil
	}
	var out []string
	for _, iface := range full.State.Network {
		for _, addr := range iface.Addresses {
			if addr.Scope == "global" && addr.Address != "" {
				out = append(out, addr.Address)
			}
		}
	}
	sortStrings(out)
	return out
}

func firstGlobalIPv4(iface api.InstanceStateNetwork) string {
	// Interfaces that are down never carry useful addresses.
	if iface.State != "" && iface.State != "up" && iface.State != "unknown" {
		return ""
	}
	for _, addr := range iface.Addresses {
		if addr.Family != "inet" {
			continue
		}
		if addr.Scope != "global" {
			continue
		}
		if strings.HasPrefix(addr.Address, "127.") {
			continue
		}
		return addr.Address
	}
	return ""
}

// StateMemoryUsed returns the instance's current memory usage in bytes.
func StateMemoryUsed(state *api.InstanceState) int64 {
	if state == nil {
		return 0
	}
	return state.Memory.Usage
}

// StateDiskUsed returns the instance's current disk usage in bytes.
func StateDiskUsed(state *api.InstanceState) int64 {
	if state == nil {
		return 0
	}
	var total int64
	for _, disk := range state.Disk {
		total += disk.Usage
	}
	return total
}

// StateNetworkTotals sums an instance's receive and transmit counters.
func StateNetworkTotals(state *api.InstanceState) (rx, tx int64) {
	if state == nil {
		return 0, 0
	}
	for _, iface := range state.Network {
		rx += iface.Counters.BytesReceived
		tx += iface.Counters.BytesSent
	}
	return rx, tx
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
