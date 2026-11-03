package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type App struct {
	cfg   Config
	store *Store

	mu        sync.RWMutex
	links     map[string]Link   // host + path -> link
	fallbacks map[string]string // host -> fallback template
	rendered  []byte

	clicks chan clickEvent
	// probeID marks signpost's own verification requests, so they are told
	// apart from real visitors and never counted.
	probeID string
}

type clickEvent struct {
	id int64
	c  Click
}

func serve(cfg Config) error {
	if cfg.Token == "" {
		log.Println("warning: SIGNPOST_TOKEN is empty, so the API only accepts Authelia's Remote-User")
	}
	st, err := OpenStore(cfg.DB)
	if err != nil {
		return err
	}
	b := make([]byte, 16)
	rand.Read(b)
	app := &App{cfg: cfg, store: st, clicks: make(chan clickEvent, 1024), probeID: hex.EncodeToString(b)}
	if err := app.reload(); err != nil {
		return err
	}
	go app.clickWriter()

	pub := &http.Server{Addr: cfg.Listen, Handler: http.HandlerFunc(app.redirect), ReadHeaderTimeout: 10 * time.Second}
	adm := &http.Server{Addr: cfg.AdminListen, Handler: app.adminRoutes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- pub.ListenAndServe() }()
	go func() { errc <- adm.ListenAndServe() }()
	log.Printf("redirects on %s, admin on %s", cfg.Listen, cfg.AdminListen)

	// Re-check anything left pending by a previous run.
	if links, err := st.Links(); err == nil {
		for _, l := range links {
			if l.Check == "pending" {
				go app.verify(l.ID)
			}
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-sig:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub.Shutdown(ctx)
	adm.Shutdown(ctx)
	close(app.clicks)
	return nil
}

// reload rebuilds the in-memory lookup table and the Traefik config from the
// database. Called after every write.
func (a *App) reload() error {
	domains, err := a.store.Domains()
	if err != nil {
		return err
	}
	links, err := a.store.Links()
	if err != nil {
		return err
	}
	rendered, err := Render(a.cfg, domains, links)
	if err != nil {
		return err
	}
	table := make(map[string]Link, len(links))
	for _, l := range links {
		table[l.Host+l.Path] = l
	}
	fb := map[string]string{}
	for _, d := range domains {
		if d.Fallback != "" {
			fb[d.Host] = d.Fallback
		}
	}
	a.mu.Lock()
	a.links, a.fallbacks, a.rendered = table, fb, rendered
	a.mu.Unlock()
	if a.cfg.TraefikFile != "" {
		if err := WriteAtomic(a.cfg.TraefikFile, rendered); err != nil {
			return fmt.Errorf("write %s: %w", a.cfg.TraefikFile, err)
		}
	}
	return nil
}

func requestHost(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

func expand(tmpl, path string) string {
	return strings.ReplaceAll(tmpl, "{path}", strings.TrimPrefix(path, "/"))
}

func withQuery(target, rawQuery string) string {
	if rawQuery == "" {
		return target
	}
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	if u.RawQuery == "" {
		u.RawQuery = rawQuery
	} else {
		u.RawQuery += "&" + rawQuery
	}
	return u.String()
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	a.mu.RLock()
	l, ok := a.links[host+r.URL.Path]
	if !ok && len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") {
		l, ok = a.links[host+strings.TrimSuffix(r.URL.Path, "/")]
	}
	if !ok {
		l, ok = a.links[host+"*"]
	}
	fallback := a.fallbacks[host]
	a.mu.RUnlock()

	w.Header().Set("X-Signpost", "1")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "unsafe-url")
	if !ok {
		if fallback != "" {
			http.Redirect(w, r, withQuery(expand(fallback, r.URL.Path), r.URL.RawQuery), http.StatusFound)
			return
		}
		http.Error(w, "no signpost here", http.StatusNotFound)
		return
	}

	target := expand(l.Target, r.URL.Path)
	if l.KeepQuery {
		target = withQuery(target, r.URL.RawQuery)
	}
	w.Header().Set("X-Signpost-Link", strconv.FormatInt(l.ID, 10))
	http.Redirect(w, r, target, l.Status)

	if r.Method == http.MethodGet && r.Header.Get("X-Signpost-Probe") != a.probeID {
		select {
		case a.clicks <- clickEvent{l.ID, Click{
			At:       time.Now().Unix(),
			Referrer: trunc(r.Referer(), 300),
			Country:  r.Header.Get("Cf-Ipcountry"),
			UA:       trunc(r.UserAgent(), 300),
		}}:
		default: // never let stats slow a redirect down
		}
	}
}

func (a *App) clickWriter() {
	for ev := range a.clicks {
		if err := a.store.RecordClick(ev.id, ev.c); err != nil {
			log.Printf("click %d: %v", ev.id, err)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- admin ----

func (a *App) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			tok, ok := strings.CutPrefix(h, "Bearer ")
			if ok && a.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			jsonErr(w, http.StatusUnauthorized, "bad token")
			return
		}
		if a.cfg.TrustRemoteUser && r.Header.Get("Remote-User") != "" {
			next.ServeHTTP(w, r)
			return
		}
		jsonErr(w, http.StatusUnauthorized, "sign in through Authelia or send a bearer token")
	})
}

