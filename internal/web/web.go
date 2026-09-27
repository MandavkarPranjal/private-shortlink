package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"private-shortlink/internal/store"
)

//go:embed templates
var templateFS embed.FS

// Identity is the requester as seen by the server, derived per mode
// (tailnet WhoIs, tailcat peer address, or local).
type Identity struct {
	ID      string
	IsAdmin bool
}

// IdentityFunc maps a request to an Identity. It must never fail; unknown
// callers get an empty ID.
type IdentityFunc func(r *http.Request) Identity

type Handler struct {
	st       *store.Store
	identity IdentityFunc
	open     bool
	tmpl     *template.Template
	mux      *http.ServeMux
}

type Config struct {
	Store    *store.Store
	Identity IdentityFunc // nil defaults to an anonymous, non-admin identity
	Open     bool         // disable ownership checks (ownership still recorded)
}

func New(cfg Config) (*Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("web: Store is required")
	}
	if cfg.Identity == nil {
		cfg.Identity = func(*http.Request) Identity { return Identity{} }
	}
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	h := &Handler{st: cfg.Store, identity: cfg.Identity, open: cfg.Open, tmpl: tmpl}
	h.mux = http.NewServeMux()
	h.routes()
	return h, nil
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /{$}", h.handleHome)
	h.mux.HandleFunc("POST /-/save", h.handleSave)
	h.mux.HandleFunc("POST /-/delete", h.handleDelete)
	h.mux.HandleFunc("GET /-/edit/{name...}", h.handleEdit)
	h.mux.HandleFunc("GET /-/export", h.handleExport)
	h.mux.HandleFunc("GET /-/api", h.handleAPIList)
	h.mux.HandleFunc("GET /-/api/{name...}", h.handleAPIGet)
	h.mux.HandleFunc("GET /{name...}", h.handleRedirect)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// ---------- helpers ----------

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Type"), "application/json")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// flash redirects to / with a message shown on the home page.
func flash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	q := url.Values{"msg": {msg}, "kind": {kind}}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

// validName reports whether name is a safe link name: lowercase
// alnum plus "-", ".", "_" in slash-separated segments.
func validName(name string) bool {
	if name == "" || len(name) > 100 || strings.HasPrefix(name, "-") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
				r == '-', r == '.', r == '_':
			default:
				return false
			}
		}
	}
	return true
}

