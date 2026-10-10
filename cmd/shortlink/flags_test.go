package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

// envMap turns a map into a lookup function for parseFlags.
func envMap(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	cfg, opts, err := parseFlags(nil, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "tailnet" {
		t.Errorf("mode = %q, want tailnet", cfg.mode)
	}
	if cfg.hostname != "go" || cfg.dbPath != "shortlink.db" {
		t.Errorf("defaults = %+v", cfg)
	}
	if opts.logLevel != "info" || opts.logFormat != "text" || opts.version {
		t.Errorf("options = %+v", opts)
	}
}

func TestParseFlagsFromArgs(t *testing.T) {
	cfg, opts, err := parseFlags([]string{
		"-mode", "local",
		"-listen", "127.0.0.1:9999",
		"-db", "/tmp/x.db",
		"-open",
		"-log-level", "debug",
		"-log-format", "json",
	}, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "local" || cfg.listen != "127.0.0.1:9999" || cfg.dbPath != "/tmp/x.db" || !cfg.open {
		t.Errorf("config = %+v", cfg)
	}
	if opts.logLevel != "debug" || opts.logFormat != "json" {
		t.Errorf("options = %+v", opts)
	}
}

func TestParseFlagsFromEnv(t *testing.T) {
	cfg, opts, err := parseFlags(nil, envMap(map[string]string{
		"SHORTLINK_MODE":                 "netbird",
		"SHORTLINK_NETBIRD_ADMIN_GROUP":  "admins",
		"SHORTLINK_LOG_LEVEL":            "warn",
		"SHORTLINK_LOG_FORMAT":           "json",
		"SHORTLINK_OPEN":                 "true",
		"SHORTLINK_KEY_FILE":             "/var/lib/shortlink/key",
		"SHORTLINK_NETBIRD_ADMIN_GROUPS": "ignored", // not a flag
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "netbird" || cfg.netbirdAdminGroup != "admins" || cfg.keyFile != "/var/lib/shortlink/key" {
		t.Errorf("config = %+v", cfg)
	}
	if !cfg.open {
		t.Error("SHORTLINK_OPEN did not set -open")
	}
	if opts.logLevel != "warn" || opts.logFormat != "json" {
		t.Errorf("options = %+v", opts)
	}
}

// A flag on the command line must beat the environment: that is the whole
// point of applying the environment before parsing.
func TestParseFlagsArgBeatsEnv(t *testing.T) {
	cfg, opts, err := parseFlags([]string{"-mode", "tailcat", "-log-level", "error"},
		envMap(map[string]string{
			"SHORTLINK_MODE":      "local",
			"SHORTLINK_LOG_LEVEL": "debug",
		}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != "tailcat" {
		t.Errorf("mode = %q, want tailcat (the flag must win)", cfg.mode)
	}
	if opts.logLevel != "error" {
		t.Errorf("log level = %q, want error", opts.logLevel)
	}
}

// An unset variable leaves the default alone; a variable that is set but empty
// is a value, because that is how an operator disables a setting that defaults
// to something.
func TestParseFlagsEmptyEnvApplied(t *testing.T) {
	cfg, _, err := parseFlags(nil, envMap(map[string]string{
		"SHORTLINK_HOSTNAME":            "",
		"SHORTLINK_NETBIRD_ADMIN_GROUP": "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.hostname != "" {
		t.Errorf("hostname = %q, want the empty env value", cfg.hostname)
	}
	if cfg.netbirdAdminGroup != "" {
		t.Errorf("admin group = %q, want empty: an empty variable is how you turn admins off", cfg.netbirdAdminGroup)
	}
}

func TestParseFlagsUnsetEnvKeepsDefault(t *testing.T) {
	cfg, _, err := parseFlags(nil, envMap(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.hostname != "go" {
		t.Errorf("hostname = %q, want the default \"go\"", cfg.hostname)
	}
	if cfg.netbirdAdminGroup != "shortlink-admin" {
		t.Errorf("admin group = %q, want the default", cfg.netbirdAdminGroup)
	}
}

func TestParseFlagsBadEnvValue(t *testing.T) {
	_, _, err := parseFlags(nil, envMap(map[string]string{"SHORTLINK_OPEN": "yes-please"}))
	if err == nil {
		t.Fatal("invalid SHORTLINK_OPEN: want error")
	}
	if !strings.Contains(err.Error(), "SHORTLINK_OPEN") {
		t.Errorf("error should name the variable: %v", err)
	}
}

// A command-line flag overrides the environment, so a bad value in the
// environment must not stop a run that supplied a good one.
func TestParseFlagsBadEnvValueOverriddenByArg(t *testing.T) {
	cfg, opts, err := parseFlags(
		[]string{"-log-format", "json"},
		envMap(map[string]string{"SHORTLINK_LOG_FORMAT": "xml"}),
	)
	if err != nil {
		t.Fatalf("a flag given on the command line should win over a bad env value: %v", err)
	}
	if opts.logFormat != "json" {
		t.Errorf("log format = %q, want json", opts.logFormat)
	}
	if cfg.mode != "tailnet" {
		t.Errorf("mode = %q, want the default", cfg.mode)
	}
}

func TestParseFlagsBadEnvValueStillFatalForOtherFlags(t *testing.T) {
	_, _, err := parseFlags(
		[]string{"-log-format", "json"},
		envMap(map[string]string{
			"SHORTLINK_LOG_FORMAT": "xml",
			"SHORTLINK_OPEN":       "yes-please",
		}),
	)
	if err == nil {
		t.Fatal("an unrelated bad env value must still fail the run")
	}
	if !strings.Contains(err.Error(), "SHORTLINK_OPEN") {
		t.Errorf("error should name the variable that was not overridden: %v", err)
	}
	if strings.Contains(err.Error(), "SHORTLINK_LOG_FORMAT") {
		t.Errorf("error should not mention the overridden flag: %v", err)
	}
}

func TestParseFlagsUnknownFlag(t *testing.T) {
	// parseFlags prints usage to stderr on a bad flag; keep the test output
	// clean.
	var err error
	out := captureStderr(t, func() {
		_, _, err = parseFlags([]string{"-nope"}, envMap(nil))
	})
	if err == nil {
		t.Fatal("unknown flag: want an error")
	}
	if !strings.Contains(out, "Usage of shortlink") {
		t.Errorf("usage was not printed:\n%s", out)
	}
	if !strings.Contains(err.Error(), "-nope") {
		t.Errorf("err = %v, want it to name the unknown flag", err)
	}
}

// -h must reach main as flag.ErrHelp so the process exits 0 after printing
// usage, instead of logging "shortlink: flag: help requested".
func TestParseFlagsHelpExitsQuietly(t *testing.T) {
	var err error
	captureStderr(t, func() {
		_, _, err = parseFlags([]string{"-h"}, envMap(nil))
	})
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("err = %v, want flag.ErrHelp", err)
	}
}

func TestParseFlagsRejectsPositionalArgs(t *testing.T) {
	// `shortlink -mode local :8080` is a common typo; catch it instead of
	// silently ignoring the address.
	_, _, err := parseFlags([]string{":8080"}, envMap(nil))
	if err == nil {
		t.Fatal("positional argument: want error")
	}
	if !strings.Contains(err.Error(), "-:8080") {
		t.Errorf("error should suggest the flag form: %v", err)
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"log-level":           "SHORTLINK_LOG_LEVEL",
		"log-format":          "SHORTLINK_LOG_FORMAT",
		"db":                  "SHORTLINK_DB",
		"netbird-admin-group": "SHORTLINK_NETBIRD_ADMIN_GROUP",
		"version":             "SHORTLINK_VERSION",
	} {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every flag should be reachable from the environment; otherwise the env file
// in deploy/ would document knobs that do not exist. This walks the real flag
// set, so a new flag is covered the moment it is declared — no second copy of
// the list here to forget.
func TestEveryFlagHasAnEnvName(t *testing.T) {
	var cfg config
	var opts options
	fs := newFlagSet(&cfg, &opts)

	// boolFlag is how the flag package marks a value that only accepts "true"
	// and "false". Testing for it keeps this test working when a boolean flag is
	// added: anything else gets a string.
	boolFlag := func(f *flag.Flag) bool {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		return ok && b.IsBoolFlag()
	}

	env := map[string]string{}
	fs.VisitAll(func(f *flag.Flag) {
		name := envName(f.Name)
		if !strings.HasPrefix(name, "SHORTLINK_") || name != strings.ToUpper(name) {
			t.Errorf("flag -%s maps to unusable env name %q", f.Name, name)
		}
		if _, dup := env[name]; dup {
			t.Errorf("env name %q is claimed by two flags", name)
		}
		env[name] = "sentinel"
		if boolFlag(f) {
			env[name] = "true"
		}
	})
	if len(env) == 0 {
		t.Fatal("no flags declared")
	}
	// Every env name must round-trip through parseFlags. The lookup is scoped to
	// env so an unrelated SHORTLINK_* in the test process cannot fail the run.
	if _, _, err := parseFlags(nil, envMap(env)); err != nil {
		t.Fatalf("parseFlags with every variable set: %v", err)
	}
}

// A flag and its environment variable must agree on the value: if envName were
// changed, the environment would silently stop configuring the flag.
func TestEnvVarReachesItsFlag(t *testing.T) {
	cfg, _, err := parseFlags(nil, envMap(map[string]string{
		"SHORTLINK_DB":            "/from/env.db",
		"SHORTLINK_NETBIRD_IFACE": "wt9",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dbPath != "/from/env.db" {
		t.Errorf("db path = %q, want /from/env.db", cfg.dbPath)
	}
	if cfg.netbirdIface != "wt9" {
		t.Errorf("netbird iface = %q, want wt9", cfg.netbirdIface)
	}
}
