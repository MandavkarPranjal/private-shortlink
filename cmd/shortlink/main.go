package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mandavkarpranjal/private-shortlink/internal/store"
	"github.com/mandavkarpranjal/private-shortlink/internal/web"
)

// version is stamped at link time with
// -ldflags "-X main.version=$(git describe --tags --always --long)" (what
// `mise run build` and `mise run docker:build` do). It is a var, not a const,
// so the linker can replace it. Empty means "not stamped", which is what a
// plain `go build` or `go run` produces; devVersion then names the build and the
// commit is appended from the embedded build info.
var version = ""

// devVersion names a build whose version was not stamped by the linker.
const devVersion = "0.2.0"

type config struct {
	mode     string
	listen   string
	dbPath   string
	hostname string
	authKey  string
	stateDir string
	keyFile  string
	open     bool

	netbirdAPI        string
	netbirdToken      string
	netbirdIface      string
	netbirdAdminGroup string
}

// service is a mode-specific listener plus the identity function it implies.
type service struct {
	ln           net.Listener
	identity     web.IdentityFunc
	closeBackend func() error
	banner       []string
}

func main() {
	cfg, opts, err := parseFlags(os.Args[1:], os.LookupEnv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatalf("%v", err)
	}

	if opts.version {
		fmt.Println("shortlink", versionString())
		return
	}

	logger, err := newLogger(opts.logLevel, opts.logFormat)
	if err != nil {
		fatalf("%v", err)
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		fatalf("%v", err)
	}
}

// fatalf reports a startup failure and exits.
//
// It writes straight to stderr instead of going through slog: once
// slog.SetDefault has installed the process logger, the std logger that
// log.Fatalf uses is redirected to it, so at -log-level warn or error the
// message that explains the exit would itself be filtered out.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shortlink: "+format+"\n", args...)
	os.Exit(1)
}

// options holds the flags that configure the process itself rather than the
// service it starts.
type options struct {
	version   bool
	logLevel  string
	logFormat string
}

// prefixedLogf adapts a printf-style logger from a Tailscale library onto slog.
// Each line becomes one message tagged with the library it came from, truncated
// at the first newline because they are often multi-line. level is Debug for
// the verbose backend chatter (Server.Logf) and Info for the lines meant for
// the operator (Server.UserLogf), such as the first-run login URL — that one has
// to survive the default log level or there is no way to log in.
func prefixedLogf(prefix string, level slog.Level) func(format string, args ...any) {
	return func(format string, args ...any) {
		msg := strings.TrimSpace(fmt.Sprintf(format, args...))
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg != "" {
			slog.Default().Log(context.Background(), level, prefix+": "+msg)
		}
	}
}

// parseFlags parses args (without the program name).
//
// Every flag also reads a SHORTLINK_<FLAG> environment variable, so a systemd
// EnvironmentFile or a .env file can configure the service without rewriting a
// command line: -log-level reads SHORTLINK_LOG_LEVEL, -db reads SHORTLINK_DB.
// Environment values are applied before parsing, so a flag on the command line
// always wins.
func parseFlags(args []string, lookupEnv func(string) (string, bool)) (config, options, error) {
	var cfg config
	var opts options

	fs := newFlagSet(&cfg, &opts)

	badEnv := applyEnv(fs, lookupEnv)
	if err := fs.Parse(args); err != nil {
		return cfg, opts, err
	}
	if fs.NArg() > 0 {
		return cfg, opts, fmt.Errorf("unexpected argument %q (shortlink takes flags only, e.g. -%s %s)",
			fs.Arg(0), fs.Arg(0), fs.Arg(0))
	}
	// An unusable environment value is only fatal if nothing overrode it: a bad
	// SHORTLINK_LOG_FORMAT should not stop a run that passed -log-format json.
	if err := firstEnvError(badEnv, fs); err != nil {
		return cfg, opts, err
	}
	return cfg, opts, nil
}

