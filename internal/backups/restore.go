package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/incus"
)

// restoreHost is the slice of the Incus client a restore needs, so the swap
// logic can be tested without a daemon.
type restoreHost interface {
	Instance(name string) (*incusapi.Instance, error)
	ImportBackup(ctx context.Context, name string, r io.Reader, pool string) error
	Stop(ctx context.Context, name string) error
	ForceStop(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	RenameInstance(ctx context.Context, oldName, newName string) error
	DeleteInstance(ctx context.Context, name string) error
}

// restoreOutcome reports what a successful restore left behind.
type restoreOutcome struct {
	// Leftover names a renamed copy of the replaced instance that could not be
	// deleted. It still holds the old data.
	Leftover string
	// StartErr is set when there was no earlier instance to fall back to and
	// the restored one would not start.
	StartErr error
}

func tempName(prefix string) string {
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	return prefix + hex.EncodeToString(raw)
}

// restoreInstance replaces the instance called name with the contents of the
// archive in r.
//
// The live instance is not touched until the archive has imported cleanly
// under a temporary name. It is then moved aside rather than deleted, the
// import is renamed into place and started, and only a restored instance that
// actually starts lets the old copy go. A failure at any earlier point puts
// the original back and removes the half-finished import.
func restoreInstance(ctx context.Context, host restoreHost, name string, r io.Reader, progress func(pct int, msg string)) (restoreOutcome, error) {
	var out restoreOutcome

	existing, err := host.Instance(name)
	hadExisting := err == nil
	if err != nil && !incus.IsNotFound(err) {
		return out, fmt.Errorf("look up %s: %w", name, err)
	}
	wasRunning := hadExisting && existing.Status == "Running"

	// Cleanup and rollback must still run when the job's context is cancelled.
	bg := context.WithoutCancel(ctx)

	staging := tempName("console-restore-")
	progress(40, "Importing the backup next to the current instance")
	if err := host.ImportBackup(ctx, staging, r, ""); err != nil {
		discard(bg, host, staging)
		return out, fmt.Errorf("import backup: %w (the current instance was not changed)", err)
	}

	aside := ""
	if hadExisting {
		aside = tempName("console-old-")

		progress(60, "Stopping the current instance")
		if existing.Status != "Stopped" {
			if err := stopForRestore(ctx, host, name); err != nil {
				discard(bg, host, staging)
				return out, fmt.Errorf("stop the current instance: %w (it was not changed)", err)
			}
		}

		progress(65, "Moving the current instance aside")
		if err := host.RenameInstance(ctx, name, aside); err != nil {
			discard(bg, host, staging)
			if wasRunning {
				_ = host.Start(bg, name)
			}
			return out, fmt.Errorf("move the current instance aside: %w (it was not changed)", err)
		}
	}

	progress(70, "Putting the restored instance in place")
	if err := host.RenameInstance(ctx, staging, name); err != nil {
		discard(bg, host, staging)
		restoreOriginal(bg, host, name, aside, wasRunning)
		return out, fmt.Errorf("put the restored instance in place: %w (the original was put back)", err)
	}

	progress(80, "Starting the restored instance")
	if err := host.Start(ctx, name); err != nil {
		if !hadExisting {
			out.StartErr = err
			return out, nil
		}
		// A restore that does not boot must not cost the working instance.
		discard(bg, host, name)
		restoreOriginal(bg, host, name, aside, wasRunning)
		return out, fmt.Errorf("the restored instance did not start: %w (the original was put back)", err)
	}

	if hadExisting {
		progress(88, "Removing the replaced instance")
		if err := host.DeleteInstance(bg, aside); err != nil {
			out.Leftover = aside
		}
	}
	return out, nil
}

// stopForRestore shuts the instance down cleanly and powers it off only if it
// ignores the request: the user already agreed to replace it.
func stopForRestore(ctx context.Context, host restoreHost, name string) error {
	err := host.Stop(ctx, name)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if !errors.Is(err, incus.ErrNotShutDown) {
		return err
	}
	return host.ForceStop(ctx, name)
}

// restoreOriginal moves the aside copy back under its real name.
func restoreOriginal(ctx context.Context, host restoreHost, name, aside string, wasRunning bool) {
	if aside == "" {
		return
	}
	if err := host.RenameInstance(ctx, aside, name); err != nil {
		return
	}
	if wasRunning {
		_ = host.Start(ctx, name)
	}
}

// discard force-removes an instance created by a failed restore.
func discard(ctx context.Context, host restoreHost, name string) {
	if _, err := host.Instance(name); err != nil {
		return
	}
	_ = host.ForceStop(ctx, name)
	_ = host.DeleteInstance(ctx, name)
}
