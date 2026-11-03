package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	tokens    map[string]string // host -> DNS-check token
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
	go func() {
		// Let Traefik poll the verify routes before the first sweep.
		time.Sleep(15 * time.Second)
		app.dnsLoop()
	}()

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
	fb, tok := map[string]string{}, map[string]string{}
	for _, d := range domains {
		tok[d.Host] = d.Token
		if d.Fallback != "" {
			fb[d.Host] = d.Fallback
		}
	}
	a.mu.Lock()
	a.links, a.fallbacks, a.tokens, a.rendered = table, fb, tok, rendered
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
	if tok, ok := strings.CutPrefix(r.URL.Path, wellKnown); ok {
		a.mu.RLock()
		want := a.tokens[host]
		a.mu.RUnlock()
		if want == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("signpost:" + want))
		return
	}
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
