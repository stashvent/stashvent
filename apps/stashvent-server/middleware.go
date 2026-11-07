package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/crypto"
)

const (
	// requestTimeout bounds a handler's Postgres and storage calls. The
	// http.Server timeouts cover reading and writing, not a handler that waits.
	requestTimeout = 10 * time.Second
	maxLoggedAgent = 64
)

type requestIDKey struct{}

// requestIDHandler adds the request ID in the context, if there is one, to
// every log record, so lines written while serving a request can be matched
// to its request line.
type requestIDHandler struct {
	slog.Handler
}

func (h requestIDHandler) Handle(ctx context.Context, rec slog.Record) error {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		rec = rec.Clone()
		rec.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, rec)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// baseWriter returns the server's own ResponseWriter under any wrappers.
// http.MaxBytesReader must be given that one: after an oversized body it
// tells the writer to close the connection, and it doesn't look through a
// wrapper to find it.
func baseWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = wrapper.Unwrap()
	}
}

// withRequestLog gives every request an ID (returned in X-Request-Id), a
// deadline, and one log line. A panic becomes a 500 instead of a dropped
// connection.
func (h *handler) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Always our own: an ID sent by the client would let it write into the logs.
		id := rand.Text()
		w.Header().Set("X-Request-Id", id)

		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestIDKey{}, id), requestTimeout)
		defer cancel()
		// The mux records the matched pattern on the request it is given, so
		// logRequest must read this copy.
		r = r.WithContext(ctx)
		rec := &statusRecorder{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				h.logger.ErrorContext(ctx, "handler panicked", slog.Any("panic", p), slog.String("stack", string(debug.Stack())))
				if rec.status == 0 {
					writeError(rec, http.StatusInternalServerError, api.CodeInternal, "Internal server error")
				}
			}
			h.logRequest(ctx, r, rec.status, time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}

// logRequest writes the request line. It logs the route pattern, never the
// URL, and no header except a shortened User-Agent: tokens, presigned URLs,
// query strings, and bodies must not reach the logs.
func (h *handler) logRequest(ctx context.Context, r *http.Request, status int, latency time.Duration) {
	if status == 0 {
		status = http.StatusOK // what net/http sends when a handler writes nothing
	}
	level := slog.LevelInfo
	if r.Pattern == routeHealthz || r.Pattern == routeReadyz {
		level = slog.LevelDebug // probes would fill the log
	}

	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("route", r.Pattern),
		slog.Int("status", status),
		slog.Float64("latency_ms", float64(latency.Microseconds())/1000),
	}
	if id := r.PathValue("id"); crypto.ValidShareID(id) {
		attrs = append(attrs, slog.String("share_id", id))
	}
	if agent := r.UserAgent(); agent != "" {
		attrs = append(attrs, slog.String("user_agent", agent[:min(len(agent), maxLoggedAgent)]))
	}
	h.logger.LogAttrs(ctx, level, "request", attrs...)
}
