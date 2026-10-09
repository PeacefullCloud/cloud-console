package incus

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

func fullNamed(name string) []api.InstanceFull {
	f := api.InstanceFull{}
	f.Name = name
	return []api.InstanceFull{f}
}

func TestListCacheServesRepeatCallsFromMemory(t *testing.T) {
	var c instanceListCache
	var calls atomic.Int32
	fetch := func() ([]api.InstanceFull, error) { calls.Add(1); return fullNamed("a"), nil }

	for i := 0; i < 5; i++ {
		if _, err := c.get(time.Minute, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("fetched %d times, want 1", calls.Load())
	}
}

func TestListCacheExpires(t *testing.T) {
	var c instanceListCache
	var calls atomic.Int32
	fetch := func() ([]api.InstanceFull, error) { calls.Add(1); return fullNamed("a"), nil }

	_, _ = c.get(10*time.Millisecond, fetch)
	time.Sleep(25 * time.Millisecond)
	_, _ = c.get(10*time.Millisecond, fetch)
	if calls.Load() != 2 {
		t.Errorf("fetched %d times, want 2", calls.Load())
	}
}

func TestListCacheInvalidate(t *testing.T) {
	var c instanceListCache
	var n atomic.Int32
	fetch := func() ([]api.InstanceFull, error) {
		return fullNamed(string(rune('a' + n.Add(1) - 1))), nil
	}

	first, _ := c.get(time.Minute, fetch)
	c.invalidate()
	second, _ := c.get(time.Minute, fetch)
	if first[0].Name == second[0].Name {
		t.Error("invalidate did not force a fresh query")
	}
}

func TestListCacheSharesOneInflightQuery(t *testing.T) {
	var c instanceListCache
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func() ([]api.InstanceFull, error) {
		calls.Add(1)
		<-release
		return fullNamed("a"), nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get(time.Minute, fetch); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d concurrent queries, want 1", calls.Load())
	}
}

func TestListCacheDoesNotKeepErrors(t *testing.T) {
	var c instanceListCache
	var calls atomic.Int32
	fetch := func() ([]api.InstanceFull, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("boom")
		}
		return fullNamed("a"), nil
	}
	if _, err := c.get(time.Minute, fetch); err == nil {
		t.Fatal("expected the first error")
	}
	if _, err := c.get(time.Minute, fetch); err != nil {
		t.Fatalf("a failure was cached: %v", err)
	}
}

func TestListCacheDropsAnswerThatPredatesAChange(t *testing.T) {
	var c instanceListCache
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func() ([]api.InstanceFull, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return fullNamed("stale"), nil
		}
		return fullNamed("fresh"), nil
	}

	done := make(chan struct{})
	go func() { _, _ = c.get(time.Minute, fetch); close(done) }()
	<-started
	c.invalidate() // an instance changed while the first query was running
	close(release)
	<-done

	got, _ := c.get(time.Minute, fetch)
	if got[0].Name != "fresh" {
		t.Errorf("served %q after a change", got[0].Name)
	}
}