func validURL(raw string) bool {
	if raw == "" || len(raw) > 4096 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// canModify reports whether id may create/update/delete l.
func (h *Handler) canModify(id Identity, l *store.Link) bool {
	if h.open || id.IsAdmin {
		return true
	}
	return l.Owner == id.ID
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// cleanName trims trailing slashes from a request path name.
func cleanName(name string) string {
	return strings.Trim(name, "/")
}

// ---------- handlers ----------

type homeData struct {
	Links    []*store.Link
	Query    string
	Identity Identity
	Msg      string
	MsgKind  string
	FormName string
	FormURL  string
	ShowForm bool
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handler) handleHome(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	links, err := h.st.List(r.Context(), q)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.render(w, http.StatusOK, "index.html", homeData{
		Links:    links,
		Query:    q,
		Identity: h.identity(r),
		Msg:      r.URL.Query().Get("msg"),
		MsgKind:  r.URL.Query().Get("kind"),
		ShowForm: true,
	})
}

func (h *Handler) handleSave(w http.ResponseWriter, r *http.Request) {
	id := h.identity(r)
	jsonMode := wantsJSON(r)

	var name, rawurl string
	if jsonMode {
		var req struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		name, rawurl = req.Name, req.URL
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name, rawurl = r.FormValue("name"), r.FormValue("url")
	}
	name = strings.TrimSpace(name)
	rawurl = strings.TrimSpace(rawurl)

	if !validName(name) {
		msg := "invalid name: use lowercase letters, digits, -, ., _ separated by /"
		if jsonMode {
			writeJSONErr(w, http.StatusBadRequest, msg)
			return
		}
		h.renderHomeError(w, r, http.StatusBadRequest, msg, name, rawurl)
		return
	}
	if !validURL(rawurl) {
		msg := "invalid URL: must be an absolute http:// or https:// URL"
		if jsonMode {
			writeJSONErr(w, http.StatusBadRequest, msg)
			return
		}
		h.renderHomeError(w, r, http.StatusBadRequest, msg, name, rawurl)
		return
	}

	existing, err := h.st.Get(r.Context(), name)
	switch {
	case err == nil:
		if !h.canModify(id, existing) {
			msg := fmt.Sprintf("link %q is owned by %s", name, existing.Owner)
			if jsonMode {
				writeJSONErr(w, http.StatusForbidden, msg)
				return
			}
			http.Error(w, msg, http.StatusForbidden)
			return
		}
	case isNotFound(err):
		existing = nil
	default:
		if jsonMode {
			writeJSONErr(w, http.StatusInternalServerError, "database error")
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	l := &store.Link{Name: name, URL: rawurl, Owner: id.ID}
	if err := h.st.Save(r.Context(), l, existing != nil); err != nil {
		if errors.Is(err, store.ErrExists) {
			if jsonMode {
				writeJSONErr(w, http.StatusConflict, "link already exists")
				return
			}
			h.renderHomeError(w, r, http.StatusConflict, "link already exists", name, rawurl)
			return
		}
		if jsonMode {
			writeJSONErr(w, http.StatusInternalServerError, "database error")
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	if jsonMode {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "link": l})
		return
	}
	msg := fmt.Sprintf("saved %s → %s", l.Name, l.URL)
	flash(w, r, "ok", msg)
}

func (h *Handler) renderHomeError(w http.ResponseWriter, r *http.Request, status int, msg, name, rawurl string) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	links, err := h.st.List(r.Context(), q)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.render(w, status, "index.html", homeData{
		Links:    links,
		Query:    q,
		Identity: h.identity(r),
		Msg:      msg,
		MsgKind:  "err",
		FormName: name,
		FormURL:  rawurl,
		ShowForm: true,
	})
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := h.identity(r)
	jsonMode := wantsJSON(r)

	var name string
	if jsonMode {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		name = req.Name
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name = r.FormValue("name")
	}
	name = strings.TrimSpace(name)

	l, err := h.st.Get(r.Context(), name)
	if err != nil {
		if isNotFound(err) {
			if jsonMode {
				writeJSONErr(w, http.StatusNotFound, "link not found")
				return
			}
			flash(w, r, "err", fmt.Sprintf("link %q not found", name))
			return
		}
		if jsonMode {
			writeJSONErr(w, http.StatusInternalServerError, "database error")
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if !h.canModify(id, l) {
		msg := fmt.Sprintf("link %q is owned by %s", name, l.Owner)
		if jsonMode {
			writeJSONErr(w, http.StatusForbidden, msg)
			return
		}
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	if err := h.st.Delete(r.Context(), name); err != nil {
		if jsonMode {
			writeJSONErr(w, http.StatusInternalServerError, "database error")
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if jsonMode {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": name})
		return
	}
	flash(w, r, "ok", fmt.Sprintf("deleted %s", name))
}

func (h *Handler) handleEdit(w http.ResponseWriter, r *http.Request) {
	name := cleanName(r.PathValue("name"))
	l, err := h.st.Get(r.Context(), name)
	if err != nil {
		if isNotFound(err) {
			http.Error(w, "link not found", http.StatusNotFound)
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	id := h.identity(r)
	if !h.canModify(id, l) {
		http.Error(w, fmt.Sprintf("link %q is owned by %s", l.Name, l.Owner), http.StatusForbidden)
		return
	}
	h.render(w, http.StatusOK, "edit.html", struct {
		Link     *store.Link
		Identity Identity
		Msg      string
		MsgKind  string
	}{Link: l, Identity: id, Msg: r.URL.Query().Get("msg"), MsgKind: r.URL.Query().Get("kind")})
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	links, err := h.st.List(r.Context(), "")
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	enc := json.NewEncoder(w)
	for _, l := range links {
		if err := enc.Encode(l); err != nil {
			return
		}
	}
}

func (h *Handler) handleAPIList(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	links, err := h.st.List(r.Context(), q)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if links == nil {
		links = []*store.Link{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "links": links})
}

func (h *Handler) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	name := cleanName(r.PathValue("name"))
	l, err := h.st.Get(r.Context(), name)
	if err != nil {
		if isNotFound(err) {
			writeJSONErr(w, http.StatusNotFound, "link not found")
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "link": l})
}

func (h *Handler) handleRedirect(w http.ResponseWriter, r *http.Request) {
	name := cleanName(r.PathValue("name"))
	// Management URLs under /-/ that no explicit route matched are 404s,
	// not link names (link names may not start with "-").
	if name == "" || strings.HasPrefix(name, "-") {
		http.NotFound(w, r)
		return
	}
	l, err := h.st.Get(r.Context(), name)
	if err != nil {
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if err := h.st.IncClick(r.Context(), name); err != nil {
		// Redirecting is more important than counting.
		_ = err
	}
	http.Redirect(w, r, l.URL, http.StatusFound)
}
