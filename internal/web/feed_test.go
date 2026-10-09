package web

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan frame) frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame arrived")
		return frame{}
	}
}

func TestFeedRendersOncePerTickForAllSubscribers(t *testing.T) {
	var renders atomic.Int32
	f := newFeed(20*time.Millisecond, func() (string, string, error) {
		renders.Add(1)
		return "<p>x</p>", "same", nil
	})

	var chans []<-chan frame
	for i := 0; i < 20; i++ {
		ch, cancel := f.subscribe(context.Background())
		defer cancel()
		chans = append(chans, ch)
	}
	for _, ch := range chans {
		if got := recv(t, ch); got.HTML != "<p>x</p>" {
			t.Errorf("frame = %+v", got)
		}
	}

	before := renders.Load()
	time.Sleep(100 * time.Millisecond)
	ticks := renders.Load() - before
	if ticks > 8 { // ~5 expected for one renderer; 20 renderers would be ~100
		t.Errorf("%d renders in 100ms for 20 subscribers: not shared", ticks)
	}
}

func TestFeedOnlySendsChanges(t *testing.T) {
	var n atomic.Int32
	f := newFeed(10*time.Millisecond, func() (string, string, error) {
		v := n.Load()
		return "v" + strconv.Itoa(int(v)), "fp" + strconv.Itoa(int(v)), nil
	})
	ch, cancel := f.subscribe(context.Background())
	defer cancel()

	if got := recv(t, ch); got.Fingerprint != "fp0" {
		t.Fatalf("first = %+v", got)
	}
	select {
	case got := <-ch:
		t.Fatalf("unchanged content was re-sent: %+v", got)
	case <-time.After(60 * time.Millisecond):
	}

	n.Store(1)
	if got := recv(t, ch); got.Fingerprint != "fp1" {
		t.Fatalf("after change = %+v", got)
	}
}

func TestFeedLateSubscriberGetsCurrentState(t *testing.T) {
	f := newFeed(10*time.Millisecond, func() (string, string, error) { return "now", "fp", nil })
	first, cancelFirst := f.subscribe(context.Background())
	defer cancelFirst()
	recv(t, first)

	late, cancelLate := f.subscribe(context.Background())
	defer cancelLate()
	if got := recv(t, late); got.HTML != "now" {
		t.Errorf("late subscriber got %+v", got)
	}
}

func TestFeedStopsPollingWithoutSubscribers(t *testing.T) {
	var renders atomic.Int32
	f := newFeed(10*time.Millisecond, func() (string, string, error) {
		renders.Add(1)
		return "x", "fp", nil
	})
	ch, cancel := f.subscribe(context.Background())
	recv(t, ch)
	cancel()

	time.Sleep(30 * time.Millisecond) // let an in-flight tick finish
	settled := renders.Load()
	time.Sleep(100 * time.Millisecond)
	if renders.Load() != settled {
		t.Error("still rendering with nobody listening")
	}

	// And it starts again for the next subscriber, with fresh state.
	ch2, cancel2 := f.subscribe(context.Background())
	defer cancel2()
	recv(t, ch2)
}

func TestFeedStopsOnParentContext(t *testing.T) {
	var renders atomic.Int32
	f := newFeed(10*time.Millisecond, func() (string, string, error) {
		renders.Add(1)
		return "x", "fp", nil
	})
	ctx, stop := context.WithCancel(context.Background())
	ch, cancel := f.subscribe(ctx)
	defer cancel()
	recv(t, ch)

	stop()
	time.Sleep(30 * time.Millisecond)
	settled := renders.Load()
	time.Sleep(80 * time.Millisecond)
	if renders.Load() != settled {
		t.Error("kept rendering after shutdown")
	}
}
