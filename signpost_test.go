package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{ServiceName: "signpost", UpstreamURL: "http://signpost:8080", EntryPoints: []string{"websecure"}, AdminHost: "links.test"}
}

// newTestApp opens a fresh database with the given hosts already connected,
// so nothing in a test kicks off a real DNS check.
func newTestApp(t *testing.T, hosts ...string) (*App, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signpost.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, h := range hosts {
		if err := st.PutDomain(Domain{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := newApp(testConfig(), st)
	if err != nil {
		t.Fatal(err)
	}
	return a, path
}

func mustCreate(t *testing.T, a *App, ls ...Link) []Link {
	t.Helper()
	for i := range ls {
		if ls[i].Status == 0 {
			ls[i].Status = 302
		}
	}
	out, err := a.store.CreateLinks(ls, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.reload(); err != nil {
		t.Fatal(err)
	}
	return out
}

func get(a *App, method, host, target string, body url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Host = host
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.redirect(w, req)
	return w
}

func TestMigrateFromUnversionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	if err := migrations[0](tx); err != nil {
		t.Fatal(err)
	}
	tx.Exec(`INSERT INTO domains (host, created_at) VALUES ('milind.dev', 1)`)
	tx.Exec(`INSERT INTO links (host, path, target, created_at, updated_at, clicks) VALUES ('milind.dev', '/ade', 'https://example.com', 1, 1, 1)`)
	tx.Exec(`INSERT INTO clicks (link_id, at, referrer, ua) VALUES (1, 100, 'https://l.instagram.com/', 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) Instagram 350')`)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close() // user_version is still 0, like a database from before versioning

	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v int
	st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version = %d, want %d", v, len(migrations))
	}
	l, err := st.LinkByURL("milind.dev", "/ade")
	if err != nil || l.Target != "https://example.com" || l.Clicks != 1 || l.Protected || l.Disabled {
		t.Fatalf("link after migration: %+v %v", l, err)
	}
	cs, _ := st.Clicks(l.ID, 10)
	if len(cs) != 1 || cs[0].RefHost != "instagram.com" || cs[0].Browser != "Instagram" || cs[0].Device != "mobile" {
		t.Fatalf("old click not backfilled: %+v", cs)
	}
	// Opening again is a no-op.
	st.Close()
	if st, err = OpenStore(path); err != nil {
		t.Fatal(err)
	}
}

func TestRedirects(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev", "go.test")
	past := time.Now().Add(-time.Hour).Unix()
	mustCreate(t, a,
		Link{Host: "milind.dev", Path: "/ade", Target: "https://example.com/ade", KeepQuery: true},
		Link{Host: "milind.dev", Path: "/noq", Target: "https://example.com/noq?a=1", KeepQuery: false, Status: 301},
		Link{Host: "milind.dev", Path: "/old", Target: "https://example.com/old", ExpiresAt: &past},
		Link{Host: "milind.dev", Path: "/off", Target: "https://example.com/off", Disabled: true},
		Link{Host: "go.test", Path: "*", Target: "https://luma.com/{path}", KeepQuery: true},
		Link{Host: "go.test", Path: "/exact", Target: "https://example.com/exact"},
	)
	cases := []struct {
		host, path string
		code       int
		loc        string
	}{
		{"milind.dev", "/ade", 302, "https://example.com/ade"},
		{"milind.dev", "/ade/", 302, "https://example.com/ade"},
		{"milind.dev", "/ade?utm_source=ig", 302, "https://example.com/ade?utm_source=ig"},
		{"MILIND.DEV.", "/ade", 302, "https://example.com/ade"},
		{"milind.dev", "/noq?x=2", 301, "https://example.com/noq?a=1"},
		{"milind.dev", "/old", 410, ""},
		{"milind.dev", "/off", 410, ""},
		{"milind.dev", "/nope", 404, ""},
		{"go.test", "/winter-26", 302, "https://luma.com/winter-26"},
		{"go.test", "/a/b?x=1", 302, "https://luma.com/a/b?x=1"},
		{"go.test", "/exact", 302, "https://example.com/exact"},
	}
	for _, c := range cases {
		w := get(a, "GET", c.host, c.path, nil)
		if w.Code != c.code || w.Header().Get("Location") != c.loc {
			t.Errorf("%s%s: got %d %q, want %d %q", c.host, c.path, w.Code, w.Header().Get("Location"), c.code, c.loc)
		}
	}
}

func TestPasswordLink(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev")
	hash, err := hashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	l := mustCreate(t, a, Link{Host: "milind.dev", Path: "/press", Target: "https://example.com/kit", PassHash: hash})[0]

	if w := get(a, "GET", "milind.dev", "/press", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "password protected") || w.Header().Get("Location") != "" {
		t.Fatalf("GET without cookie: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(a, "POST", "milind.dev", "/press", url.Values{"password": {"wrong"}}); w.Code != 401 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w := get(a, "POST", "milind.dev", "/press", url.Values{"password": {"hunter2"}})
	if w.Code != 303 || w.Header().Get("Location") != "https://example.com/kit" {
		t.Fatalf("right password: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookie := w.Result().Cookies()[0]
	if cookie.Path != "/press" || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("cookie attributes: %+v", cookie)
	}
	if w := get(a, "GET", "milind.dev", "/press", nil, cookie); w.Code != 302 {
		t.Fatalf("with cookie: %d", w.Code)
	}
	forged := &http.Cookie{Name: cookie.Name, Value: "9999999999.00"}
	if w := get(a, "GET", "milind.dev", "/press", nil, forged); w.Code != 200 {
		t.Fatalf("forged cookie got through: %d", w.Code)
	}

	// A new password invalidates every cookie issued for the old one.
	pw := "new-one"
	if _, err := a.UpdateLink(l.ID, linkPatch{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	if w := get(a, "GET", "milind.dev", "/press", nil, cookie); w.Code != 200 {
		t.Fatalf("old cookie still works after password change: %d", w.Code)
	}

	// Guesses are rate limited per IP and link (the first attempts above
	// came from the same test IP, so the bucket is partly used).
	codes := map[int]int{}
	for range 8 {
		codes[get(a, "POST", "milind.dev", "/press", url.Values{"password": {"nope"}}).Code]++
	}
	if codes[429] == 0 {
		t.Fatalf("no 429 after repeated guesses: %v", codes)
	}
}

func TestProbeSeesPlainRedirect(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev")
	hash, _ := hashPassword("x")
	mustCreate(t, a, Link{Host: "milind.dev", Path: "/p", Target: "https://example.com", PassHash: hash, Disabled: true})
	req := httptest.NewRequest("GET", "/p", nil)
	req.Host = "milind.dev"
	req.Header.Set("X-Signpost-Probe", a.probeID)
	w := httptest.NewRecorder()
	a.redirect(w, req)
	if w.Code != 302 || w.Header().Get("X-Signpost-Link") == "" {
		t.Fatalf("probe got %d", w.Code)
	}
	if len(a.clicks) != 0 {
		t.Fatal("probe was counted as a click")
	}
}

func TestClicksAreCountedButBotsAreNot(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev")
	l := mustCreate(t, a, Link{Host: "milind.dev", Path: "/ade", Target: "https://example.com", KeepQuery: true})[0]
	for _, ua := range []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1",
		"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
		"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
	} {
		req := httptest.NewRequest("GET", "/ade", nil)
		req.Host = "milind.dev"
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Referer", "https://l.instagram.com/?u=x")
		a.redirect(httptest.NewRecorder(), req)
	}
	close(a.clicks)
	a.clickWriter(time.Hour, 500) // drains and flushes once the channel is closed
	got, _ := a.store.Link(l.ID)
	if got.Clicks != 2 {
		t.Fatalf("clicks = %d, want 2 (bots excluded)", got.Clicks)
	}
	cs, _ := a.store.Clicks(l.ID, 10)
	if len(cs) != 4 {
		t.Fatalf("recorded %d clicks, want all 4 kept", len(cs))
	}
	st, err := a.store.Stats(statsQuery{LinkID: l.ID, Range: parseRange("24h"), Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if st.Clicks != 2 || st.Bots != 2 || st.Referrers[0].Key != "instagram.com" {
		t.Fatalf("stats: clicks %d bots %d refs %+v", st.Clicks, st.Bots, st.Referrers)
	}
}

// A write from outside this process (a sqlite3 shell, a restored backup)
// must reach the routing table without a restart.
func TestCacheReloadsOnExternalWrite(t *testing.T) {
	a, path := newTestApp(t, "milind.dev")
	go a.watch(20*time.Millisecond, time.Hour)

	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`INSERT INTO links (host, path, target, created_at, updated_at) VALUES ('milind.dev', '/outside', 'https://example.com/outside', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := a.table.Load().lookup("milind.dev", "/outside"); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("routing table never picked up the external write")
}

func TestBulkCreate(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev", "go.test")
	mustCreate(t, a, Link{Host: "milind.dev", Path: "/taken", Target: "https://example.com"})
	ins, err := parseCSV(strings.NewReader("\ufeffSource,Destination,Code,Note,Tags\n" +
		"milind.dev/a,https://example.com/a,301,first,\"x, Y z\"\n" +
		"go.test/,https://example.com/slug\n" +
		"milind.dev/a,https://example.com/again\n" +
		"milind.dev/taken,https://example.com/t\n" +
		"milind.dev/bad path,https://example.com\n" +
		"milind.dev/b,ftp://nope\n" +
		"milind.dev/c,https://example.com/c,999\n"))
	if err != nil {
		t.Fatal(err)
	}
	rows, n, err := a.BulkCreate(context.Background(), ins, bulkOpts{DryRun: true, SkipProbe: true})
	if err != nil || n != 0 {
		t.Fatalf("dry run: n=%d err=%v", n, err)
	}
	want := []string{"ok", "ok", "duplicate", "duplicate", "invalid", "invalid", "invalid"}
	for i, r := range rows {
		if r.Result != want[i] {
			t.Errorf("row %d: %s (%s), want %s", r.Row, r.Result, r.Msg, want[i])
		}
	}
	if all, _ := a.store.Links(); len(all) != 1 {
		t.Fatalf("dry run created links: %d", len(all))
	}
	// Committing the preview's own rows keeps the generated slug.
	slug := rows[1].Input.Path
	again, err := parseCSV(strings.NewReader(rowsCSV(rows)))
	if err != nil {
		t.Fatal(err)
	}
	rows, n, err = a.BulkCreate(context.Background(), again, bulkOpts{SkipProbe: true})
	if err != nil || n != 2 {
		t.Fatalf("commit: n=%d err=%v", n, err)
	}
	if rows[1].Link == nil || rows[1].Link.Path != slug {
		t.Fatalf("slug changed between preview and commit: %v vs %s", rows[1].Link, slug)
	}
	l, _ := a.store.LinkByURL("milind.dev", "/a")
	if l.Status != 301 || l.Note != "first" || strings.Join(l.Tags, ",") != "x,y-z" {
		t.Fatalf("imported link: %+v", l)
	}
	if _, ok := a.table.Load().lookup("go.test", slug); !ok {
		t.Fatal("imported link not in the routing table")
	}

	// Bulk actions on a selection.
	all, _ := a.store.Links()
	var ids []int64
	for _, l := range all {
		ids = append(ids, l.ID)
	}
	if n, err := a.BulkAction(ids, "disable", ""); err != nil || n != 3 {
		t.Fatalf("disable: %d %v", n, err)
	}
	if w := get(a, "GET", "milind.dev", "/a", nil); w.Code != 410 {
		t.Fatalf("disabled link answered %d", w.Code)
	}
	if n, err := a.BulkAction(ids, "delete", ""); err != nil || n != 3 {
		t.Fatalf("delete: %d %v", n, err)
	}
}

func TestPrepareLink(t *testing.T) {
	a, _ := newTestApp(t)
	bad := []linkInput{
		{Host: "milind.dev", Path: "/x", Target: "https://milind.dev/x"}, // loops
		{Host: "milind.dev", Path: "*", Target: "https://milind.dev/y"},  // a /* link pointing at its own host loops too
		{Host: "links.test", Path: "/x", Target: "https://example.com"},  // the dashboard's own host
		{Host: "milind.dev", Path: "/", Target: "https://example.com"},   // the root
		{Host: "not a host", Path: "/x", Target: "https://example.com"},  // host
		{Host: "milind.dev", Path: "/x", Target: "javascript:alert(1)"},  // scheme
		{Host: "milind.dev", Path: "/x", Target: "https://e.com", Status: 303},
		{Host: "milind.dev", Path: "/x", Target: "https://e.com", Tags: []string{"no!"}},
	}
	for _, in := range bad {
		if _, err := a.prepareLink(in, nil); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	l, err := a.prepareLink(linkInput{Host: "https://Milind.dev/", Path: "", Target: " https://example.com "}, nil)
	if err != nil || l.Host != "milind.dev" || len(l.Path) != slugLen+1 || l.Target != "https://example.com" || l.Status != 302 || !l.KeepQuery {
		t.Fatalf("normalised link: %+v %v", l, err)
	}
}

func TestSlugAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		s := randomSlug()
		if len(s) != slugLen || strings.ContainsAny(s, "0O1lI") {
			t.Fatalf("bad slug %q", s)
		}
		seen[s] = true
	}
	if len(seen) < 1990 {
		t.Fatalf("only %d distinct slugs in 2000", len(seen))
	}
}

func TestParseUA(t *testing.T) {
	cases := []struct {
		ua                  string
		browser, os, device string
		bot                 bool
	}{
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 350.0.0.0", "Instagram", "iOS", "mobile", false},
		{"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36", "Chrome", "Android", "mobile", false},
		{"Mozilla/5.0 (Linux; Android 14; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", "Chrome", "Android", "tablet", false},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0", "Edge", "Windows", "desktop", false},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.6; rv:132.0) Gecko/20100101 Firefox/132.0", "Firefox", "macOS", "desktop", false},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15", "Safari", "macOS", "desktop", false},
		{"Mozilla/5.0 (iPad; CPU OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", "Safari", "iOS", "tablet", false},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "Bot", "Bot", "bot", true},
		{"WhatsApp/2.24.21.80 A", "Bot", "Bot", "bot", true},
		{"TelegramBot (like TwitterBot)", "Bot", "Bot", "bot", true},
		{"curl/8.10.1", "Bot", "Bot", "bot", true},
		{"", "Bot", "Bot", "bot", true},
	}
	for _, c := range cases {
		u := parseUA(c.ua)
		if u.Browser != c.browser || u.OS != c.os || u.Device != c.device || u.Bot != c.bot {
			t.Errorf("%q: got %+v", c.ua, u)
		}
	}
}

func TestRefHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://l.instagram.com/?u=x": "instagram.com", "https://www.google.com/": "google.com",
		"https://t.co/abc": "x.com", "https://lm.facebook.com/l.php": "facebook.com",
		"https://m.example.co": "example.co", "": "", "not a url": "",
	} {
		if got := refHost(in); got != want {
			t.Errorf("refHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatsBucketsAndTimeZone(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev")
	l := mustCreate(t, a, Link{Host: "milind.dev", Path: "/ade", Target: "https://example.com"})[0]
	// Monday 2026-11-02 23:30 UTC is Tuesday 05:00 in India (+5:30).
	at := time.Date(2026, 11, 2, 23, 30, 0, 0, time.UTC)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	prev := now.AddDate(0, 0, -10) // inside the previous 7-day period
	a.store.RecordClicks([]clickEvent{
		{l.ID, Click{At: at.Unix(), Visitor: "v1"}},
		{l.ID, Click{At: at.Unix() + 60, Visitor: "v1"}},
		{l.ID, Click{At: prev.Unix(), Visitor: "v2"}},
	})

	utc, err := a.store.Stats(statsQuery{LinkID: l.ID, Range: parseRange("7d"), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(utc.Series) != 7 || utc.Clicks != 2 || utc.Visitors != 1 || utc.PrevClicks != 1 {
		t.Fatalf("utc: %d buckets, %d clicks, %d visitors, %d prev", len(utc.Series), utc.Clicks, utc.Visitors, utc.PrevClicks)
	}
	if utc.Heat[0][23] != 2 {
		t.Fatalf("utc heatmap: Monday 23:00 = %d", utc.Heat[0][23])
	}
	ist, _ := a.store.Stats(statsQuery{LinkID: l.ID, Range: parseRange("7d"), Now: now, TZ: 330})
	if ist.Heat[1][5] != 2 {
		t.Fatalf("IST heatmap: Tuesday 05:00 = %d", ist.Heat[1][5])
	}
	// The last bucket is "today" in the viewer's zone and the series is zero-filled.
	total := 0
	for _, p := range ist.Series {
		total += p.N
	}
	if total != 2 || ist.Series[6].N != 2 {
		t.Fatalf("IST series: %+v", ist.Series)
	}
	if d, s := delta(2, 1); d != "+100%" || s != 1 {
		t.Fatalf("delta = %s %d", d, s)
	}
}

func TestTraefikConfigOwnedHost(t *testing.T) {
	links := []Link{
		{ID: 1, Host: "go.test", Path: "*"},
		{ID: 2, Host: "go.test", Path: "/x"},
		{ID: 3, Host: "milind.dev", Path: "/ade"},
	}
	out, err := Render(testConfig(), []Domain{{Host: "go.test"}, {Host: "milind.dev"}}, links)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "signpost-link-1:") || strings.Contains(s, "signpost-link-2:") || !strings.Contains(s, "signpost-link-3:") {
		t.Fatalf("routers:\n%s", s)
	}
}

func TestTraefikETag(t *testing.T) {
	a, _ := newTestApp(t)
	h := a.adminRoutes()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/traefik.yml", nil))
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" {
		t.Fatalf("first fetch: %d %q", w.Code, etag)
	}
	req := httptest.NewRequest("GET", "/traefik.yml", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("conditional fetch: %d, %d bytes", w.Code, w.Body.Len())
	}
}

func TestUIRequiresHeaderAndAuth(t *testing.T) {
	a, _ := newTestApp(t)
	a.cfg.TrustRemoteUser = true
	h := a.adminRoutes()
	for _, c := range []struct {
		user, hdr string
		code      int
	}{
		{"", "1", 401},      // nobody signed in
		{"milind", "", 403}, // no htmx header: a cross-site form
		{"milind", "1", 200},
	} {
		req := httptest.NewRequest("POST", "/ui/settings", strings.NewReader("default_host=milind.dev"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c.user != "" {
			req.Header.Set("Remote-User", c.user)
		}
		if c.hdr != "" {
			req.Header.Set("X-Signpost-UI", c.hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.code && !(c.code == 200 && w.Code == 204) {
			t.Errorf("user %q header %q: %d, want %d", c.user, c.hdr, w.Code, c.code)
		}
	}
	// Every page renders.
	mustCreate(t, a, Link{Host: "milind.dev", Path: "/ade", Target: "https://example.com"})
	// Link pages with an expiry, past and future, once broke the header.
	past, soon := time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()
	mustCreate(t, a, Link{Host: "milind.dev", Path: "/gone", Target: "https://example.com", ExpiresAt: &past})
	mustCreate(t, a, Link{Host: "milind.dev", Path: "/soon", Target: "https://example.com", ExpiresAt: &soon})
	for _, p := range []string{"/links", "/links/1", "/links/2", "/links/3", "/analytics", "/domains", "/import", "/settings", "/links?q=ade&state=live"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Remote-User", "milind")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "</html>") {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func BenchmarkRedirect(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	st, _ := OpenStore(path)
	defer st.Close()
	st.PutDomain(Domain{Host: "milind.dev"})
	a, _ := newApp(testConfig(), st)
	var ls []Link
	for i := range 5000 {
		ls = append(ls, Link{Host: "milind.dev", Path: "/l" + strings.Repeat("x", i%5) + time.Duration(i).String(), Target: "https://example.com", Status: 302, KeepQuery: true})
	}
	st.CreateLinks(ls, "")
	a.reload()
	go func() {
		for range a.clicks {
		}
	}()
	req := httptest.NewRequest("GET", ls[2500].Path+"?utm_source=x", nil)
	req.Host = "milind.dev"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		a.redirect(httptest.NewRecorder(), req)
	}
}

// Writing clicks one transaction at a time (how signpost used to) against
// the batches clickWriter now makes.
func BenchmarkClickWrites(b *testing.B) {
	for _, batch := range []int{1, 500} {
		b.Run("batch="+strconv.Itoa(batch), func(b *testing.B) {
			st, _ := OpenStore(filepath.Join(b.TempDir(), "bench.db"))
			defer st.Close()
			st.PutDomain(Domain{Host: "milind.dev"})
			l, _ := st.CreateLink(Link{Host: "milind.dev", Path: "/x", Target: "https://example.com", Status: 302}, "")
			evs := make([]clickEvent, batch)
			for i := range evs {
				evs[i] = clickEvent{l.ID, Click{At: time.Now().Unix(), Browser: "Chrome", Visitor: "v"}}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i += batch {
				st.RecordClicks(evs)
			}
		})
	}
}

// The install files are served without signpost's own auth (Authelia guards
// them at Traefik), from the root, with types browsers accept.
func TestPWAFiles(t *testing.T) {
	a, _ := newTestApp(t)
	h := a.adminRoutes()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}

	w := get("/manifest.webmanifest")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var m struct {
		StartURL    string `json:"start_url"`
		Scope       string `json:"scope"`
		ShareTarget struct {
			Action string `json:"action"`
		} `json:"share_target"`
		Icons []struct {
			Src     string `json:"src"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if m.StartURL != "/links" || m.Scope != "/" || m.ShareTarget.Action != "/links" {
		t.Fatalf("manifest: %+v", m)
	}
	// Every icon the manifest names must be served.
	for _, ic := range m.Icons {
		if c := get(ic.Src).Code; c != 200 {
			t.Errorf("icon %s: %d", ic.Src, c)
		}
	}
	if c := get("/static/offline.html").Code; c != 200 {
		t.Errorf("offline page: %d", c)
	}

	// sw.js is an esbuild output, absent until `make js` has run.
	if _, err := webFS.ReadFile("web/static/sw.js"); err != nil {
		t.Skip("web/static/sw.js not built")
	}
	w = get("/sw.js")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("sw.js: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
	}
}

func TestTitleCase(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"mobile":                         "Mobile",
		"answered 404 by something else": "Answered 404 by something else",
		"milind.dev/ade already exists":  "milind.dev/ade already exists",
		"https://x.com/a is taken":       "https://x.com/a is taken",
	} {
		if got := titleCase(in); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", in, got, want)
		}
	}
}
