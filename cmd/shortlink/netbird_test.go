package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testPeersJSON = `[
	  {"id":"p1","name":"laptop-01","ip":"100.64.0.5","ipv6":"fd00::5",
	   "dns_label":"laptop-01.netbird.cloud","user_id":"u1",
	   "groups":[{"id":"g1","name":"shortlink-admin"},{"id":"g2","name":"devs"}]},
	  {"id":"p2","name":"ci-runner","ip":"100.64.0.6",
	   "dns_label":"ci-runner.netbird.cloud","user_id":"u2",
	   "groups":[{"id":"g2","name":"devs"}]},
	  {"id":"p3","name":"setup-key-peer","ip":"100.64.0.7",
	   "dns_label":"setup-key-peer.netbird.cloud","user_id":"","groups":[]},
	  {"id":"p4","name":"ghost-peer","ip":"100.64.0.8",
	   "dns_label":"ghost-peer.netbird.cloud","user_id":"u-gone",
	   "groups":[{"id":"g2","name":"devs"}]}
	]`

	testUsersJSON = `[
	  {"id":"u1","email":"alice@example.com"},
	  {"id":"u2","email":"bob@example.com"}
	]`
)

// netbirdAPIServer serves the canned peers/users directory and checks the
// authorization header on every request.
func netbirdAPIServer(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, r *http.Request, body string) {
		t.Helper()
		if got := r.Header.Get("Authorization"); got != "Token nbp_test" {
			t.Errorf("%s Authorization = %q, want %q", r.URL.Path, got, "Token nbp_test")
		}
		if fail != nil && fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		write(w, r, testPeersJSON)
	})
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		write(w, r, testUsersJSON)
	})
	return httptest.NewServer(mux)
}

func newTestDirectory(baseURL, adminGroup string) *netbirdDirectory {
	return &netbirdDirectory{
		baseURL:    baseURL,
		token:      "nbp_test",
		adminGroup: adminGroup,
		client:     http.DefaultClient,
	}
}

func TestNetbirdIdentity(t *testing.T) {
	api := netbirdAPIServer(t, nil)
	defer api.Close()

	dir := newTestDirectory(api.URL, "shortlink-admin")
	if err := dir.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "100.64.0.5:41000" // peer with a user and the admin group
	if got := dir.identity(req); got.ID != "alice@example.com" || !got.IsAdmin {
		t.Errorf("100.64.0.5 identity = %+v, want alice@example.com admin", got)
	}

	cases := []struct {
		host    string
		wantID  string
		wantAdm bool
	}{
		{"100.64.0.6", "bob@example.com", false}, // user, no admin group
		{"100.64.0.7", "setup-key-peer", false},  // no user: peer name
		{"100.64.0.8", "ghost-peer", false},      // user id not in /api/users
		{"fd00::5", "alice@example.com", true},   // IPv6 address index
		{"100.64.0.99", "100.64.0.99", false},    // unknown peer: raw address
		{"not-an-ip", "not-an-ip", false},        // malformed remote addr
	}
	for _, c := range cases {
		got := dir.identityFor(c.host)
		if got.ID != c.wantID || got.IsAdmin != c.wantAdm {
			t.Errorf("identityFor(%q) = %+v, want {ID:%q IsAdmin:%v}", c.host, got, c.wantID, c.wantAdm)
		}
	}

	// Exact group-name matching.
	dirEx := newTestDirectory(api.URL, "Admins")
	if err := dirEx.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := dirEx.identityFor("100.64.0.5"); got.IsAdmin {
		t.Errorf("case-mismatched group granted admin: %+v", got)
	}

	// Empty admin group grants nobody.
	dirNone := newTestDirectory(api.URL, "")
	if err := dirNone.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := dirNone.identityFor("100.64.0.5"); got.IsAdmin {
		t.Errorf("empty admin group granted admin: %+v", got)
	}
}

func TestNetbirdSelf(t *testing.T) {
	api := netbirdAPIServer(t, nil)
	defer api.Close()

	dir := newTestDirectory(api.URL, "shortlink-admin")
	if err := dir.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	p, ok := dir.self("100.64.0.5")
	if !ok || p.DNSLabel != "laptop-01.netbird.cloud" {
		t.Errorf("self(100.64.0.5) = %+v, %v; want dns_label laptop-01.netbird.cloud", p, ok)
	}
	if _, ok := dir.self("100.64.0.99"); ok {
		t.Error("self of unknown address: want miss")
	}
}

func TestNetbirdRefreshFailureKeepsIdentities(t *testing.T) {
	var fail atomic.Bool
	api := netbirdAPIServer(t, &fail)
	defer api.Close()

	dir := newTestDirectory(api.URL, "shortlink-admin")
	if err := dir.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	fail.Store(true)
	if err := dir.refresh(context.Background()); err == nil {
		t.Fatal("refresh against 500: want error")
	}
	if got := dir.identityFor("100.64.0.5"); got.ID != "alice@example.com" || !got.IsAdmin {
		t.Errorf("identity after failed refresh = %+v, want previous alice@example.com admin", got)
	}
}

func TestNetbirdAuthHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nbp_abc123", "Token nbp_abc123"},
		{"Bearer eyJhbGciOi", "Bearer eyJhbGciOi"},
		{"", "Token "},
	}
	for _, c := range cases {
		if got := netbirdAuthHeader(c.in); got != c.want {
			t.Errorf("netbirdAuthHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNetbirdIfaceNotFound(t *testing.T) {
	_, err := netbirdIfaceIPv4("no-such-iface0")
	if err == nil {
		t.Fatal("missing interface: want error")
	}
	if !strings.Contains(err.Error(), "no-such-iface0") {
		t.Errorf("error %q does not name the interface", err)
	}
}

func TestSetupNetbirdRequiresToken(t *testing.T) {
	t.Setenv("NETBIRD_API_TOKEN", "")
	_, err := setupNetbird(context.Background(), config{listen: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("missing token: got %v, want an error naming the token", err)
	}
}

func TestSetupNetbird(t *testing.T) {
	api := netbirdAPIServer(t, nil)
	defer api.Close()
	t.Setenv("NETBIRD_API_TOKEN", "nbp_test")

	svc, err := setupNetbird(context.Background(), config{
		listen:            "127.0.0.1:0",
		netbirdAPI:        api.URL,
		netbirdAdminGroup: "shortlink-admin",
	})
	if err != nil {
		t.Fatalf("setupNetbird: %v", err)
	}
	defer svc.closeBackend()
	if svc.ln == nil || svc.ln.Addr() == nil {
		t.Fatal("no listener")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "100.64.0.5:41000"
	if got := svc.identity(req); got.ID != "alice@example.com" || !got.IsAdmin {
		t.Errorf("identity = %+v, want alice@example.com admin", got)
	}

	banner := strings.Join(svc.banner, "\n")
	for _, want := range []string{"NetBird network", "127.0.0.1", "shortlink-admin", api.URL} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q:\n%s", want, banner)
		}
	}
}