// newFlagSet declares every flag and binds it to cfg/opts. parseFlags and the
// tests share it, so a new flag cannot drift out of sync with either.
func newFlagSet(cfg *config, opts *options) *flag.FlagSet {
	fs := flag.NewFlagSet("shortlink", flag.ContinueOnError)
	fs.BoolVar(&opts.version, "version", false, "print version and exit")
	fs.StringVar(&cfg.mode, "mode", "tailnet", "service mode: tailnet, tailcat, netbird, or local")
	fs.StringVar(&cfg.listen, "listen", "", "listen address (default \":80\" for tailnet/tailcat/netbird, \":8080\" for local)")
	fs.StringVar(&cfg.dbPath, "db", "shortlink.db", "path to the SQLite database")
	fs.StringVar(&cfg.hostname, "hostname", "go", "tailnet: MagicDNS hostname to advertise")
	fs.StringVar(&cfg.authKey, "ts-authkey", "", "tailnet: node auth key (default $TS_AUTHKEY; unused after first login)")
	fs.StringVar(&cfg.stateDir, "state-dir", "", "tailnet: tsnet node state directory (default under the user config dir)")
	fs.StringVar(&cfg.keyFile, "key-file", "shortlink-tailcat.key", "tailcat: persistent node key file (keeps the tc... address stable)")
	fs.StringVar(&cfg.netbirdAPI, "netbird-api", "https://api.netbird.io", "netbird: management API base URL")
	fs.StringVar(&cfg.netbirdToken, "netbird-token", "", "netbird: management API token (default $NETBIRD_API_TOKEN)")
	fs.StringVar(&cfg.netbirdIface, "netbird-iface", "wt0", "netbird: WireGuard interface to bind")
	fs.StringVar(&cfg.netbirdAdminGroup, "netbird-admin-group", "shortlink-admin", "netbird: peer group granted admin (empty for none)")
	fs.BoolVar(&cfg.open, "open", false, "disable ownership checks; anyone may edit any link")
	fs.StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	fs.StringVar(&opts.logFormat, "log-format", "text", "log format: text or json")
	return fs
}

// envName maps a flag name to its environment variable: -log-level reads
// SHORTLINK_LOG_LEVEL, -netbird-api reads SHORTLINK_NETBIRD_API.
func envName(flagName string) string {
	return "SHORTLINK_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyEnv seeds every flag from its environment variable. It runs before
// Parse, so a flag given on the command line still takes precedence.
//
// A variable that is set but empty counts as a value, because some settings are
// only meaningful when blank — SHORTLINK_NETBIRD_ADMIN_GROUP= is how an operator
// says "no admin group", and skipping empties would silently leave the default
// group in place. A variable that is unset leaves the flag default alone.
//
// Unusable values are returned keyed by flag name rather than raised here, so
// the caller can decide whether the command line already overrode them.
func applyEnv(fs *flag.FlagSet, lookupEnv func(string) (string, bool)) map[string]error {
	bad := make(map[string]error)
	fs.VisitAll(func(f *flag.Flag) {
		val, ok := lookupEnv(envName(f.Name))
		if !ok {
			return
		}
		// f.Value.Set rather than fs.Set: the latter marks the flag as
		// explicitly set, which makes -h print its default twice.
		if err := f.Value.Set(val); err != nil {
			bad[f.Name] = fmt.Errorf("%s=%q is not a valid -%s value: %w", envName(f.Name), val, f.Name, err)
		}
	})
	return bad
}

// firstEnvError returns the first unusable environment value whose flag was not
// also given on the command line. Report order is sorted so the same bad
// environment always produces the same message.
func firstEnvError(bad map[string]error, fs *flag.FlagSet) error {
	if len(bad) == 0 {
		return nil
	}
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var names []string
	for name := range bad {
		if !given[name] {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	msgs := make([]string, 0, len(names))
	for _, name := range names {
		msgs = append(msgs, bad[name].Error())
	}
	return errors.New(strings.Join(msgs, "; "))
}

// versionString is the version to report. A linker-stamped version is
// authoritative and is printed as-is: `git describe --tags --always --long`
// already carries the commit, so appending build info would only duplicate it.
// Without a stamp, name the build and add the VCS revision from the embedded
// build info, so a binary built outside the release process is still traceable
// to a commit.
func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if rev := vcsRevision(bi); rev != "" {
			return devVersion + "+" + rev
		}
	}
	return devVersion
}

func vcsRevision(bi *debug.BuildInfo) string {
	for _, s := range bi.Settings {
		if s.Key != "vcs.revision" {
			continue
		}
		rev := s.Value
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if s.Key == "vcs.revision" {
			for _, other := range bi.Settings {
				if other.Key == "vcs.modified" && other.Value == "true" {
					rev += "-dirty"
				}
			}
		}
		return rev
	}
	return ""
}

// newLogger builds the process logger. -log-format json is for log shippers;
// text is for reading on a terminal.
func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid -log-level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("invalid -log-format %q (want text or json)", format)
	}
}

