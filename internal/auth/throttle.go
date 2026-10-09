package auth

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Throttle is a sliding-window failure counter. Once a key has accumulated
// limit failures inside the window it is blocked until the oldest of them
// ages out.
//
// State lives in memory: like the TOTP challenges, the console is one process,
// and a restart clearing the counters is an acceptable trade for no schema.
type Throttle struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time
}

// maxThrottleKeys bounds memory when an attacker sprays many distinct keys.
const maxThrottleKeys = 10000

// NewThrottle allows limit failures per key within window.
func NewThrottle(limit int, window time.Duration) *Throttle {
	return &Throttle{
		limit:    limit,
		window:   window,
		now:      time.Now,
		failures: map[string][]time.Time{},
	}
}

// normaliseKey keeps keys case-insensitive and bounded in size.
func normaliseKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if len(key) > 128 {
		key = key[:128]
	}
	return key
}

// recent drops failures that left the window and returns what remains.
// The caller must hold t.mu.
func (t *Throttle) recent(key string, now time.Time) []time.Time {
	list := t.failures[key]
	cutoff := now.Add(-t.window)

	keep := list[:0]
	for _, ts := range list {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}

	if len(keep) == 0 {
		delete(t.failures, key)
		return nil
	}
	t.failures[key] = keep
	return keep
}

// Blocked reports whether key has used up its failures, and for how much
// longer it stays blocked.
func (t *Throttle) Blocked(key string) (bool, time.Duration) {
	key = normaliseKey(key)
	if key == "" {
		return false, 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	list := t.recent(key, now)
	if len(list) < t.limit {
		return false, 0
	}

	// The block lifts when enough of the oldest failures have aged out to
	// drop the count back under the limit.
	lifts := list[len(list)-t.limit].Add(t.window)
	return true, lifts.Sub(now)
}

// Fail records one failure for key.
func (t *Throttle) Fail(key string) {
	key = normaliseKey(key)
	if key == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if len(t.failures) >= maxThrottleKeys {
		t.sweep(now)
	}
	t.recent(key, now)
	t.failures[key] = append(t.failures[key], now)
}

// Reset forgets key's failures, e.g. after a successful sign-in.
func (t *Throttle) Reset(key string) {
	key = normaliseKey(key)

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
}

// sweep removes every key whose failures have all aged out. If the table is
// still full of live keys, it is cleared: failing open for a moment beats
// letting an attacker grow memory without bound.
func (t *Throttle) sweep(now time.Time) {
	for key := range t.failures {
		t.recent(key, now)
	}
	if len(t.failures) >= maxThrottleKeys {
		t.failures = map[string][]time.Time{}
	}
}

// LockoutError is returned when sign-in attempts are temporarily refused.
type LockoutError struct {
	RetryAfter time.Duration
}

func (e *LockoutError) Error() string {
	return "too many sign-in attempts — try again in " + humanWait(e.RetryAfter)
}

// humanWait renders a wait as "N minutes", rounding up so it is never "0".
func humanWait(d time.Duration) string {
	minutes := int((d + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}
