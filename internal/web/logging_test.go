package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mandavkarpranjal/private-shortlink/internal/store"
)

// captureLogs returns a logger writing JSON lines into the returned buffer, so
// tests can assert on structured fields rather than formatted text.
func captureLogs(t *testing.T, level slog.Level) (*slog.Logger, *strings.Builder) {
	t.Helper()
	var sb strings.Builder
	h := slog.NewJSONHandler(&sb, &slog.HandlerOptions{Level: level})
	return slog.New(h), &sb
}

// logRecords decodes the JSON lines a captureLogs logger produced.
func logRecords(t *testing.T, sb *strings.Builder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(sb.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestRequestLogging(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	// Two handlers over one store: alice owns the link, bob asks for it.
	h, st := newTestHandler(t, nil, false)
	link := &store.Link{Name: "blog", URL: "https://example.com/blog", Owner: "alice"}
	if err := st.Save(context.Background(), link, false); err != nil {
		t.Fatal(err)
	}
	h.log = logger

	bob, err := New(Config{
		Store:    st,
		Identity: func(*http.Request) Identity { return Identity{ID: "bob"} },
		Logger:   logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	do(t, h, "GET", "/blog", nil, nil)          // 302 redirect
	do(t, bob, "GET", "/-/edit/blog", nil, nil) // 403
	do(t, h, "GET", "/missing", nil, nil)       // 404
	do(t, h, "GET", "/-/edit/blog", nil, nil)   // 200

	recs := logRecords(t, sb)
	if len(recs) != 4 {
		t.Fatalf("got %d log lines, want 4:\n%s", len(recs), sb.String())
	}

	want := []struct {
		path   string
		status float64
		level  string
		who    string
	}{
		// The redirect and the 404 resolve no identity — the first is the hot
		// path, the second needs no owner — so they are attributed to the peer.
		{"/blog", 302, "DEBUG", "192.0.2.1"},
		{"/-/edit/blog", 403, "WARN", "bob"},
		{"/missing", 404, "WARN", "192.0.2.1"},
		{"/-/edit/blog", 200, "DEBUG", "alice"},
	}
	for i, w := range want {
		got := recs[i]
		if got["path"] != w.path || got["status"] != w.status || got["level"] != w.level || got["who"] != w.who {
			t.Errorf("line %d = %v, want path=%s status=%v level=%s who=%s", i, got, w.path, w.status, w.level, w.who)
		}
		if got["method"] != "GET" {
			t.Errorf("line %d method = %v", i, got["method"])
		}
		if _, ok := got["duration"]; !ok {
			t.Errorf("line %d has no duration: %v", i, got)
		}
	}

	// The 200 edit page has a body; the redirect has none.
	if recs[3]["bytes"].(float64) <= 0 {
		t.Errorf("edit page bytes = %v, want > 0", recs[3]["bytes"])
	}
	// The recorder counts the body net/http writes alongside the 302 header.
	if recs[0]["bytes"].(float64) <= 0 {
		t.Errorf("redirect bytes = %v, want > 0", recs[0]["bytes"])
	}
}

func TestRequestLoggingRespectsLevel(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelWarn)
	h, _ := newTestHandler(t, nil, false)
	h.log = logger

	do(t, h, "GET", "/", nil, nil) // 200 -> Debug, filtered out
	if recs := logRecords(t, sb); len(recs) != 0 {
		t.Fatalf("info-level run logged %d lines, want 0:\n%s", len(recs), sb.String())
	}

	do(t, h, "GET", "/missing", nil, nil) // 404 -> Warn
	recs := logRecords(t, sb)
	if len(recs) != 1 || recs[0]["status"] != float64(404) {
		t.Fatalf("want a single 404 warning, got %v", recs)
	}
}

func TestAnonymousIdentityLogged(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h, _ := newTestHandler(t, func(*http.Request) Identity { return Identity{} }, false)
	h.log = logger

	do(t, h, "GET", "/", nil, nil)
	recs := logRecords(t, sb)
	if len(recs) != 1 {
		t.Fatalf("got %d lines, want 1", len(recs))
	}
	if recs[0]["who"] != "anonymous" {
		t.Errorf("who = %v, want \"anonymous\"", recs[0]["who"])
	}
}

func TestLoggingDisabledWhenLoggerNil(t *testing.T) {
	h, _ := newTestHandler(t, nil, false)
	if h.log != nil {
		t.Fatal("test handler should have no logger")
	}
	// Must not panic and must still dispatch.
	if rec := do(t, h, "GET", "/", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestIdentityResolvedOncePerRequest(t *testing.T) {
	calls := 0
	id := func(*http.Request) Identity {
		calls++
		return Identity{ID: "alice"}
	}
	h, st := newTestHandler(t, id, false)
	// A route that consults the identity more than once: the home page renders
	// the caller's own links and whether they may edit each one.
	before := calls
	do(t, h, "GET", "/", nil, nil)
	if got := calls - before; got != 1 {
		t.Errorf("identity resolved %d times, want 1", got)
	}
	if _, err := st.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityFromContext(t *testing.T) {
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Error("IdentityFrom on a bare context should report false")
	}

	req := httptest.NewRequest("GET", "/", nil)
	ctx := withIdentityState(context.Background(), req,
		func(*http.Request) Identity { return Identity{ID: "alice", IsAdmin: true} })

	// Reading the context must not be what triggers the lookup.
	if _, ok := IdentityFrom(ctx); ok {
		t.Error("IdentityFrom should not resolve on its own; only a handler may")
	}

	h, _ := newTestHandler(t, func(*http.Request) Identity {
		return Identity{ID: "alice", IsAdmin: true}
	}, false)
	req = req.WithContext(ctx)
	got := h.idOf(req)
	if got.ID != "alice" || !got.IsAdmin {
		t.Fatalf("idOf = %+v", got)
	}
	if cached, ok := IdentityFrom(ctx); !ok || cached != got {
		t.Errorf("IdentityFrom = %+v, %v; want the cached %+v, true", cached, ok, got)
	}
	// And the cache is what a second reader sees, with no further lookup.
	if again := h.idOf(req); again != got {
		t.Errorf("second idOf = %+v, want %+v", again, got)
	}
}

// A redirect must not resolve an identity: that lookup is a WhoIs call to
// tailscaled, and every click on a link is a redirect.
func TestRedirectDoesNotResolveIdentity(t *testing.T) {
	calls := 0
	h, st := newTestHandler(t, func(*http.Request) Identity {
		calls++
		return Identity{ID: "alice"}
	}, false)
	if err := st.Save(context.Background(), &store.Link{Name: "x", URL: "http://example.com"}, false); err != nil {
		t.Fatal(err)
	}
	before := calls

	if rec := do(t, h, "GET", "/x", nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := calls - before; got != 0 {
		t.Errorf("redirect resolved the identity %d times, want 0", got)
	}
}

// The access log still has to attribute the request, using the peer address
// when no handler needed the identity.
func TestRedirectLoggedWithPeer(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h, st := newTestHandler(t, func(*http.Request) Identity { return Identity{ID: "alice"} }, false)
	h.log = logger
	if err := st.Save(context.Background(), &store.Link{Name: "x", URL: "http://example.com"}, false); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "100.64.0.7:41234"
	h.ServeHTTP(httptest.NewRecorder(), req)

	recs := logRecords(t, sb)
	if len(recs) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(recs), sb.String())
	}
	if recs[0]["who"] != "100.64.0.7" {
		t.Errorf("who = %v, want the peer address 100.64.0.7", recs[0]["who"])
	}
}

// A handler that needs the identity gets the login name, not the peer address,
// and still only one lookup for the whole request.
func TestAccessLogUsesResolvedIdentity(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	calls := 0
	h, _ := newTestHandler(t, func(*http.Request) Identity {
		calls++
		return Identity{ID: "alice"}
	}, false)
	h.log = logger

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "100.64.0.7:41234"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if calls != 1 {
		t.Errorf("identity resolved %d times, want 1", calls)
	}
	recs := logRecords(t, sb)
	if len(recs) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(recs), sb.String())
	}
	if recs[0]["who"] != "alice" {
		t.Errorf("who = %v, want alice", recs[0]["who"])
	}
}

func TestServeRecovering(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := httptest.NewRecorder()
	serveRecovering(h, rec, httptest.NewRequest("GET", "/panic", nil), logger)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	recs := logRecords(t, sb)
	if len(recs) != 1 {
		t.Fatalf("got %d log lines, want 1:\n%s", len(recs), sb.String())
	}
	if recs[0]["level"] != "ERROR" || recs[0]["panic"] != "boom" {
		t.Errorf("log = %v", recs[0])
	}
	if s, _ := recs[0]["stack"].(string); !strings.Contains(s, "TestServeRecovering") {
		t.Errorf("stack does not mention the panicking test: %q", recs[0]["stack"])
	}
}

func TestServeRecoveringRethrowsAbortHandler(t *testing.T) {
	logger, _ := captureLogs(t, slog.LevelDebug)
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", r)
		}
	}()
	serveRecovering(h, httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), logger)
	t.Error("expected the panic to propagate")
}

// A panicking request must still produce an access line, or the one request
// most worth investigating is the only one missing from the log.
func TestPanicStillLogsAccessLine(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h, _ := newTestHandler(t, nil, false)
	h.log = logger
	// Dispatch that panics after committing a status, the case where the
	// recovery cannot rewrite the response.
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("/x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("boom")
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))

	recs := logRecords(t, sb)
	var sawError, sawRequest bool
	for _, rec := range recs {
		if rec["msg"] == "panic serving request" {
			sawError = true
		}
		if rec["msg"] == "request" && rec["status"] == float64(500) {
			sawRequest = true
		}
	}
	if !sawError {
		t.Errorf("no panic line:\n%s", sb.String())
	}
	if !sawRequest {
		t.Errorf("panicking request was not logged as a 500:\n%s", sb.String())
	}
	// The response kept the status the handler committed: net/http already sent
	// it, so the error body must not be spliced onto it.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the committed 200", rec.Code)
	}
}

func TestServeRecoveringPassesThrough(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	serveRecovering(h, rec, httptest.NewRequest("GET", "/", nil), logger)
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	if sb.String() != "" {
		t.Errorf("unexpected log output: %s", sb.String())
	}
}
