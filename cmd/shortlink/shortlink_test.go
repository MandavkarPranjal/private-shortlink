package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"

	"private-shortlink/internal/store"
	"private-shortlink/internal/web"
)

func TestLoadOrCreateTailcatKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "shortlink-tailcat.key")

	k1, p1, err := loadOrCreateTailcatKeys(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k1.IsZero() || p1.IsZero() {
		t.Fatal("generated zero keys")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %o, want 600", fi.Mode().Perm())
	}

	k2, p2, err := loadOrCreateTailcatKeys(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !k1.Equal(k2) || !p1.Equal(p2) {
		t.Fatal("keys not stable across reload")
	}

	// Corrupt file fails loudly instead of silently regenerating.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateTailcatKeys(path); err == nil {
		t.Error("corrupt key file: want error")
	}
}

func TestServedPort(t *testing.T) {
	if got := servedPort(":80", nil); got != "80" {
		t.Errorf(`servedPort(":80") = %q, want "80"`, got)
	}
	if got := servedPort("127.0.0.1:8080", nil); got != "8080" {
		t.Errorf(`servedPort("127.0.0.1:8080") = %q, want "8080"`, got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	want, err := strconv.Atoi(portOf(ln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := servedPort(":0", ln); got != strconv.Itoa(want) {
		t.Errorf("servedPort(:0, ln) = %q, want %d", got, want)
	}
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return p
}

func TestListenError(t *testing.T) {
	perm := fmt.Errorf("bind: %w", os.ErrPermission)

	err := listenError("100.64.0.1:80", perm)
	for _, want := range []string{
		"100.64.0.1:80", "permission denied", "CAP_NET_BIND_SERVICE", "-listen 100.64.0.1:8080",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("listenError = %q, missing %q", err, want)
		}
	}

	if err := listenError(":80", perm); !strings.Contains(err.Error(), "-listen :8080") {
		t.Errorf("host-less addr hint: %v", err)
	}
	if err := listenError("100.64.0.1:8080", perm); strings.Contains(err.Error(), "CAP_NET_BIND_SERVICE") {
		t.Errorf("high port should not get the hint: %v", err)
	}
	other := errors.New("address already in use")
	err = listenError("100.64.0.1:80", other)
	if !strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "CAP_NET_BIND_SERVICE") {
		t.Errorf("non-permission error: %v", err)
	}
}

func TestIsAdminCap(t *testing.T) {
	cases := []struct {
		name string
		cm   tailcfg.PeerCapMap
		want bool
	}{
		{"nil", nil, false},
		{"no cap", tailcfg.PeerCapMap{"other": nil}, false},
		{"cap no value", tailcfg.PeerCapMap{adminCap: nil}, false},
		{"admin true", tailcfg.PeerCapMap{
			adminCap: {tailcfg.RawMessage(`{"admin": true}`)},
		}, true},
		{"admin false", tailcfg.PeerCapMap{
			adminCap: {tailcfg.RawMessage(`{"admin": false}`)},
		}, false},
		{"admin in second value", tailcfg.PeerCapMap{
			adminCap: {tailcfg.RawMessage(`{}`), tailcfg.RawMessage(`{"admin":true}`)},
		}, true},
		{"garbage value", tailcfg.PeerCapMap{
			adminCap: {tailcfg.RawMessage(`not json`)},
		}, false},
	}
	for _, c := range cases {
		if got := isAdminCap(c.cm); got != c.want {
			t.Errorf("%s: isAdminCap = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTailcatEndToEnd runs a real tailcat server and client in-process,
// relaying through DERP. It needs network access to tailcat.dev.
func TestTailcatEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tailcat e2e in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dir := t.TempDir()
	nk, psk, err := loadOrCreateTailcatKeys(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err := web.New(web.Config{
		Store: st,
		Identity: func(r *http.Request) web.Identity {
			return web.Identity{ID: r.RemoteAddr}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := &tailcat.Server{Key: nk, PresharedKey: psk, Logf: t.Logf}
	ln, err := srv.Listen(ctx, "tcp", ":0")
	if err != nil {
		t.Skipf("tailcat listen failed (no network?): %v", err)
	}
	defer srv.Close()
	go http.Serve(ln, h)

	port := servedPort(":0", ln)
	t.Logf("tailcat addr %s, served port %s", srv.TailcatAddr(), port)
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatalf("bad served port %q: %v", port, err)
	}

	client := tailcat.NewClient(srv.TailcatAddr())
	defer client.Close()

	doer := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return client.DialTCPPort(ctx, uint16(portNum))
			},
		},
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Home page over the tunnel.
	resp, err := doer.Get("http://shortlink.test/")
	if err != nil {
		t.Fatalf("GET / over tailcat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("shortlink")) {
		t.Fatalf("GET /: status %d body %.200q", resp.StatusCode, body)
	}

	// Create a link as the remote (synthetic) identity.
	reqBody, _ := json.Marshal(map[string]string{"name": "e2e", "url": "https://example.com/e2e"})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://shortlink.test/-/save", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = doer.Do(req)
	if err != nil {
		t.Fatalf("POST /-/save over tailcat: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save: status %d body %s", resp.StatusCode, respBody)
	}

	// Identity came from the client's tailcat address, not "local".
	l, err := st.Get(context.Background(), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if l.Owner == "" || l.Owner == "local" {
		t.Errorf("owner = %q, want tailcat peer address", l.Owner)
	}
	t.Logf("owner recorded as %q", l.Owner)

	// Redirect works and counts a click.
	resp, err = doer.Get("http://shortlink.test/e2e")
	if err != nil {
		t.Fatalf("GET /e2e over tailcat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("redirect status = %d, want 302", resp.StatusCode)
	}
	l, _ = st.Get(context.Background(), "e2e")
	if l.Clicks != 1 {
		t.Errorf("clicks = %d, want 1", l.Clicks)
	}
}
