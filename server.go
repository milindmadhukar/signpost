package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type App struct {
	cfg    Config
	store  *Store
	secret []byte

	// table is the whole routing table, rebuilt from the database and swapped
	// in whole. The redirect path does one atomic load and never takes a lock.
	table    atomic.Pointer[routeTable]
	reloadMu sync.Mutex // serialises rebuilds, so an older snapshot never lands last

	clicks  chan clickEvent
	dropped atomic.Int64 // clicks lost to a full queue, reported once a minute
	// probeID marks signpost's own verification requests, so they are told
	// apart from real visitors and never counted.
	probeID string
	guesses *limiter // password attempts per (IP, link)
	geo     *geoDB   // nil without SIGNPOST_GEOIP_DB
	checks  chan int64
}

// route is the slice of a link the redirect path needs. Display fields
// (counts, notes, probe results) stay in the database, so nothing in here
// goes stale as clicks arrive.
type route struct {
	id        int64
	target    string
	status    int
	keepQuery bool
	passHash  string
	expiresAt int64 // 0 = never
	disabled  bool
	url       string // host + path, for the public pages
}

type routeKey struct{ host, path string }

type routeTable struct {
	links     map[routeKey]*route
	fallbacks map[string]string // host -> fallback template
	tokens    map[string]string // host -> DNS-check token
	rendered  []byte            // Traefik dynamic config
	etag      string
	built     time.Time
}

func newApp(cfg Config, st *Store) (*App, error) {
	secret, err := st.Secret()
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	a := &App{
		cfg: cfg, store: st, secret: secret,
		clicks:  make(chan clickEvent, 4096),
		probeID: hex.EncodeToString(b),
		guesses: newLimiter(5, 5),
		checks:  make(chan int64, 100000),
	}
	return a, a.reload()
}

