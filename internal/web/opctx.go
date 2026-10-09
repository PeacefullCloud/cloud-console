package web

import (
	"context"
	"net/http"
	"time"
)

// operationTimeout bounds a mutating Incus call that has been cut loose from
// its HTTP request.
const operationTimeout = 10 * time.Minute

// operationContext returns the context for a state-changing action.
//
// The request context is cancelled when the browser tab closes or the
// connection drops, which would abort a stop, delete or rename halfway and
// leave the instance in a state nobody asked for. Values (user, CSRF) are kept;
// only cancellation is dropped, and a timeout replaces it.
func operationContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), operationTimeout)
}
