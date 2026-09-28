package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"private-shortlink/internal/web"
)

// netbirdRefresh is how often identities are reloaded from the management API.
const netbirdRefresh = 60 * time.Second

// setupNetbird serves over this host's NetBird interface: callers are already
// on your NetBird network, and their identity (user email, admin group) is
// resolved through the NetBird management API.
func setupNetbird(ctx context.Context, cfg config) (*service, error) {
	token := cfg.netbirdToken
	if token == "" {
		token = os.Getenv("NETBIRD_API_TOKEN")
	}
	if token == "" {
		return nil, errors.New("netbird mode needs an API token: set -netbird-token or NETBIRD_API_TOKEN (create one in the NetBird dashboard under Users → Me)")
	}
	baseURL := strings.TrimRight(cfg.netbirdAPI, "/")
	if baseURL == "" {
		baseURL = "https://api.netbird.io"
	}

	addr := cfg.listen
	iface := ""
	if addr == "" {
		ip, err := netbirdIfaceIPv4(cfg.netbirdIface)
		if err != nil {
			return nil, err
		}
		iface = cfg.netbirdIface
		addr = net.JoinHostPort(ip, "80")
	}

	dir := &netbirdDirectory{
		baseURL:    baseURL,
		token:      token,
		adminGroup: cfg.netbirdAdminGroup,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
	// Fail fast when the management API is unreachable: better to refuse to
	// start than to record raw mesh IPs as link owners.
	if err := dir.refresh(ctx); err != nil {
		return nil, fmt.Errorf("netbird: %w", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, listenError(addr, err)
	}

	stop := make(chan struct{})
	go dir.keepFresh(ctx, stop)

	boundHost, port, _ := net.SplitHostPort(ln.Addr().String())
	display := boundHost
	if p, ok := dir.self(boundHost); ok && p.DNSLabel != "" {
		display = p.DNSLabel
	}
	if port != "80" {
		display = net.JoinHostPort(display, port)
	}
	banner := []string{
		fmt.Sprintf("Shortlink is on your NetBird network at http://%s/", display),
		fmt.Sprintf("  Identity: caller's NetBird user email via %s", baseURL),
	}
	if iface != "" {
		banner = append(banner, fmt.Sprintf("  Bound to %s on interface %q (-listen to override)", ln.Addr(), iface))
	}
	if cfg.netbirdAdminGroup != "" {
		banner = append(banner, fmt.Sprintf("  Admins: peers in the %q NetBird group", cfg.netbirdAdminGroup))
	} else {
		banner = append(banner, "  Admins: none (set -netbird-admin-group to grant)")
	}

	return &service{
		ln:           ln,
		identity:     dir.identity,
		closeBackend: func() error { close(stop); return nil },
		banner:       banner,
	}, nil
}

// netbirdGroup is the group reference embedded in a NetBird peer.
type netbirdGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type netbirdPeer struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	IP       string         `json:"ip"`
	IPv6     string         `json:"ipv6"`
	DNSLabel string         `json:"dns_label"`
	UserID   string         `json:"user_id"`
	Groups   []netbirdGroup `json:"groups"`
}

type netbirdUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// netbirdDirectory maps NetBird peer addresses to web identities, refreshed
// from the management API so ownership is recorded as the caller's user email
// rather than a raw mesh IP.
type netbirdDirectory struct {
	baseURL    string
	token      string
	adminGroup string
	client     *http.Client

	mu     sync.RWMutex
	peers  map[string]netbirdPeer // by IPv4 or IPv6 mesh address
	emails map[string]string      // user id → email
}

// refresh reloads the peer and user directories from the management API.
func (d *netbirdDirectory) refresh(ctx context.Context) error {
	var peers []netbirdPeer
	if err := d.getJSON(ctx, "/api/peers", &peers); err != nil {
		return err
	}
	var users []netbirdUser
	if err := d.getJSON(ctx, "/api/users", &users); err != nil {
		return err
	}

	emails := make(map[string]string, len(users))
	for _, u := range users {
		if u.Email != "" {
			emails[u.ID] = u.Email
		}
	}
	byIP := make(map[string]netbirdPeer, len(peers))
	for _, p := range peers {
		if p.IP != "" {
			byIP[p.IP] = p
		}
		if p.IPv6 != "" {
			byIP[p.IPv6] = p
		}
	}

	d.mu.Lock()
	d.peers, d.emails = byIP, emails
	d.mu.Unlock()
	return nil
}

func (d *netbirdDirectory) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", netbirdAuthHeader(d.token))
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("%s: decode: %w", path, err)
	}
	return nil
}

// identity resolves the caller's mesh address to their NetBird user email.
// Callers unknown to the API fall back to their raw address and are never
// admins; it must not fail.
func (d *netbirdDirectory) identity(r *http.Request) web.Identity {
	return d.identityFor(hostOf(r.RemoteAddr))
}

func (d *netbirdDirectory) identityFor(host string) web.Identity {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.peers[host]
	if !ok {
		return web.Identity{ID: host}
	}
	id := d.emails[p.UserID]
	if id == "" {
		id = p.Name
	}
	if id == "" {
		id = host
	}
	return web.Identity{ID: id, IsAdmin: d.isAdmin(p)}
}

// isAdmin reports whether the peer belongs to the configured admin group.
// Group names are matched exactly; an empty adminGroup grants no one.
func (d *netbirdDirectory) isAdmin(p netbirdPeer) bool {
	if d.adminGroup == "" {
		return false
	}
	for _, g := range p.Groups {
		if g.Name == d.adminGroup {
			return true
		}
	}
	return false
}

// self reports the NetBird peer record for a mesh address, if any.
func (d *netbirdDirectory) self(addr string) (netbirdPeer, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.peers[addr]
	return p, ok
}

// keepFresh re-reads the directory until ctx is done or stop is closed.
// Refresh failures keep the previous identities and are only logged.
func (d *netbirdDirectory) keepFresh(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(netbirdRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			if err := d.refresh(ctx); err != nil {
				log.Printf("netbird: identity refresh failed (keeping previous): %v", err)
			}
		}
	}
}

// netbirdAuthHeader authorizes a management API call. Tokens are personal
// access tokens ("Token nbp_..."); a token that already carries a scheme
// (e.g. a Bearer JWT) is passed through unchanged.
func netbirdAuthHeader(token string) string {
	if strings.ContainsAny(token, " \t") {
		return token
	}
	return "Token " + token
}

// netbirdIfaceIPv4 returns the IPv4 address of this host's NetBird WireGuard
// interface (wt0 by default, override with -netbird-iface).
func netbirdIfaceIPv4(name string) (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifs {
		if iface.Name != name {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			return "", fmt.Errorf("netbird interface %q is down", name)
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", fmt.Errorf("netbird interface %q addrs: %w", name, err)
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				return ipnet.IP.To4().String(), nil
			}
		}
		return "", fmt.Errorf("netbird interface %q has no IPv4 address", name)
	}
	return "", fmt.Errorf("netbird interface %q not found (is the NetBird client running? see -netbird-iface)", name)
}

// hostOf strips the port from a RemoteAddr, falling back to the raw value.
func hostOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
