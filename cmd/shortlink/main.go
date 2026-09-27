package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"private-shortlink/internal/store"
	"private-shortlink/internal/web"
)

const version = "0.1.0"

type config struct {
	mode     string
	listen   string
	dbPath   string
	hostname string
	authKey  string
	stateDir string
	keyFile  string
	open     bool
}

// service is a mode-specific listener plus the identity function it implies.
type service struct {
	ln           net.Listener
	identity     web.IdentityFunc
	closeBackend func() error
	banner       []string
}

func main() {
	cfg := config{}
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.StringVar(&cfg.mode, "mode", "tailnet", "service mode: tailnet, tailcat, or local")
	flag.StringVar(&cfg.listen, "listen", "", "listen address (default \":80\" for tailnet/tailcat, \":8080\" for local)")
	flag.StringVar(&cfg.dbPath, "db", "shortlink.db", "path to the SQLite database")
	flag.StringVar(&cfg.hostname, "hostname", "go", "tailnet: MagicDNS hostname to advertise (default \"go\")")
	flag.StringVar(&cfg.authKey, "ts-authkey", "", "tailnet: node auth key (default $TS_AUTHKEY; unused after first login)")
	flag.StringVar(&cfg.stateDir, "state-dir", "", "tailnet: tsnet state directory (default under the user config dir)")
	flag.StringVar(&cfg.keyFile, "key-file", "shortlink-tailcat.key", "tailcat: persistent node key file (keeps the tc... address stable)")
	flag.BoolVar(&cfg.open, "open", false, "disable ownership checks; anyone may edit any link")
	flag.Parse()

	if *showVersion {
		fmt.Println("shortlink", version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatalf("shortlink: %v", err)
	}
}

func run(ctx context.Context, cfg config) error {
	switch cfg.mode {
	case "tailnet", "tailcat", "local":
	default:
		return fmt.Errorf("unknown -mode %q (want tailnet, tailcat, or local)", cfg.mode)
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
	case "tailnet":
		svc, err = setupTailnet(ctx, cfg)
	}
	if err != nil {
		return err
	}
	if svc.closeBackend != nil {
		defer svc.closeBackend()
	}

	h, err := web.New(web.Config{Store: st, Identity: svc.identity, Open: cfg.open})
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

	log.Printf("shortlink %s serving in %s mode on %s (db %s)", version, cfg.mode, svc.ln.Addr(), cfg.dbPath)
	for _, line := range svc.banner {
		fmt.Fprintln(os.Stdout, line)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(svc.ln) }()

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
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

// setupLocal binds a plain TCP listener (development / single-host mode).
func setupLocal(cfg config) (*service, error) {
	addr := cfg.listen
	if addr == "" {
		addr = ":8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
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
