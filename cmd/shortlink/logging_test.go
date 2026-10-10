package main

import (
	"bytes"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

func TestNewLoggerRejectsBadInput(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "INFO", "WARN"} {
		if _, err := newLogger(level, "text"); err != nil {
			t.Errorf("newLogger(%q, text): %v", level, err)
		}
	}
	if _, err := newLogger("verbose", "text"); err == nil {
		t.Error("newLogger with an unknown level: want error")
	}
	if _, err := newLogger("info", "logfmt"); err == nil {
		t.Error("newLogger with an unknown format: want error")
	}
}

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns what was
// written. newLogger resolves os.Stderr at construction time, so the swap must
// happen before the call under test.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestNewLoggerWritesToStderr(t *testing.T) {
	// Anything the operator captures with journald must include log output.
	out := captureStderr(t, func() {
		l, err := newLogger("info", "json")
		if err != nil {
			t.Fatal(err)
		}
		l.Info("hello", slog.String("k", "v"))
	})
	if !strings.Contains(out, `"k":"v"`) {
		t.Errorf("json log line = %q", out)
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	out := captureStderr(t, func() {
		warn, err := newLogger("warn", "text")
		if err != nil {
			t.Fatal(err)
		}
		warn.Info("filtered")
		warn.Warn("kept")
		warn.Error("kept too")
	})
	if strings.Contains(out, "filtered") {
		t.Errorf("Info line survived a Warn threshold: %q", out)
	}
	for _, want := range []string{"kept", "kept too"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output: %q", want, out)
		}
	}
}

// A fatal message is the last thing the process says before exiting. It has to
// land in the configured format, or a -log-format json stream carries one
// unparseable line at exactly the moment someone is debugging a startup
// failure.
func TestFatalLogfUsesTheConfiguredFormat(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			// newLogger binds os.Stderr, so it must run inside captureStderr.
			out := captureStderr(t, func() {
				l, err := newLogger("info", format)
				if err != nil {
					t.Fatal(err)
				}
				slog.SetDefault(l)
				fatalLogf("cannot open %s", "/var/lib/shortlink/shortlink.db")
			})
			if !strings.Contains(out, "/var/lib/shortlink/shortlink.db") {
				t.Errorf("fatal message lost: %q", out)
			}
			if format == "json" && !strings.HasPrefix(strings.TrimSpace(out), "{") {
				t.Errorf("fatal message is not a JSON record: %q", out)
			}
		})
	}
}

// An error record survives every level a caller can configure, including the
// one that drops everything but errors. Losing the fatal message to its own
// log threshold would exit with no explanation at all.
func TestFatalLogfSurvivesEveryLevel(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			out := captureStderr(t, func() {
				l, err := newLogger(level, "text")
				if err != nil {
					t.Fatal(err)
				}
				slog.SetDefault(l)
				fatalLogf("boom")
			})
			if !strings.Contains(out, "boom") {
				t.Errorf("fatal message filtered at -log-level %s: %q", level, out)
			}
		})
	}
}

func TestNewLoggerTextFormat(t *testing.T) {
	out := captureStderr(t, func() {
		l, err := newLogger("debug", "text")
		if err != nil {
			t.Fatal(err)
		}
		l.Debug("plain")
	})
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "msg=plain") {
		t.Errorf("text log line = %q", out)
	}
}

func TestVersionString(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "1.2.3"
	got := versionString()
	if !strings.HasPrefix(got, "1.2.3") {
		t.Errorf("versionString() = %q, want it to start with 1.2.3", got)
	}
	// A test binary carries no VCS settings, so no stray "+" suffix.
	if suffix := strings.TrimPrefix(got, "1.2.3"); suffix != "" && !strings.HasPrefix(suffix, "+") {
		t.Errorf("unexpected version suffix %q", suffix)
	}
}

func TestVCSRevision(t *testing.T) {
	tests := []struct {
		name string
		in   *debug.BuildInfo
		want string
	}{
		{"none", &debug.BuildInfo{}, ""},
		{
			"long commit",
			&debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef"},
			}},
			"0123456",
		},
		{
			"dirty tree",
			&debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef"},
				{Key: "vcs.modified", Value: "true"},
			}},
			"0123456-dirty",
		},
		{
			"clean tree",
			&debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "0123456789abcdef"},
				{Key: "vcs.modified", Value: "false"},
			}},
			"0123456",
		},
		{
			"empty revision",
			&debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: ""},
			}},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vcsRevision(tc.in); got != tc.want {
				t.Errorf("vcsRevision = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrefixedLogf covers the printf-style adapter the tsnet and tailcat
// loggers are wired to: one trimmed Debug line, empty messages dropped.
func TestPrefixedLogf(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var sb bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&sb, &slog.HandlerOptions{Level: slog.LevelDebug})))

	logf := prefixedLogf("tsnet", tsnetUserLogLevel)
	logf("hello %s\nsecond line", "world")
	logf("   \n  ")
	prefixedLogf("tailcat", slog.LevelDebug)("up")

	out := sb.String()
	if !strings.Contains(out, "tsnet: hello world") {
		t.Errorf("missing trimmed message: %q", out)
	}
	if !strings.Contains(out, "tailcat: up") {
		t.Errorf("tailcat prefix missing: %q", out)
	}
	if strings.Contains(out, "second line") {
		t.Errorf("multi-line tail not trimmed: %q", out)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 2 {
		t.Errorf("got %d log lines, want 2:\n%s", n, out)
	}
}

// TestPrefixedLogfRespectsLevel documents that the verbose backend chatter is
// hidden at the default level while the operator-facing lines (the tsnet login
// URL) still come through.
func TestPrefixedLogfRespectsLevel(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var sb bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&sb, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// These use the levels the servers are actually constructed with, so
	// rewiring setupTailnet or setupTailcat to debug fails here rather than
	// leaving a test that passes while the login URL is invisible.
	prefixedLogf("tailcat", tailcatLogLevel)("chatter")
	if sb.String() != "" {
		t.Errorf("backend chatter visible at info level: %q", sb.String())
	}

	// tsnet's printAuthURLLoop emits exactly this line, once per second, while
	// the node is unauthenticated.
	sb.Reset()
	prefixedLogf("tsnet", tsnetUserLogLevel)("To start this tsnet server, restart with TS_AUTHKEY set, or go to: https://login.tailscale.com/a/abc123")
	out := sb.String()
	if !strings.Contains(out, "https://login.tailscale.com/a/abc123") {
		t.Errorf("auth URL must survive the default level: %q", out)
	}
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("auth URL should log at info: %q", out)
	}
}

// The level constants only matter if they are the ones the servers use. Assert
// the wiring directly, so a future edit to setupTailnet cannot quietly drop the
// login URL below the default level.
func TestTsnetUserLogLevelIsInfoOrHigher(t *testing.T) {
	if tsnetUserLogLevel > slog.LevelInfo {
		t.Errorf("tsnetUserLogLevel = %v; the login URL is logged through UserLogf, so it must be visible at the default level", tsnetUserLogLevel)
	}
	if tailcatLogLevel != slog.LevelDebug {
		t.Errorf("tailcatLogLevel = %v, want debug for the verbose backend sink", tailcatLogLevel)
	}
}
