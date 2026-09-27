package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"private-shortlink/internal/store"
)

func newTestHandler(t *testing.T, identity IdentityFunc, open bool) (*Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if identity == nil {
		identity = func(*http.Request) Identity { return Identity{ID: "alice"} }
	}
	h, err := New(Config{Store: st, Identity: identity, Open: open})
	if err != nil {
		t.Fatal(err)
	}
	return h, st
}

func do(t *testing.T, h http.Handler, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRedirectAndClickCount(t *testing.T) {
	h, st := newTestHandler(t, nil, false)
	ctx := context.Background()
	if err := st.Save(ctx, &store.Link{Name: "blog", URL: "https://example.com/blog", Owner: "alice"}, false); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		rec := do(t, h, "GET", "/blog", nil, nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "https://example.com/blog" {
			t.Fatalf("Location = %q", loc)
		}
	}
	l, err := st.Get(ctx, "blog")
	if err != nil || l.Clicks != 2 {
		t.Fatalf("clicks = %d (err %v), want 2", l.Clicks, err)
	}

	// Multi-segment name.
	if err := st.Save(ctx, &store.Link{Name: "a/docs", URL: "https://docs.example.com", Owner: "alice"}, false); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "GET", "/a/docs", nil, nil); rec.Code != http.StatusFound {
		t.Errorf("multi-segment status = %d", rec.Code)
	}

	// Unknown and reserved names are 404.
	for _, p := range []string{"/nope", "/-/nonexistent", "/-/save"} {
		if rec := do(t, h, "GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", p, rec.Code)
		}
	}
}