func (a *App) adminRoutes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/links", a.apiLinks)
	api.HandleFunc("POST /api/links", a.apiCreateLink)
	api.HandleFunc("GET /api/links/{id}", a.apiLink)
	api.HandleFunc("PATCH /api/links/{id}", a.apiUpdateLink)
	api.HandleFunc("DELETE /api/links/{id}", a.apiDeleteLink)
	api.HandleFunc("GET /api/links/{id}/clicks", a.apiClicks)
	api.HandleFunc("POST /api/links/{id}/check", a.apiCheck)
	api.HandleFunc("GET /api/domains", a.apiDomains)
	api.HandleFunc("PUT /api/domains/{host}", a.apiPutDomain)
	api.HandleFunc("DELETE /api/domains/{host}", a.apiDeleteDomain)
	api.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"user": r.Header.Get("Remote-User"), "admin_host": a.cfg.AdminHost, "default_resolver": a.cfg.CertResolver})
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", a.authed(api))
	// Unauthenticated on purpose: Traefik's HTTP provider polls this over the
	// docker network. It holds hostnames and paths, nothing secret. Targets
	// are not in it.
	mux.HandleFunc("GET /traefik.yml", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		body := a.rendered
		a.mu.RUnlock()
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(body)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/", a.authed(http.FileServerFS(static)))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (a *App) apiLinks(w http.ResponseWriter, r *http.Request) {
	links, err := a.store.Links()
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	writeJSON(w, links)
}

func (a *App) apiLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	l, err := a.store.Link(id)
	if err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	writeJSON(w, l)
}

type linkInput struct {
	Host      string `json:"host"`
	Path      string `json:"path"`
	Target    string `json:"target"`
	Status    int    `json:"status"`
	KeepQuery *bool  `json:"keep_query"`
	Note      string `json:"note"`
	Force     bool   `json:"force"`
}

func validTarget(t string) error {
	u, err := url.Parse(strings.ReplaceAll(t, "{path}", "x"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("target must be an absolute http(s) URL")
	}
	return nil
}

func validStatus(s int) bool { return s == 301 || s == 302 || s == 307 || s == 308 }

func (a *App) apiCreateLink(w http.ResponseWriter, r *http.Request) {
	var in linkInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	l := Link{
		Host:   strings.ToLower(strings.TrimSpace(in.Host)),
		Path:   strings.TrimSpace(in.Path),
		Target: strings.TrimSpace(in.Target),
		Status: in.Status,
		Note:   in.Note,
	}
	if l.Path == "/*" {
		l.Path = "*"
	}
	if l.Path != "*" && !strings.HasPrefix(l.Path, "/") {
		l.Path = "/" + l.Path
	}
	if l.Path != "*" && len(l.Path) > 1 {
		l.Path = strings.TrimSuffix(l.Path, "/")
	}
	l.KeepQuery = in.KeepQuery == nil || *in.KeepQuery
	if l.Status == 0 {
		l.Status = 302
	}
	switch {
	case !hostRe.MatchString(l.Host):
		jsonErr(w, 400, "host must be a plain hostname like milind.dev")
		return
	case l.Host == a.cfg.AdminHost:
		jsonErr(w, 400, "that host is signpost's own admin UI")
		return
	case l.Path != "*" && (l.Path == "/" || !pathRe.MatchString(l.Path)):
		jsonErr(w, 400, "path may use letters, digits and . _ ~ - / only, and cannot be the root")
		return
	case !validStatus(l.Status):
		jsonErr(w, 400, "status must be 301, 302, 307 or 308")
		return
	}
	if err := validTarget(l.Target); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if !in.Force {
		if msg := a.conflict(r.Context(), l); msg != "" {
			jsonErr(w, 409, msg+" (pass force to take it anyway)")
			return
		}
	}
	l, err := a.store.CreateLink(l, a.cfg.CertResolver)
	if err != nil {
		jsonErr(w, 409, err.Error())
		return
	}
	if err := a.reload(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	go a.verify(l.ID)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, l)
}

func (a *App) apiUpdateLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	l, err := a.store.Link(id)
	if err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	var in struct {
		Target    *string `json:"target"`
		Status    *int    `json:"status"`
		KeepQuery *bool   `json:"keep_query"`
		Note      *string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	if in.Target != nil {
		if err := validTarget(*in.Target); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		l.Target = strings.TrimSpace(*in.Target)
	}
	if in.Status != nil {
		if !validStatus(*in.Status) {
			jsonErr(w, 400, "status must be 301, 302, 307 or 308")
			return
		}
		l.Status = *in.Status
	}
	if in.KeepQuery != nil {
		l.KeepQuery = *in.KeepQuery
	}
	if in.Note != nil {
		l.Note = *in.Note
	}
	if l, err = a.store.UpdateLink(l); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	if err := a.reload(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	writeJSON(w, l)
}

func (a *App) apiDeleteLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	if err := a.store.DeleteLink(id); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	if err := a.reload(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) apiClicks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	c, err := a.store.Clicks(id, 500)
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	writeJSON(w, c)
}

func (a *App) apiCheck(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	if _, err := a.store.Link(id); err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	a.store.SetCheck(id, "pending", "")
	go a.verify(id)
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) apiDomains(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.Domains()
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	writeJSON(w, d)
}

func (a *App) apiPutDomain(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.PathValue("host"))
	if !hostRe.MatchString(host) || host == a.cfg.AdminHost {
		jsonErr(w, 400, "bad host")
		return
	}
	var d Domain
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	d.Host = host
	if d.Fallback != "" {
		if err := validTarget(d.Fallback); err != nil {
			jsonErr(w, 400, "fallback: "+err.Error())
			return
		}
	}
	if d.CertResolver == "" {
		d.CertResolver = a.cfg.CertResolver
	}
	if err := a.store.PutDomain(d); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	if err := a.reload(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	writeJSON(w, d)
}

func (a *App) apiDeleteDomain(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteDomain(strings.ToLower(r.PathValue("host"))); err != nil {
		code := 409
		if errors.Is(err, ErrNotFound) {
			code = 404
		}
		jsonErr(w, code, err.Error())
		return
	}
	if err := a.reload(); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
