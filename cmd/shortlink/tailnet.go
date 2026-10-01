package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"
	"tailscale.com/tsnet"

	"github.com/mandavkarpranjal/private-shortlink/internal/web"
)

// adminCap is the tailcfg capability that grants admin rights on this app.
// Grant it in your tailnet ACLs:
//
//	"grants": [
//	  {"src": ["group:ops"], "dst": ["tag:shortlink"], "ip": ["80"],
//	   "app": {"tailscale.com/cap/shortlink": [{"admin": true}]}},
//	]
const adminCap = peercap.Cap("tailscale.com/cap/shortlink")

// setupTailnet embeds a Tailscale node (tsnet) and serves on the tailnet only.
func setupTailnet(ctx context.Context, cfg config) (*service, error) {
	dir := cfg.stateDir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("user config dir: %w", err)
		}
		dir = filepath.Join(base, "shortlink-tsnet")
	}

	authKey := cfg.authKey
	if authKey == "" {
		authKey = os.Getenv("TS_AUTHKEY")
	}

	s := &tsnet.Server{
		Dir:      dir,
		Hostname: cfg.hostname,
		AuthKey:  authKey,
		UserLogf: log.Printf,
	}

	// Up logs in (or prints an auth URL) and waits until the node is running.
	st, err := s.Up(ctx)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale login: %w", err)
	}

	addr := cfg.listen
	if addr == "" {
		addr = ":80"
	}
	ln, err := s.Listen("tcp", addr)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale listen: %w", err)
	}
	lc, err := s.LocalClient()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("tailscale local client: %w", err)
	}

	banner := []string{fmt.Sprintf(
		"Shortlink is on your tailnet at http://%s/ (state in %s)", cfg.hostname, dir)}
	if st != nil {
		if st.Self != nil && st.Self.DNSName != "" {
			banner = append(banner, "  Also reachable as http://"+strings.TrimSuffix(st.Self.DNSName, ".")+"/")
		}
		if len(st.TailscaleIPs) > 0 {
			banner = append(banner, "  Tailnet IPs: "+fmt.Sprint(st.TailscaleIPs))
		}
	}

	return &service{
		ln:           ln,
		identity:     whoIsIdentity(lc),
		closeBackend: func() error { return s.Close() },
		banner:       banner,
	}, nil
}

// whoIsIdentity resolves the caller via tailscaled's WhoIs API, using the
// user's login name as the owner identity and the app admin capability to
// decide admin rights.
func whoIsIdentity(lc *local.Client) web.IdentityFunc {
	return func(r *http.Request) web.Identity {
		resp, err := lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || resp == nil || resp.UserProfile == nil {
			return web.Identity{ID: hostOf(r.RemoteAddr)}
		}
		id := resp.UserProfile.LoginName
		if id == "" {
			id = resp.UserProfile.DisplayName
		}
		return web.Identity{ID: id, IsAdmin: isAdminCap(resp.CapMap)}
	}
}

// isAdminCap reports whether the peer was granted admin:true on adminCap.
func isAdminCap(cm tailcfg.PeerCapMap) bool {
	vals, ok := cm[adminCap]
	if !ok {
		return false
	}
	for _, raw := range vals {
		var v struct {
			Admin bool `json:"admin"`
		}
		if json.Unmarshal([]byte(raw), &v) == nil && v.Admin {
			return true
		}
	}
	return false
}