func serve(cfg Config) error {
	if cfg.Token == "" {
		log.Println("warning: SIGNPOST_TOKEN is empty, so the API only accepts Authelia's Remote-User")
	}
	st, err := OpenStore(cfg.DB)
	if err != nil {
		return err
	}
	app, err := newApp(cfg, st)
	if err != nil {
		return err
	}
	if cfg.GeoIPDB != "" {
		// A missing or broken file costs countries, not redirects, so it
		// is reported and signpost carries on without it.
		if g, err := openGeo(cfg.GeoIPDB); err != nil {
			log.Printf("warning: SIGNPOST_GEOIP_DB: %v; countries will come from Cloudflare only", err)
		} else {
			app.geo = g
			geoCredit.Store(strings.HasPrefix(strings.ToUpper(g.desc), "DBIP"))
			defer g.Close()
			log.Printf("country lookups from %s (%s, built %s)", cfg.GeoIPDB, g.desc, g.built.Format("2006-01-02"))
		}
	}
	writerDone := make(chan struct{})
	go func() { app.clickWriter(time.Second, 500); close(writerDone) }()
	go app.watch(2*time.Second, 5*time.Minute)
	go app.reportDropped()
	for range 16 {
		go app.verifyWorker()
	}
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
	log.Printf("signpost %s: redirects on %s, admin on %s", release, cfg.Listen, cfg.AdminListen)

	// Re-check anything left pending by a previous run.
	if links, err := st.Links(); err == nil {
		for _, l := range links {
			if l.Check == "pending" {
				app.queueVerify(l.ID)
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
	// Handlers are done, so nothing sends on clicks any more: flush the
	// queue before exiting.
	close(app.clicks)
	select {
	case <-writerDone:
	case <-ctx.Done():
	}
	return st.Close()
}

// reload rebuilds the routing table and the Traefik config from the
// database. Every write in this process calls it; watch covers writes from
// anywhere else.
func (a *App) reload() error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
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
	t := &routeTable{
		links:     make(map[routeKey]*route, len(links)),
		fallbacks: map[string]string{},
		tokens:    map[string]string{},
		rendered:  rendered,
		built:     time.Now(),
	}
	sum := sha256.Sum256(rendered)
	t.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	for _, l := range links {
		rt := &route{id: l.ID, target: l.Target, status: l.Status, keepQuery: l.KeepQuery,
			passHash: l.PassHash, disabled: l.Disabled, url: l.URL()}
		if l.ExpiresAt != nil {
			rt.expiresAt = *l.ExpiresAt
		}
		t.links[routeKey{l.Host, l.Path}] = rt
	}
	for _, d := range domains {
		t.tokens[d.Host] = d.Token
		if d.Fallback != "" {
			t.fallbacks[d.Host] = d.Fallback
		}
	}
	a.table.Store(t)
	if a.cfg.TraefikFile != "" {
		if err := WriteAtomic(a.cfg.TraefikFile, rendered); err != nil {
			return fmt.Errorf("write %s: %w", a.cfg.TraefikFile, err)
		}
	}
	return nil
}

// watch keeps the cache honest against writes this process did not make:
// someone editing the file with the sqlite3 shell, or a restored backup.
// PRAGMA data_version moves only when another connection commits, so the
// poll is one cheap query and almost never triggers a rebuild. A full
// rebuild every `every` is a backstop on top.
func (a *App) watch(poll, every time.Duration) {
	last, _ := a.store.DataVersion()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	lastFull := time.Now()
	for range tick.C {
		v, err := a.store.DataVersion()
		if err != nil {
			continue
		}
		if v != last || time.Since(lastFull) >= every {
			if err := a.reload(); err != nil {
				log.Printf("reload: %v", err)
				continue
			}
			last, lastFull = v, time.Now()
		}
	}
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

func (t *routeTable) lookup(host, path string) (*route, bool) {
	if rt, ok := t.links[routeKey{host, path}]; ok {
		return rt, true
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		if rt, ok := t.links[routeKey{host, strings.TrimSuffix(path, "/")}]; ok {
			return rt, true
		}
	}
	rt, ok := t.links[routeKey{host, "*"}]
	return rt, ok
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	t := a.table.Load()
	if tok, ok := strings.CutPrefix(r.URL.Path, wellKnown); ok {
		want := t.tokens[host]
		if want == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("signpost:" + want))
		return
	}
	rt, ok := t.lookup(host, r.URL.Path)

	w.Header().Set("X-Signpost", "1")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "unsafe-url")
	if !ok {
		if fb := t.fallbacks[host]; fb != "" {
			http.Redirect(w, r, withQuery(expand(fb, r.URL.Path), r.URL.RawQuery), http.StatusFound)
			return
		}
		http.Error(w, "no signpost here", http.StatusNotFound)
		return
	}
	w.Header().Set("X-Signpost-Link", strconv.FormatInt(rt.id, 10))
	target := expand(rt.target, r.URL.Path)
	if rt.keepQuery {
		target = withQuery(target, r.URL.RawQuery)
	}
	// signpost's own probe only asks "is this route reaching me", so it gets
	// the plain redirect whatever state the link is in, and is never counted.
	if r.Header.Get("X-Signpost-Probe") == a.probeID {
		http.Redirect(w, r, target, rt.status)
		return
	}
	if rt.disabled || (rt.expiresAt > 0 && time.Now().Unix() >= rt.expiresAt) {
		servePublic(w, http.StatusGone, publicView{Title: "Link no longer active", Link: rt.url, Gone: true})
		return
	}
	if rt.passHash != "" && !a.unlocked(r, rt) {
		a.passwordGate(w, r, rt, target)
		return
	}
	http.Redirect(w, r, target, rt.status)
	if r.Method == http.MethodGet {
		a.count(r, rt.id)
	}
}

// passwordGate shows the prompt on GET and checks the answer on POST. The
// POST lands on the same path, which the link's Traefik router already sends
// here.
func (a *App) passwordGate(w http.ResponseWriter, r *http.Request, rt *route, target string) {
	v := publicView{Title: "Password required", Link: rt.url}
	if r.Method != http.MethodPost {
		servePublic(w, http.StatusOK, v)
		return
	}
	if !a.guesses.allow(clientIP(r) + "|" + strconv.FormatInt(rt.id, 10)) {
		w.Header().Set("Retry-After", "60")
		v.Err = "Too many attempts. Wait a minute and try again."
		servePublic(w, http.StatusTooManyRequests, v)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !checkPassword(rt.passHash, r.PostFormValue("password")) {
		v.Err = "That password is not right."
		servePublic(w, http.StatusUnauthorized, v)
		return
	}
	exp := time.Now().Add(unlockTTL).Unix()
	path := r.URL.Path
	if strings.HasSuffix(path, "/") && len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	http.SetCookie(w, &http.Cookie{
		Name: unlockCookie(rt.id), Value: a.unlockToken(rt.id, rt.passHash, exp),
		Path: path, MaxAge: int(unlockTTL.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, target, http.StatusSeeOther)
	a.count(r, rt.id)
}

// count queues a click. The redirect has already been written, and a full
// queue drops the click rather than make anyone wait.
func (a *App) count(r *http.Request, id int64) {
	ua := r.UserAgent()
	parsed := parseUA(ua)
	now := time.Now()
	c := Click{
		At:       now.Unix(),
		Referrer: trunc(r.Referer(), 300),
		RefHost:  refHost(r.Referer()),
		Country:  clickCountry(r, a.geo),
		UA:       trunc(ua, 300),
		Device:   parsed.Device,
		Browser:  parsed.Browser,
		OS:       parsed.OS,
		Bot:      parsed.Bot,
		Visitor:  a.visitor(now, clientIP(r), ua),
	}
	select {
	case a.clicks <- clickEvent{id, c}:
	default:
		a.dropped.Add(1)
	}
}

// visitor is a daily-rotating pseudonym: the same person on the same day
// hashes the same, so uniques can be counted, but the salt changes every day
// and no IP is ever stored, so visitors cannot be followed across days.
func (a *App) visitor(now time.Time, ip, ua string) string {
	m := hmac.New(sha256.New, a.secret)
	fmt.Fprintf(m, "visitor|%s|%s|%s", now.UTC().Format("2006-01-02"), ip, ua)
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// clickWriter batches clicks into one transaction per flush: up to max
// events, or whatever arrived within every. One fsync per batch instead of
// per click keeps a burst of traffic from queueing behind the disk.
func (a *App) clickWriter(every time.Duration, max int) {
	batch := make([]clickEvent, 0, max)
	tick := time.NewTicker(every)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.store.RecordClicks(batch); err != nil {
			log.Printf("clicks: lost %d: %v", len(batch), err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case ev, ok := <-a.clicks:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= max {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (a *App) reportDropped() {
	for range time.Tick(time.Minute) {
		if n := a.dropped.Swap(0); n > 0 {
			log.Printf("clicks: queue full, dropped %d in the last minute", n)
		}
	}
}

// queueVerify hands a link to the verify workers. A bulk import of hundreds
// of links then probes sixteen at a time instead of all at once.
func (a *App) queueVerify(id int64) {
	select {
	case a.checks <- id:
	default:
		go a.verify(id)
	}
}

func (a *App) verifyWorker() {
	for id := range a.checks {
		a.verify(id)
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
