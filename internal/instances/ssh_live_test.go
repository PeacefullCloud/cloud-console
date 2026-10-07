package instances

// Live verification of the dnf/sshd SSH recipe on a real Rocky instance.
// Guarded by INCUS_LIVE_TEST=1 so the normal suite never touches Incus.
// Run: sg incus-admin -c "INCUS_LIVE_TEST=1 go test ./internal/instances -run TestLiveRockySSH -v"

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/incus"
)

func TestLiveRockySSH(t *testing.T) {
	if os.Getenv("INCUS_LIVE_TEST") != "1" {
		t.Skip("live incus test; set INCUS_LIVE_TEST=1")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client, err := incus.Connect("/var/lib/incus/unix.socket", "", log)
	if err != nil {
		t.Fatalf("connect incus: %v", err)
	}
	svc := New(&core.App{Incus: client, Log: log})

	image, ok := incus.LookupImage("rocky-9")
	if !ok {
		t.Fatal("rocky-9 not in catalog")
	}
	if image.PackageManager != "dnf" || image.SSHUnit != "sshd" {
		t.Fatalf("unexpected recipe: %+v", image)
	}

	// Ephemeral keypair for the test.
	tmp := t.TempDir()
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", filepath.Join(tmp, "id"), "-C", "live-test").CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(filepath.Join(tmp, "id.pub"))
	if err != nil {
		t.Fatalf("read pubkey: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := svc.EnsureSSH(ctx, "rocky-test", image, "Live-Test-8!", string(pub)); err != nil {
		t.Fatalf("EnsureSSH: %v", err)
	}

	// Port 22 reachable from the host.
	full, err := client.InstanceFull("rocky-test")
	if err != nil {
		t.Fatalf("instance full: %v", err)
	}
	ip := incus.InstanceIP(full)
	if ip == "" {
		t.Fatal("no IP for rocky-test")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 10*time.Second)
	if err != nil {
		t.Fatalf("dial ssh: %v", err)
	}
	_ = conn.Close()
	fmt.Println("rocky-test SSH reachable at", ip)
}
