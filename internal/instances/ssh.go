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

	if key != "" {
		if err := s.installSSHKey(ctx, name, key); err != nil {
			return err
		}
	}
	if password != "" {
		if err := s.setRootPassword(ctx, name, password); err != nil {
			return err
		}
	}
	if err := s.ensureSSHServer(ctx, name, image); err != nil {
		return err
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
		install = "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y openssh-server"
	case "dnf":
		install = "dnf install -y openssh-server"
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
		`sed -i 's/^#*PermitRootLogin.*/PermitRootLogin %s/' /etc/ssh/sshd_config; `+
			`grep -q '^PermitRootLogin' /etc/ssh/sshd_config || echo 'PermitRootLogin %s' >> /etc/ssh/sshd_config; `+
			`sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication %s/' /etc/ssh/sshd_config; `+
			`grep -q '^PasswordAuthentication' /etc/ssh/sshd_config || echo 'PasswordAuthentication %s' >> /etc/ssh/sshd_config`,
		permitRoot, permitRoot, passAuth, passAuth)
	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", configure); err != nil {
		return fmt.Errorf("configure sshd: %w", err)
	}

	unit := image.SSHUnit
	if unit == "" {
		unit = "sshd"
	}
	starts := []string{
		"systemctl enable --now " + unit,
		"service " + unit + " start",
		"rc-update add sshd default && rc-service sshd start",
		"/usr/sbin/sshd",
	}
	var lastErr error
	for _, start := range starts {
		if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", start); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return fmt.Errorf("start sshd: %w", lastErr)
	}

	if _, err := s.app.Incus.Exec(ctx, name, "", "sh", "-c", "pgrep -x sshd"); err != nil {
		return fmt.Errorf("sshd is not running: %w", err)
	}
	return nil
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
