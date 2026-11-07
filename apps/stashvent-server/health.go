package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Route patterns of the health endpoints. They are not rate-limited, and
// their request lines are logged at debug level.
const (
	routeHealthz = "GET /healthz"
	routeReadyz  = "GET /readyz"
)

const (
	readyCheckTimeout = 2 * time.Second
	// readyCacheTTL is how long a readiness result is reused. /readyz needs no
	// token and isn't rate-limited, so without it every request would cost a
	// Postgres ping and a storage call.
	readyCacheTTL = time.Second
)

// readyCheck is one dependency the server needs before it can serve.
type readyCheck struct {
	name string
	ping func(ctx context.Context) error
}

// readiness reports whether every check passes.
type readiness struct {
	checks []readyCheck

	mu      sync.Mutex
	checked time.Time
	ready   bool
}

// ok runs the checks, or returns the last result if it is younger than
// readyCacheTTL. A failed check is logged by name; callers only learn yes or no.
func (rd *readiness) ok(ctx context.Context, now func() time.Time, logger *slog.Logger) bool {
	rd.mu.Lock()
	defer rd.mu.Unlock()

	// The clock is read after the wait for the lock: a request that queued
	// behind a running check must find that check's result still fresh,
	// instead of running the checks again.
	if age := now().Sub(rd.checked); !rd.checked.IsZero() && age >= 0 && age < readyCacheTTL {
		return rd.ready
	}

	// The result is shared, so one client disconnecting must not fail it.
	ctx = context.WithoutCancel(ctx)
	// All at once: with every dependency down, the answer takes one timeout,
	// not one per check.
	errs := make([]error, len(rd.checks))
	var wg sync.WaitGroup
	for i, c := range rd.checks {
		wg.Go(func() {
			checkCtx, cancel := context.WithTimeout(ctx, readyCheckTimeout)
			defer cancel()
			errs[i] = c.ping(checkCtx)
		})
	}
	wg.Wait()

	rd.ready = true
	for i, err := range errs {
		if err != nil {
			rd.ready = false
			logger.WarnContext(ctx, "readiness check failed", slog.String("check", rd.checks[i].name), slog.Any("error", err))
		}
	}
	// Stamped when the checks end, not when they began: slow checks (a
	// dependency timing out) would otherwise store a result that is already
	// too old to reuse.
	rd.checked = now()
	return rd.ready
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok\n")
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	if !h.ready.ok(r.Context(), h.now, h.logger) {
		writeText(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}
	writeText(w, http.StatusOK, "ok\n")
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body)) // client disconnects are not actionable
}
