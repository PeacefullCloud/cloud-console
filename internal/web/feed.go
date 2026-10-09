package web

import (
	"context"
	"sync"
	"time"
)

// frame is one rendering of a live fragment, with a fingerprint that changes
// only when the content does.
type frame struct {
	HTML        string
	Fingerprint string
}

// feed renders a shared fragment once per interval for every open stream,
// instead of once per stream. Polling runs only while someone is subscribed.
type feed struct {
	interval time.Duration
	render   func() (html, fingerprint string, err error)

	mu      sync.Mutex
	subs    map[chan frame]struct{}
	last    frame
	hasLast bool
	cancel  context.CancelFunc
}

func newFeed(interval time.Duration, render func() (string, string, error)) *feed {
	return &feed{interval: interval, render: render, subs: map[chan frame]struct{}{}}
}

// subscribe returns a channel of frames: the latest one right away (when there
// is one), then one per change. A slow reader only ever misses frames it would
// have replaced anyway. Call the returned func when done.
func (f *feed) subscribe(parent context.Context) (<-chan frame, func()) {
	ch := make(chan frame, 1)

	f.mu.Lock()
	f.subs[ch] = struct{}{}
	if f.hasLast {
		ch <- f.last
	}
	if f.cancel == nil {
		ctx, cancel := context.WithCancel(parent)
		f.cancel = cancel
		f.hasLast = false
		go f.poll(ctx)
	}
	f.mu.Unlock()

	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, ch)
		if len(f.subs) == 0 && f.cancel != nil {
			f.cancel()
			f.cancel = nil
			f.hasLast = false
		}
	}
}

func (f *feed) poll(ctx context.Context) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	f.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.tick(ctx)
		}
	}
}

func (f *feed) tick(ctx context.Context) {
	html, fingerprint, err := f.render()
	if err != nil || ctx.Err() != nil {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Stale poller: the last subscriber left and a new run may have begun.
	if ctx.Err() != nil {
		return
	}
	if f.hasLast && f.last.Fingerprint == fingerprint {
		return
	}
	f.last, f.hasLast = frame{HTML: html, Fingerprint: fingerprint}, true
	for ch := range f.subs {
		select {
		case <-ch: // drop the frame the reader never picked up
		default:
		}
		ch <- f.last
	}
}
