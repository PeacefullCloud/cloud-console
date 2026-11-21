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

// EnsureConsoleUser creates the unprivileged account the web terminal logs
// into. It mirrors the AWS "ubuntu" user: a real home directory, a locked
// password (key auth only), the owner's SSH key, and a validated sudoers
// drop-in granting passwordless root. It is idempotent, so re-running it is
// safe. loginUser comes from config and is validated there before it can be
// interpolated into these shell commands.
func (s *Service) EnsureConsoleUser(ctx context.Context, name, key, loginUser string) error {
	ctx, cancel := context.WithTimeout(ctx, sshCommandTimeout)
	defer cancel()

	// Account, home, shell and group membership. useradd exists on glibc and
	// dnf images, adduser on Debian/Alpine; try both before giving up.
	setup := fmt.Sprintf(`
set -e
if ! id -u %[1]s >/dev/null 2>&1; then
  shell=/bin/bash
  [ -x "$shell" ] || shell=/bin/sh
  if command -v useradd >/dev/null 2>&1; then
    useradd -m -d /home/%[1]s -s "$shell" -U %[1]s
  elif command -v adduser >/dev/null 2>&1; then
    adduser -D -h /home/%[1]s -s "$shell" %[1]s
  else
    echo "no useradd/adduser available" >&2
    exit 1
  fi
fi
# Lock the password so only keys (and root's su) can authenticate.
passwd -l %[1]s >/dev/null 2>&1 || true
for group in sudo wheel admin; do
  getent group "$group" >/dev/null 2>&1 || continue
  if command -v usermod >/dev/null 2>&1; then
    usermod -aG "$group" %[1]s >/dev/null 2>&1 || true
  elif command -v addgroup >/dev/null 2>&1; then
    addgroup %[1]s "$group" >/dev/null 2>&1 || true
  fi
done
mkdir -p /home/%[1]s/.ssh
chmod 700 /home/%[1]s /home/%[1]s/.ssh
chown -R %[1]s /home/%[1]s 2>/dev/null || true
`, loginUser)
	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", setup); err != nil {
		return fmt.Errorf("create console user %s: %w", loginUser, err)
	}

	// Give the default user the owner's key as well, so
	// `ssh cloud-user@<private-ip>` works just like the web terminal.
	if key != "" {
		keyPath := "/home/" + loginUser + "/.ssh/authorized_keys"
		if err := s.app.Incus.PushFile(ctx, name, keyPath, key+"\n", 0, 0, 0o600); err != nil {
			return err
		}
		chown := fmt.Sprintf("chown %[1]s:%[1]s %[2]s 2>/dev/null || chown %[1]s %[2]s 2>/dev/null || true", loginUser, keyPath)
		if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", chown); err != nil {
			return fmt.Errorf("set ownership of console user SSH key: %w", err)
		}
	}

	// Passwordless sudo. The drop-in is parsed with visudo before it is kept:
	// a malformed rule would lock every administrator out of sudo.
	sudoers := fmt.Sprintf("# Managed by Peaceful Cloud Console - do not edit\n%s ALL=(ALL:ALL) NOPASSWD:ALL\n", loginUser)
	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", "mkdir -p /etc/sudoers.d && chmod 755 /etc/sudoers.d"); err != nil {
		return fmt.Errorf("prepare sudoers.d: %w", err)
	}
	path := "/etc/sudoers.d/99-cloud-console-" + loginUser
	if err := s.app.Incus.PushFile(ctx, name, path, sudoers, 0, 0, 0o440); err != nil {
		return err
	}
	verify := fmt.Sprintf("if command -v visudo >/dev/null 2>&1; then visudo -cf %[1]s; else echo 'visudo unavailable; skipping syntax check'; fi", path)
	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", verify); err != nil {
		_, _ = s.app.Incus.Exec(ctx, name, "", "rm", "-f", path)
		return fmt.Errorf("invalid sudoers rule for %s: %w", loginUser, err)
	}
	return nil
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

	// sudo is installed alongside sshd: the web console's default user needs
	// it for passwordless root, so an image with only sshd is not finished.
	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", "command -v sshd >/dev/null 2>&1 && command -v sudo >/dev/null 2>&1"); err == nil {
		return nil
	}

	var install string
	switch image.PackageManager {
	case "apt":
		// Fresh container images usually ship no apt lists, so an update
		// is needed; skip it when the cache is younger than a day, and
		// skip recommended extras (xauth and friends) sshd never needs.
		install = `test -n "$(find /var/lib/apt/lists -maxdepth 1 -name '*_InRelease' -mmin -1440 2>/dev/null)" || apt-get update` +
			` && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server sudo`
	case "dnf":
		install = "dnf install -y --setopt=install_weak_deps=False openssh-server sudo"
	case "apk":
		install = "apk add --no-cache openssh-server openssh-server-common openrc sudo"
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
