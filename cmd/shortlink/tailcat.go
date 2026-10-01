package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/mandavkarpranjal/private-shortlink/internal/web"
)

// setupTailcat serves over a tailcat address: a Tailscale data plane without
// a control plane. Clients reach the service with `tailcat forward` or
// `tailcat open` after receiving the printed tc... address out of band.
func setupTailcat(ctx context.Context, cfg config) (*service, error) {
	nodeKey, psk, err := loadOrCreateTailcatKeys(cfg.keyFile)
	if err != nil {
		return nil, err
	}

	addr := cfg.listen
	if addr == "" {
		addr = ":80"
	}
	s := &tailcat.Server{
		Key:          nodeKey,
		PresharedKey: psk,
		Logf:         log.Printf,
	}
	ln, err := s.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tailcat listen: %w", err)
	}

	tailAddr := string(s.TailcatAddr())
	servedPort := servedPort(addr, ln)

	return &service{
		ln: ln,
		identity: func(r *http.Request) web.Identity {
			// The host is the client's synthetic tailcat address, derived
			// from its node key, so it is stable across sessions.
			return web.Identity{ID: hostOf(r.RemoteAddr)}
		},
		closeBackend: func() error { return s.Close() },
		banner: []string{
			"Tailcat address (share this with your clients):",
			"  " + tailAddr,
			fmt.Sprintf("Clients can then run:  tailcat open %s", tailAddr),
			fmt.Sprintf("Or forward a local port:  tailcat forward %s 18080:%s", tailAddr, servedPort),
			fmt.Sprintf("Node key file: %s (keep it to keep the address stable)", cfg.keyFile),
		},
	}, nil
}

// servedPort is the tailcat port clients should forward to.
func servedPort(listenAddr string, ln net.Listener) string {
	if _, p, err := net.SplitHostPort(listenAddr); err == nil && p != "" && p != "0" {
		return p
	}
	if _, p, err := net.SplitHostPort(ln.Addr().String()); err == nil && p != "" {
		return p
	}
	return "80"
}

type tailcatKeyFile struct {
	Key       string `json:"key"`
	Preshared string `json:"preshared"`
}

// loadOrCreateTailcatKeys persists the node private key and preshared key so
// the tc... address survives restarts. Both halves are required: the address
// embeds the preshared key.
func loadOrCreateTailcatKeys(path string) (key.NodePrivate, tailcat.PresharedKey, error) {
	var nk key.NodePrivate
	var psk tailcat.PresharedKey

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		nk = key.NewNode()
		psk = tailcat.NewPresharedKey()
		if err := saveTailcatKeys(path, nk, psk); err != nil {
			return nk, psk, err
		}
		return nk, psk, nil
	}
	if err != nil {
		return nk, psk, fmt.Errorf("read key file: %w", err)
	}

	var kf tailcatKeyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nk, psk, fmt.Errorf("parse key file %s: %w", path, err)
	}
	if err := nk.UnmarshalText([]byte(kf.Key)); err != nil {
		return nk, psk, fmt.Errorf("key file %s: bad node key: %w", path, err)
	}
	if err := psk.UnmarshalText([]byte(kf.Preshared)); err != nil {
		return nk, psk, fmt.Errorf("key file %s: bad preshared key: %w", path, err)
	}
	return nk, psk, nil
}

func saveTailcatKeys(path string, nk key.NodePrivate, psk tailcat.PresharedKey) error {
	keyText, err := nk.MarshalText()
	if err != nil {
		return fmt.Errorf("marshal node key: %w", err)
	}
	pskText, err := psk.MarshalText()
	if err != nil {
		return fmt.Errorf("marshal preshared key: %w", err)
	}
	data, err := json.MarshalIndent(tailcatKeyFile{
		Key:       string(keyText),
		Preshared: string(pskText),
	}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create key dir: %w", err)
		}
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	return nil
}
