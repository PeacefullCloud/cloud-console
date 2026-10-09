package incus

import (
	"sync"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

// instanceListTTL bounds how stale a shared instance list can be when
// something changed it behind the console's back (the incus CLI, say).
const instanceListTTL = 2 * time.Second

// instanceListCache holds the last full instance list and lets concurrent
// callers share one in-flight query.
type instanceListCache struct {
	mu       sync.Mutex
	value    []api.InstanceFull
	fetched  time.Time
	valid    bool
	inflight *listCall
	// generation changes on invalidate so a query that started before a
	// change cannot store its (older) answer afterwards.
	generation uint64
}

type listCall struct {
	done  chan struct{}
	value []api.InstanceFull
	err   error
}

func (l *instanceListCache) get(ttl time.Duration, fetch func() ([]api.InstanceFull, error)) ([]api.InstanceFull, error) {
	l.mu.Lock()
	if l.valid && time.Since(l.fetched) < ttl {
		v := l.value
		l.mu.Unlock()
		return v, nil
	}
	if call := l.inflight; call != nil {
		l.mu.Unlock()
		<-call.done
		return call.value, call.err
	}

	call := &listCall{done: make(chan struct{})}
	l.inflight = call
	gen := l.generation
	l.mu.Unlock()

	call.value, call.err = fetch()

	l.mu.Lock()
	if l.inflight == call {
		l.inflight = nil
	}
	if call.err == nil && l.generation == gen {
		l.value, l.fetched, l.valid = call.value, time.Now(), true
	}
	l.mu.Unlock()
	close(call.done)
	return call.value, call.err
}

func (l *instanceListCache) invalidate() {
	l.mu.Lock()
	l.valid = false
	l.value = nil
	l.generation++
	// A query already running may predate the change; later callers must
	// start their own rather than join it.
	l.inflight = nil
	l.mu.Unlock()
}
