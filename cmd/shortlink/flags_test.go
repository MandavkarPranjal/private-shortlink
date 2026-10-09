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

func TestParseFlagsEmptyEnvIgnored(t *testing.T) {
	// An env file with `FOO=` must not blank out a flag default.
	cfg, _, err := parseFlags(nil, envMap(map[string]string{"SHORTLINK_HOSTNAME": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.hostname != "go" {
		t.Errorf("hostname = %q, want the default \"go\"", cfg.hostname)
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
// in deploy/ would document knobs that do not exist.
func TestEveryFlagHasAnEnvName(t *testing.T) {
	var cfg config
	var opts options
	fs := flag.NewFlagSet("shortlink", flag.ContinueOnError)
	fs.BoolVar(&opts.version, "version", false, "")
	fs.StringVar(&cfg.mode, "mode", "", "")
	fs.StringVar(&cfg.listen, "listen", "", "")
	fs.StringVar(&cfg.dbPath, "db", "", "")
	fs.StringVar(&cfg.hostname, "hostname", "", "")
	fs.StringVar(&cfg.authKey, "ts-authkey", "", "")
	fs.StringVar(&cfg.stateDir, "state-dir", "", "")
	fs.StringVar(&cfg.keyFile, "key-file", "", "")
	fs.StringVar(&cfg.netbirdAPI, "netbird-api", "", "")
	fs.StringVar(&cfg.netbirdToken, "netbird-token", "", "")
	fs.StringVar(&cfg.netbirdIface, "netbird-iface", "", "")
	fs.StringVar(&cfg.netbirdAdminGroup, "netbird-admin-group", "", "")
	fs.BoolVar(&cfg.open, "open", false, "")
	fs.StringVar(&opts.logLevel, "log-level", "", "")
	fs.StringVar(&opts.logFormat, "log-format", "", "")

	seen := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		name := envName(f.Name)
		if !strings.HasPrefix(name, "SHORTLINK_") || name != strings.ToUpper(name) {
			t.Errorf("flag -%s maps to unusable env name %q", f.Name, name)
		}
		if seen[name] {
			t.Errorf("env name %q is claimed by two flags", name)
		}
		seen[name] = true
	})
	if len(seen) != 15 {
		t.Errorf("covered %d flags, want 15", len(seen))
	}
}