func TestSaveFormOwnership(t *testing.T) {
	h, st := newTestHandler(t, nil, false)

	// Create as alice.
	form := url.Values{"name": {"blog"}, "url": {"https://example.com"}}
	rec := do(t, h, "POST", "/-/save", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	l, err := st.Get(context.Background(), "blog")
	if err != nil || l.Owner != "alice" {
		t.Fatalf("owner = %+v (err %v)", l, err)
	}

	// Bob cannot update (second handler sharing the same store).
	bob := func(*http.Request) Identity { return Identity{ID: "bob"} }
	hb, err := New(Config{Store: st, Identity: bob})
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"name": {"blog"}, "url": {"https://evil.example.com"}}
	rec = do(t, hb, "POST", "/-/save", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob update status = %d, want 403", rec.Code)
	}
	l, _ = st.Get(context.Background(), "blog")
	if l.URL != "https://example.com" {
		t.Errorf("URL changed by non-owner: %s", l.URL)
	}

	// Bob cannot delete.
	form = url.Values{"name": {"blog"}}
	rec = do(t, hb, "POST", "/-/delete", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob delete status = %d, want 403", rec.Code)
	}

	// Admin can update anyone's link.
	admin, err := New(Config{Store: st, Identity: func(*http.Request) Identity {
		return Identity{ID: "root", IsAdmin: true}
	}})
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"name": {"blog"}, "url": {"https://example.org"}}
	rec = do(t, admin, "POST", "/-/save", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin update status = %d (%s)", rec.Code, rec.Body.String())
	}
	l, _ = st.Get(context.Background(), "blog")
	if l.URL != "https://example.org" || l.Owner != "alice" {
		t.Errorf("after admin update: %+v", l)
	}

	// Owner can delete.
	rec = do(t, h, "POST", "/-/delete", strings.NewReader(url.Values{"name": {"blog"}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("owner delete status = %d", rec.Code)
	}
	if _, err := st.Get(context.Background(), "blog"); err == nil {
		t.Error("link still exists after owner delete")
	}
}

func TestSaveJSONAPI(t *testing.T) {
	h, st := newTestHandler(t, nil, false)

	rec := do(t, h, "POST", "/-/save", bytes.NewBufferString(`{"name":"go","url":"https://go.dev"}`),
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool        `json:"ok"`
		Link *store.Link `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Link.Owner != "alice" {
		t.Fatalf("save response: %s (err %v)", rec.Body.String(), err)
	}

	// List.
	rec = do(t, h, "GET", "/-/api", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list struct {
		Links []*store.Link `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Links) != 1 {
		t.Fatalf("list: %s (err %v)", rec.Body.String(), err)
	}

	// Single.
	rec = do(t, h, "GET", "/-/api/go", nil, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"https://go.dev"`)) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}

	// 404.
	if rec := do(t, h, "GET", "/-/api/missing", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing api status = %d", rec.Code)
	}

	// Delete via JSON.
	rec = do(t, h, "POST", "/-/delete", bytes.NewBufferString(`{"name":"go"}`),
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.Get(context.Background(), "go"); err == nil {
		t.Error("link still exists")
	}
}

func TestSaveValidation(t *testing.T) {
	h, _ := newTestHandler(t, nil, false)

	bad := []struct {
		name, url string
	}{
		{"UPPER", "https://example.com"},
		{"-reserved", "https://example.com"},
		{"has space", "https://example.com"},
		{"a//b", "https://example.com"},
		{"a/../../etc", "https://example.com"},
		{"", "https://example.com"},
		{"ok", "ftp://example.com"},
		{"ok", "notaurl"},
		{"ok", ""},
	}
	for _, c := range bad {
		form := url.Values{"name": {c.name}, "url": {c.url}}
		rec := do(t, h, "POST", "/-/save", strings.NewReader(form.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("save (%q, %q): status = %d, want 400", c.name, c.url, rec.Code)
		}
		// JSON path rejects with 400 too.
		body, _ := json.Marshal(map[string]string{"name": c.name, "url": c.url})
		rec = do(t, h, "POST", "/-/save", bytes.NewReader(body),
			map[string]string{"Content-Type": "application/json"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("json save (%q, %q): status = %d, want 400", c.name, c.url, rec.Code)
		}
	}

	// Valid names accepted.
	for _, name := range []string{"x", "my-blog", "a/b/c", "v1.2.3", "a_b"} {
		form := url.Values{"name": {name}, "url": {"https://example.com"}}
		rec := do(t, h, "POST", "/-/save", strings.NewReader(form.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if rec.Code != http.StatusSeeOther {
			t.Errorf("save %q: status = %d, want 303 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestHomePageSearchAndEdit(t *testing.T) {
	h, st := newTestHandler(t, nil, false)
	ctx := context.Background()
	for _, l := range []*store.Link{
		{Name: "alpha", URL: "https://a.example.com", Owner: "alice"},
		{Name: "beta", URL: "https://b.example.com", Owner: "bob"},
	} {
		if err := st.Save(ctx, l, false); err != nil {
			t.Fatal(err)
		}
	}

	rec := do(t, h, "GET", "/", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alpha") || !strings.Contains(rec.Body.String(), "beta") {
		t.Fatalf("home: %d, contains alpha/beta: %v", rec.Code, strings.Contains(rec.Body.String(), "alpha"))
	}

	rec = do(t, h, "GET", "/?q=alpha", nil, nil)
	body := rec.Body.String()
	if !strings.Contains(body, "alpha") || strings.Contains(body, ">beta<") {
		t.Fatalf("search page: %s", body)
	}

	rec = do(t, h, "GET", "/-/edit/alpha", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://a.example.com") {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}

	// Non-owner cannot open edit form.
	hb, _ := New(Config{Store: st, Identity: func(*http.Request) Identity { return Identity{ID: "bob"} }})
	if rec := do(t, hb, "GET", "/-/edit/alpha", nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("bob edit status = %d, want 403", rec.Code)
	}
	if rec := do(t, h, "GET", "/-/edit/missing", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing edit status = %d", rec.Code)
	}
}

func TestExport(t *testing.T) {
	h, st := newTestHandler(t, nil, false)
	ctx := context.Background()
	if err := st.Save(ctx, &store.Link{Name: "x", URL: "https://x.example.com", Owner: "alice"}, false); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "GET", "/-/export", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d", rec.Code)
	}
	var l store.Link
	if err := json.Unmarshal(bytes.TrimSpace(rec.Body.Bytes()), &l); err != nil {
		t.Fatalf("export line: %q err %v", rec.Body.String(), err)
	}
	if l.Name != "x" || l.URL != "https://x.example.com" {
		t.Errorf("exported: %+v", l)
	}
}

func TestOpenMode(t *testing.T) {
	h, st := newTestHandler(t, func(*http.Request) Identity { return Identity{ID: "bob"} }, true)
	ctx := context.Background()
	if err := st.Save(ctx, &store.Link{Name: "x", URL: "https://x.example.com", Owner: "alice"}, false); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"x"}, "url": {"https://y.example.com"}}
	rec := do(t, h, "POST", "/-/save", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("open mode update: %d", rec.Code)
	}
}
