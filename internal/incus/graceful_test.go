package incus

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGracefulChange(t *testing.T) {
	boom := errors.New("operation timed out")

	t.Run("clean shutdown is plain success", func(t *testing.T) {
		var gotTimeout time.Duration
		err := gracefulChange(context.Background(), func(d time.Duration) error {
			gotTimeout = d
			return nil
		}, func() bool { t.Fatal("must not probe state on success"); return false })
		if err != nil {
			t.Fatal(err)
		}
		if gotTimeout != gracefulTimeout {
			t.Errorf("timeout = %v, want %v", gotTimeout, gracefulTimeout)
		}
	})

	t.Run("still running after failure points at force stop", func(t *testing.T) {
		err := gracefulChange(context.Background(), func(time.Duration) error { return boom },
			func() bool { return true })
		if !errors.Is(err, ErrNotShutDown) {
			t.Fatalf("want ErrNotShutDown, got %v", err)
		}
	})

	t.Run("failure on an instance that is already down stays the original error", func(t *testing.T) {
		err := gracefulChange(context.Background(), func(time.Duration) error { return boom },
			func() bool { return false })
		if !errors.Is(err, boom) || errors.Is(err, ErrNotShutDown) {
			t.Fatalf("want the original error, got %v", err)
		}
	})

	t.Run("cancelled request is not blamed on the guest", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := gracefulChange(ctx, func(time.Duration) error { return context.Canceled },
			func() bool { t.Fatal("must not probe state after cancellation"); return true })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}
