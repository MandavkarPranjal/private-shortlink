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
		{"/blog", 302, "DEBUG", "alice"},
		{"/-/edit/blog", 403, "WARN", "bob"},
		{"/missing", 404, "WARN", "alice"},
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
	before := calls
	do(t, h, "GET", "/-/api", nil, nil)
	if got := calls - before; got != 1 {
		t.Errorf("identity resolved %d times, want 1", got)
	}
	if _, err := st.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityFromContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := IdentityFrom(ctx); ok {
		t.Error("IdentityFrom on a bare context should report false")
	}
	id := Identity{ID: "alice", IsAdmin: true}
	got, ok := IdentityFrom(withIdentity(ctx, id))
	if !ok || got != id {
		t.Errorf("IdentityFrom = %+v, %v; want %+v, true", got, ok, id)
	}
}

func TestLogPanics(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h := LogPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), logger)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/panic", nil))
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
	if s, _ := recs[0]["stack"].(string); !strings.Contains(s, "TestLogPanics") {
		t.Errorf("stack does not mention the panicking test: %q", recs[0]["stack"])
	}
}

func TestLogPanicsRethrowsAbortHandler(t *testing.T) {
	logger, _ := captureLogs(t, slog.LevelDebug)
	h := LogPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), logger)

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	t.Error("expected the panic to propagate")
}

func TestLogPanicsPassesThrough(t *testing.T) {
	logger, sb := captureLogs(t, slog.LevelDebug)
	h := LogPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), logger)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	if sb.String() != "" {
		t.Errorf("unexpected log output: %s", sb.String())
	}
}