func run(ctx context.Context, cfg config) error {
	switch cfg.mode {
	case "tailnet", "tailcat", "netbird", "local":
	default:
		return fmt.Errorf("unknown -mode %q (want tailnet, tailcat, netbird, or local)", cfg.mode)
	}

	st, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var svc *service
	switch cfg.mode {
	case "local":
		svc, err = setupLocal(cfg)
	case "tailcat":
		svc, err = setupTailcat(ctx, cfg)
	case "netbird":
		svc, err = setupNetbird(ctx, cfg)
	case "tailnet":
		svc, err = setupTailnet(ctx, cfg)
	}
	if err != nil {
		return err
	}
	if svc.closeBackend != nil {
		defer func() { _ = svc.closeBackend() }()
	}

	logger := slog.Default()
	h, err := web.New(web.Config{
		Store:    st,
		Identity: svc.identity,
		Open:     cfg.open,
		Logger:   logger,
	})
	if err != nil {
		return err
	}
	// A panic in a handler becomes a 500 instead of a dropped connection; the
	// Handler does that itself, so it also gets an access-log line.
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("shortlink starting",
		slog.String("version", versionString()),
		slog.String("mode", cfg.mode),
		slog.String("addr", svc.ln.Addr().String()),
		slog.String("db", cfg.dbPath),
	)
	for _, line := range svc.banner {
		fmt.Fprintln(os.Stdout, line)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(svc.ln) }()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// listenError explains the most common bind failure: ports below 1024 need
// extra privileges. (tsnet and tailcat never hit this — they never bind an
// OS port.)
func listenError(addr string, err error) error {
	if !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	p, convErr := strconv.Atoi(port)
	if convErr != nil || p >= 1024 {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return fmt.Errorf("listen %s: %w (ports below 1024 need root or CAP_NET_BIND_SERVICE; "+
		"try `sudo setcap cap_net_bind_service=+ep shortlink` or -listen %s)",
		addr, err, net.JoinHostPort(host, "8080"))
}

// setupLocal binds a plain TCP listener (development / single-host mode).
func setupLocal(cfg config) (*service, error) {
	addr := cfg.listen
	if addr == "" {
		addr = ":8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, listenError(addr, err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err == nil && (port != "" && port != "0") {
		addr = "127.0.0.1:" + port
	} else {
		addr = ln.Addr().String()
	}
	return &service{
		ln: ln,
		identity: func(*http.Request) web.Identity {
			return web.Identity{ID: "local", IsAdmin: true}
		},
		banner: []string{
			fmt.Sprintf("Open http://%s/ in a browser.", addr),
			"Local mode: identity is \"local\" (admin), no tailnet required.",
		},
	}, nil
}
