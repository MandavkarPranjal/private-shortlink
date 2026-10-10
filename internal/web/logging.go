package web

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// identityHolder is the context key under which ServeHTTP stores the lazy
// identity for a request. The value is a *identityState, so handlers and the
// access log can share one resolution without a second WhoIs call.
type identityHolder struct{}

// identityState memoizes an identity lookup. Resolving is deferred until
// something actually asks, so the redirect path — the hottest path in the
// service, since every click is a redirect — costs no WhoIs round-trip to
// tailscaled.
type identityState struct {
	resolve IdentityFunc
	done    bool
	id      Identity
}

// get resolves the identity on first call and caches it.
func (s *identityState) get(r *http.Request) Identity {
	if !s.done {
		s.id = s.resolve(r)
		s.done = true
	}
	return s.id
}

// withIdentityState returns ctx carrying a lazily resolved identity for r.
func withIdentityState(ctx context.Context, r *http.Request, resolve IdentityFunc) context.Context {
	return context.WithValue(ctx, identityHolder{}, &identityState{resolve: resolve})
}

// IdentityFrom returns the identity that was already resolved for this request,
// and whether anything resolved it. It does not trigger a lookup, so it is safe
// on paths that must not pay for one; outside a request served by the Handler it
// reports false.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	h, ok := ctx.Value(identityHolder{}).(*identityState)
	if !ok || !h.done {
		return Identity{}, false
	}
	return h.id, true
}

// idOf returns the identity for r, resolving it at most once per request.
// Handlers call this instead of h.identity. In tailnet mode a resolution is a
// WhoIs call to tailscaled and in netbird mode a directory read under a lock,
// so a second call in the same request is free.
func (h *Handler) idOf(r *http.Request) Identity {
	if holder, ok := r.Context().Value(identityHolder{}).(*identityState); ok {
		return holder.get(r)
	}
	// Request did not come through ServeHTTP (a direct call in a test, say).
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

// ServeHTTP dispatches, recovers panics, and writes one structured access-log
// line per request.
//
// The identity is attached to the context unresolved and only looked up if a
// handler asks for it (see idOf). The access log therefore reports the peer
// rather than a login name unless something needed the identity anyway.
//
// Redirects and successful reads are logged at Debug: every click on a link is
// a redirect, so logging them at Info would bury everything else. Client errors
// are Warn and server errors are Error, so the default level surfaces exactly
// the requests that need attention.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withIdentityState(r.Context(), r, h.identity))

	if h.log == nil {
		serveRecovering(h.mux, w, r, nil)
		return
	}

	start := time.Now()
	rec := &responseRecorder{ResponseWriter: w}
	// Recovery happens inside the dispatch this function logs, so a panicking
	// request still produces an access line — with the status the response
	// actually got, not the one it would have got.
	serveRecovering(h.mux, rec, r, h.log)

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
		slog.String("who", requester(r)),
	)
}

// serveRecovering runs next, turning a panic in a handler into a 500 instead of
// a dropped connection. The process is left running: one bad request is not
// worth restarting the server.
//
// If rec is non-nil the caller records the response, so this must only try to
// write an error status when the handler has not already committed one.
func serveRecovering(next http.Handler, w http.ResponseWriter, r *http.Request, log *slog.Logger) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if v == http.ErrAbortHandler {
			panic(v) // the client hung up; not our problem to log
		}
		if log != nil {
			log.Error("panic serving request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())),
			)
		}
		// Once a handler has committed a status, net/http has already sent it
		// and an http.Error here would only append a body to a response that
		// claims to be something else. Record 500 for the log instead, and let
		// the connection close.
		if rec, ok := w.(*responseRecorder); ok && rec.status != 0 {
			rec.status = http.StatusInternalServerError
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
	}()
	next.ServeHTTP(w, r)
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

// requester labels the caller for the access log. It prefers an identity that a
// handler already resolved — no extra lookup — and otherwise falls back to the
// peer's address, so a redirect is still attributable without paying for a
// WhoIs on the hot path. Anonymous callers are marked so they are easy to count.
func requester(r *http.Request) string {
	if id, ok := IdentityFrom(r.Context()); ok {
		if id.ID == "" {
			return "anonymous"
		}
		return id.ID
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}
