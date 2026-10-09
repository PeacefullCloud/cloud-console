package instances

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/incus"
)

// sshCommandTimeout bounds each setup command. First-boot package installs
// are the slowest step.
const sshCommandTimeout = 10 * time.Minute

// EnsureSSH makes root SSH work on a freshly started instance: it installs
// the OpenSSH server when missing, installs the key and/or password, and
// enables the service. It is idempotent, so re-running it is safe.
func (s *Service) EnsureSSH(ctx context.Context, name string, image incus.Image, password, key string) error {
	if password == "" && key == "" {
		return fmt.Errorf("%w: a root password or SSH key is required", ErrInvalidSpec)
	}

	// The key, the password and the server install are independent of each
	// other, so run them together: the step costs the slowest of the three
	// instead of the sum.
	type task struct {
		name string
		run  func() error
	}
	tasks := make([]task, 0, 3)
	if key != "" {
		tasks = append(tasks, task{"install SSH key", func() error {
			return s.installSSHKey(ctx, name, key)
		}})
	}
	if password != "" {
		tasks = append(tasks, task{"set root password", func() error {
			return s.setRootPassword(ctx, name, password)
		}})
	}
	tasks = append(tasks, task{"install OpenSSH server", func() error {
		return s.ensureSSHServer(ctx, name, image)
	}})

	errs := make(chan error, len(tasks))
	for _, t := range tasks {
		go func() {
			if err := t.run(); err != nil {
				errs <- fmt.Errorf("%s: %w", t.name, err)
			} else {
				errs <- nil
			}
		}()
	}
	var firstErr error
	for range tasks {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return s.enableSSH(ctx, name, image, password != "")
}

// installSSHKey writes /root/.ssh/authorized_keys via the file API, so key
// characters never travel through a shell.
func (s *Service) installSSHKey(ctx context.Context, name, key string) error {
	ctx, cancel := context.WithTimeout(ctx, sshCommandTimeout)
	defer cancel()

	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", "mkdir -p /root/.ssh && chmod 700 /root/.ssh"); err != nil {
		return fmt.Errorf("prepare /root/.ssh: %w", err)
	}
	if err := s.app.Incus.PushFile(ctx, name, "/root/.ssh/authorized_keys", key+"\n", 0, 0, 0o600); err != nil {
		return err
	}
	return nil
}

// setRootPassword pipes the credential to chpasswd over the exec channel, so
// the password never appears in a command line or process listing.
func (s *Service) setRootPassword(ctx context.Context, name, password string) error {
	ctx, cancel := context.WithTimeout(ctx, sshCommandTimeout)
	defer cancel()

	if _, err := s.app.Incus.Exec(ctx, name, "root:"+password, "chpasswd"); err != nil {
		return fmt.Errorf("set root password: %w", err)
	}
	return nil
}

// ensureSSHServer installs the OpenSSH server when the image lacks it.
func (s *Service) ensureSSHServer(ctx context.Context, name string, image incus.Image) error {
	ctx, cancel := context.WithTimeout(ctx, sshCommandTimeout)
	defer cancel()

	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", "command -v sshd"); err == nil {
		return nil
	}

	var install string
	switch image.PackageManager {
	case "apt":
		// Fresh container images usually ship no apt lists, so an update
		// is needed; skip it when the cache is younger than a day, and
		// skip recommended extras (xauth and friends) sshd never needs.
		install = `test -n "$(find /var/lib/apt/lists -maxdepth 1 -name '*_InRelease' -mmin -1440 2>/dev/null)" || apt-get update` +
			` && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server`
	case "dnf":
		install = "dnf install -y --setopt=install_weak_deps=False openssh-server"
	case "apk":
		install = "apk add --no-cache openssh-server openssh-server-common openrc"
	default:
		return fmt.Errorf("no SSH install recipe for image %q", image.ID)
	}

	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", install); err != nil {
		return fmt.Errorf("install openssh-server: %w", err)
	}
	return nil
}

// enableSSH configures root login policy and starts the service.
func (s *Service) enableSSH(ctx context.Context, name string, image incus.Image, passwordAuth bool) error {
	ctx, cancel := context.WithTimeout(ctx, sshCommandTimeout)
	defer cancel()

	permitRoot := "prohibit-password"
	passAuth := "no"
	if passwordAuth {
		permitRoot = "yes"
		passAuth = "yes"
	}
	configure := fmt.Sprintf(
		`set -e
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin %s/' /etc/ssh/sshd_config
grep -q '^PermitRootLogin' /etc/ssh/sshd_config || echo 'PermitRootLogin %s' >> /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication %s/' /etc/ssh/sshd_config
grep -q '^PasswordAuthentication' /etc/ssh/sshd_config || echo 'PasswordAuthentication %s' >> /etc/ssh/sshd_config
command -v ssh-keygen >/dev/null 2>&1 && ssh-keygen -A || true`,
		permitRoot, permitRoot, passAuth, passAuth)

	unit := image.SSHUnit
	if unit == "" {
		unit = "sshd"
	}
	// Detect the init system once and run a single start command instead of
	// trying every known starter in turn: each attempt is an exec round-trip,
	// and a systemctl call without systemd running is pure dead time.
	// Restart first: a package install often auto-starts sshd with the
	// image default config, and enable/start is a no-op on a running
	// service, which would leave the old policy active.
	start := fmt.Sprintf(`
if [ -d /run/systemd/system ]; then
  systemctl restart %[1]s || systemctl enable --now %[1]s
elif command -v rc-service >/dev/null 2>&1; then
  rc-update add sshd default && rc-service sshd restart
elif command -v service >/dev/null 2>&1; then
  service %[1]s restart || service %[1]s start
else
  pkill -x sshd 2>/dev/null || true; sleep 1; /usr/sbin/sshd
fi`, unit)
	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", configure+"\n"+start); err != nil {
		return fmt.Errorf("configure and start sshd: %w", err)
	}

	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", "pgrep -x sshd 2>/dev/null || pidof sshd >/dev/null"); err != nil {
		return fmt.Errorf("sshd is not running: %w", err)
	}
	return nil
}

// WaitForAgent polls until exec works inside the instance. Containers
// answer immediately; virtual machines need their guest agent booted,
// which lags Start by a minute or more.
func (s *Service) WaitForAgent(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := s.app.Incus.Exec(attempt, name, "", "true")
		cancel()
		if err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("guest agent did not respond for %s", name)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ValidateSSHKey checks an SSH public key the way sshd expects it:
// "<type> <base64-blob> [comment]".
func ValidateSSHKey(key string) error {
	fields := strings.Fields(strings.TrimSpace(key))
	if len(fields) < 2 {
		return fmt.Errorf("SSH key must look like \"ssh-ed25519 AAAA...\"")
	}

	switch fields[0] {
	case "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384",
		"ecdsa-sha2-nistp521", "ssh-ed25519", "sk-ssh-ed25519@openssh.com",
		"sk-ecdsa-sha2-nistp256@openssh.com":
	default:
		return fmt.Errorf("unsupported SSH key type %q", fields[0])
	}

	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return fmt.Errorf("SSH key blob is not valid base64")
	}
	return nil
}
