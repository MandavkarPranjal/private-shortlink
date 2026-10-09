package web

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// identityKey is the context key under which ServeHTTP stores the identity it
// resolved for a request.
type identityKey struct{}

// withIdentity returns ctx carrying id.
func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the identity Handler.ServeHTTP resolved for r, if any.
// Outside a request served by the Handler it reports false.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// idOf returns the identity for r, reusing the one ServeHTTP put on the
// context. Handlers call this instead of h.identity so that one request costs
// exactly one identity lookup — in tailnet mode that lookup is a WhoIs call to
// tailscaled, and in netbird mode a directory read under a lock.
func (h *Handler) idOf(r *http.Request) Identity {
	if id, ok := IdentityFrom(r.Context()); ok {
		return id
	}
	return h.identity(r)
}

// responseRecorder captures the status code and response size for the access
// log. It only needs to satisfy the optional interfaces http.ResponseWriter
// exposes; the app serves no streaming or hijacked responses.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush lets the recorder stand in for handlers that flush (http.Flusher);
// flushing after the headers are written does not change the logged status.
func (w *responseRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ServeHTTP resolves the requester's identity once, dispatches, and writes one
// structured access-log line per request.
//
// Redirects and successful reads are logged at Debug: every click on a link is
// a redirect, so logging them at Info would bury everything else. Client
// errors are Warn and server errors are Error, so the default level surfaces
// exactly the requests that need attention.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := h.identity(r)
	r = r.WithContext(withIdentity(r.Context(), id))

	if h.log == nil {
		h.mux.ServeHTTP(w, r)
		return
	}

	start := time.Now()
	rec := &responseRecorder{ResponseWriter: w}
	h.mux.ServeHTTP(rec, r)

	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	h.log.LogAttrs(r.Context(), levelFor(status), "request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int("bytes", rec.bytes),
		slog.Duration("duration", time.Since(start)),
		slog.String("who", identityLabel(id)),
	)
}

func levelFor(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// identityLabel renders an identity for the log, preferring the resolved name
// and falling back to a marker so anonymous requests are easy to count.
func identityLabel(id Identity) string {
	if id.ID == "" {
		return "anonymous"
	}
	return id.ID
}

// LogPanics wraps next so a panic in a handler is logged with its stack and
// turned into a 500 instead of tearing down the connection silently. The
// process is left running: one bad request is not worth restarting the server.
func LogPanics(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // the client hung up; not our problem to log
			}
			log.Error("panic serving request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())),
			)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
