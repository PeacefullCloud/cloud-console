package instances

import (
	"context"
	"errors"
	"testing"

	"github.com/peaceful/cloud-console/internal/config"
	"github.com/peaceful/cloud-console/internal/core"
)

func TestCapacityRefusesOnlyImpossibleRequests(t *testing.T) {
	host := struct {
		cpus int
		mem  int64
	}{8, 16 << 30}

	cases := []struct {
		name string
		req  CreateRequest
		ok   bool
	}{
		{"fits", CreateRequest{CPU: 4, MemoryMB: 8192}, true},
		{"exactly the host", CreateRequest{CPU: 8, MemoryMB: 16384}, true},
		{"too many cpus", CreateRequest{CPU: 9, MemoryMB: 1024}, false},
		{"too much memory", CreateRequest{CPU: 1, MemoryMB: 16385}, false},
	}
	for _, tc := range cases {
		err := checkCapacity(tc.req, host.cpus, host.mem)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if err != nil && !errors.Is(err, ErrLimitsTooBig) {
			t.Errorf("%s: wrong error type: %v", tc.name, err)
		}
	}

	// Unknown host figures must not block creation.
	if err := checkCapacity(CreateRequest{CPU: 64, MemoryMB: 1 << 20}, 0, 0); err != nil {
		t.Errorf("unknown host refused: %v", err)
	}
}

func TestQuotaDisabledWhenZero(t *testing.T) {
	s := New(&core.App{Cfg: &config.Config{MaxInstances: 0}})
	if err := s.checkQuota(context.Background()); err != nil {
		t.Errorf("quota 0 should mean unlimited, got %v", err)
	}
}
