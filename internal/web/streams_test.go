package web

import (
	"context"
	"testing"
	"time"
)

// An open SSE stream must unblock when the server shuts down. Otherwise
// http.Server.Shutdown waits out its timeout on Ctrl+C and the process dies
// with "context deadline exceeded".
func TestCloseEventStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{streamCtx: ctx, stopStreams: cancel}

	// Simulate a handler blocked in select on the stream context.
	released := make(chan struct{})
	go func() {
		select {
		case <-s.streamCtx.Done():
			close(released)
		case <-time.After(5 * time.Second):
		}
	}()

	s.CloseEventStreams()

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("SSE stream did not unblock on shutdown")
	}

	// Closing twice must not panic.
	s.CloseEventStreams()
}
