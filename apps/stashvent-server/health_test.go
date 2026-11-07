package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	tests := []struct {
		name   string
		method string
		want   int
	}{
		{"GET returns 200", http.MethodGet, http.StatusOK},
		{"POST is not allowed", http.MethodPost, http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler()
			rec := do(t, newMux(h), tt.method, "/healthz", "", nil)
			if rec.Code != tt.want {
				t.Errorf("%s /healthz = %d, want %d", tt.method, rec.Code, tt.want)
			}
		})
	}
}

func TestReadyz(t *testing.T) {
	errDown := errors.New("dial tcp 10.0.0.7:5432: connection refused")
	up := func(context.Context) error { return nil }
	down := func(context.Context) error { return errDown }

	tests := []struct {
		name       string
		method     string
		postgres   func(context.Context) error
		storage    func(context.Context) error
		wantStatus int
		wantBody   string
		wantLogged []string // names of the checks reported as failed
	}{
		{"everything reachable", http.MethodGet, up, up, http.StatusOK, "ok\n", nil},
		{"postgres down", http.MethodGet, down, up, http.StatusServiceUnavailable, "not ready\n", []string{"postgres"}},
		{"storage down", http.MethodGet, up, down, http.StatusServiceUnavailable, "not ready\n", []string{"storage"}},
		{"both down", http.MethodGet, down, down, http.StatusServiceUnavailable, "not ready\n", []string{"postgres", "storage"}},
		{"POST is not allowed", http.MethodPost, up, up, http.StatusMethodNotAllowed, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler()
			var logs bytes.Buffer
			h.logger = newTestLogger(&logs, slog.LevelInfo)
			h.ready = &readiness{checks: []readyCheck{
				{name: "postgres", ping: tt.postgres},
				{name: "storage", ping: tt.storage},
			}}

			rec := do(t, newMux(h), tt.method, "/readyz", "", nil)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body, tt.wantBody)
			}
			// Which dependency failed, and why, is for the logs only.
			if strings.Contains(rec.Body.String(), "10.0.0.7") {
				t.Errorf("response carries the internal error: %s", rec.Body)
			}
			var failed []string
			for _, line := range logLines(t, &logs) {
				if line["msg"] == "readiness check failed" {
					failed = append(failed, line["check"].(string))
				}
			}
			if strings.Join(failed, ",") != strings.Join(tt.wantLogged, ",") {
				t.Errorf("checks logged as failed = %v, want %v", failed, tt.wantLogged)
			}
		})
	}
}

// Every check gets a deadline, so one that hangs can't hang /readyz.
func TestReadyzChecksHaveADeadline(t *testing.T) {
	var left time.Duration
	rd := &readiness{checks: []readyCheck{{name: "storage", ping: func(ctx context.Context) error {
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		return nil
	}}}}

	rd.ok(t.Context(), time.Now, slog.New(slog.DiscardHandler))

	if left <= 0 || left > readyCheckTimeout {
		t.Errorf("check had %v left, want up to %v", left, readyCheckTimeout)
	}
}

// /readyz is open to anyone and not rate-limited, so a burst of requests must
// not become a burst of Postgres and storage calls.
func TestReadyzReusesTheResult(t *testing.T) {
	h, _ := newTestHandler()
	now := time.Now()
	h.now = func() time.Time { return now }
	calls := 0
	failing := false
	h.ready = &readiness{checks: []readyCheck{{name: "postgres", ping: func(context.Context) error {
		calls++
		if failing {
			return errors.New("down")
		}
		return nil
	}}}}
	mux := newMux(h)

	steps := []struct {
		name       string
		advance    time.Duration
		fail       bool
		wantStatus int
		wantCalls  int
	}{
		{"first request checks", 0, false, http.StatusOK, 1},
		{"a moment later reuses it", readyCacheTTL / 2, true, http.StatusOK, 1},
		{"after the TTL checks again", readyCacheTTL, true, http.StatusServiceUnavailable, 2},
		{"a failure is reused too", readyCacheTTL / 2, false, http.StatusServiceUnavailable, 2},
		{"and recovers after the TTL", readyCacheTTL, false, http.StatusOK, 3},
	}
	for _, s := range steps {
		now = now.Add(s.advance)
		failing = s.fail
		rec := do(t, mux, http.MethodGet, "/readyz", "", nil)
		if rec.Code != s.wantStatus || calls != s.wantCalls {
			t.Fatalf("%s: status = %d after %d checks, want %d after %d", s.name, rec.Code, calls, s.wantStatus, s.wantCalls)
		}
	}
}

// With a dependency timing out, a check takes longer than the result is
// reused for. Requests that queued behind it must still get its result,
// instead of each running the slow checks again.
func TestReadyzReusesASlowResult(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	calls := 0
	rd := &readiness{checks: []readyCheck{{name: "postgres", ping: func(context.Context) error {
		calls++
		now = now.Add(readyCheckTimeout) // the check itself takes this long
		return errors.New("timed out")
	}}}}
	logger := slog.New(slog.DiscardHandler)

	rd.ok(t.Context(), clock, logger)
	// What a request sees when it gets the lock right after that check.
	rd.ok(t.Context(), clock, logger)

	if calls != 1 {
		t.Errorf("%d rounds of checks, want 1: the second request should reuse the first one's result", calls)
	}
}

// The checks don't wait for each other, so two dependencies that are both
// down cost one timeout, not two.
func TestReadyzChecksRunTogether(t *testing.T) {
	var bothStarted sync.WaitGroup
	bothStarted.Add(2)
	// Returns only once the other check has started too.
	ping := func(ctx context.Context) error {
		bothStarted.Done()
		met := make(chan struct{})
		go func() {
			bothStarted.Wait()
			close(met)
		}()
		select {
		case <-met:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	rd := &readiness{checks: []readyCheck{{name: "postgres", ping: ping}, {name: "storage", ping: ping}}}

	if !rd.ok(t.Context(), time.Now, slog.New(slog.DiscardHandler)) {
		t.Error("the second check only started after the first had timed out")
	}
}

// One client giving up must not turn the shared result into "not ready".
func TestReadyzIgnoresClientDisconnect(t *testing.T) {
	rd := &readiness{checks: []readyCheck{{name: "postgres", ping: func(ctx context.Context) error { return ctx.Err() }}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if !rd.ok(ctx, time.Now, slog.New(slog.DiscardHandler)) {
		t.Error("not ready because the asking client had disconnected")
	}
}
