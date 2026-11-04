package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The dashboard: Go templates rendered server-side, with htmx for the
// interactive bits. Every page is a real URL that renders whole, and hx-boost
// makes moving between them feel instant. Mutations live under /ui/ and
// answer with the fragment they changed plus a toast.

var version = strconv.FormatInt(time.Now().Unix(), 36) // cache-busts app.css per build start

var funcs = template.FuncMap{
	"icon": func(name string, class ...string) template.HTML {
		cls := "icon"
		if len(class) > 0 {
			cls += " " + strings.Join(class, " ")
		}
		return template.HTML(`<svg class="` + template.HTMLEscapeString(cls) + `" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + iconPaths[name] + `</svg>`)
	},
	"codes": func() []int { return []int{302, 301, 307, 308} },
	"codeName": func(c int) string {
		return map[int]string{301: "Permanent", 302: "Temporary", 307: "Temporary, keeps method", 308: "Permanent, keeps method"}[c]
	},
	"shown": func(p string) string {
		if p == "*" {
			return "/*"
		}
		return p
	},
	"ago": func(ts *int64) string {
		if ts == nil {
			return "Never"
		}
		return agoUnix(*ts)
	},
	"agoUnix": agoUnix,
	"when":    func(ts int64) string { return time.Unix(ts, 0).Format("Jan 2, 15:04") },
	"whenTZ": func(ts int64, tz int) string {
		return time.Unix(ts, 0).In(time.FixedZone("", tz*60)).Format("Jan 2, 15:04")
	},
	// lt cannot compare a *int64 (what {{with .ExpiresAt}} yields); a
	// function argument is dereferenced on the way in.
	"past": func(ts int64) bool { return ts < time.Now().Unix() },
	"date": func(ts int64) string { return time.Unix(ts, 0).Format("Jan 2, 2006") },
	"stateClass": func(s string) string {
		return map[string]string{"live": "b-ok", "pending": "b-wait", "dns": "b-warn", "error": "b-bad", "disabled": "b-off", "expired": "b-off"}[s]
	},
	"stateLabel": func(s string) string {
		return map[string]string{"live": "Live", "pending": "Checking", "dns": "DNS issue", "error": "Error", "disabled": "Disabled", "expired": "Expired"}[s]
	},
	"dnsClass": func(s string) string {
		return map[string]string{"ok": "b-ok", "proxied": "b-ok", "pending": "b-wait", "elsewhere": "b-warn", "missing": "b-bad", "error": "b-bad"}[s]
	},
	"dnsLabel": func(s string) string {
		return map[string]string{"ok": "Points here", "proxied": "Via Cloudflare", "pending": "Checking", "elsewhere": "Points elsewhere", "missing": "No record", "error": "Error"}[s]
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"thousands": func(n any) string {
		switch v := n.(type) {
		case int:
			return thousands(int64(v))
		case int64:
			return thousands(v)
		}
		return fmt.Sprint(n)
	},
	"compact": compact,
	"plural":  plural,
	"spark":   sparkline,
	"delta":   func(cur, prev int) string { s, _ := delta(cur, prev); return s },
	"deltaSign": func(cur, prev int) int {
		_, s := delta(cur, prev)
		return s
	},
	"pct": func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) + "%" },
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(s[:1])
	},
	"join":        strings.Join,
	"countryName": countryName,
	"worldMap":    makeWorldMap,
	"geoCredit":   func() bool { return geoCredit.Load() },
	"titleCase":   titleCase,
	"list":        func(s ...string) []string { return s },
	"add":         func(a, b int) int { return a + b },
	"localInput": func(ts *int64, tz int) string {
		if ts == nil {
			return ""
		}
		return time.Unix(*ts, 0).In(time.FixedZone("", tz*60)).Format("2006-01-02T15:04")
	},
	"hostOf": func(s string) string {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return u.Host
		}
		return s
	},
}

func agoUnix(ts int64) string {
	s := time.Since(time.Unix(ts, 0))
	switch {
	case s < time.Minute:
		return "just now"
	case s < time.Hour:
		return strconv.Itoa(int(s.Minutes())) + "m ago"
	case s < 24*time.Hour:
		return strconv.Itoa(int(s.Hours())) + "h ago"
	case s < 60*24*time.Hour:
		return strconv.Itoa(int(s.Hours()/24)) + "d ago"
	}
	return time.Unix(ts, 0).Format("Jan 2, 2006")
}

// Each page gets its own template set (layout + shared partials + the page),
// so every page can define "content" without clashing.
var pages = func() map[string]*template.Template {
	base := template.Must(template.New("").Funcs(funcs).ParseFS(webFS,
		"web/templates/layout.html", "web/templates/partials.html", "web/templates/charts.html"))
	out := map[string]*template.Template{}
	for _, name := range []string{"links", "link", "analytics", "domains", "import", "settings"} {
		t := template.Must(base.Clone())
		out[name] = template.Must(t.ParseFS(webFS, "web/templates/"+name+".html"))
	}
	return out
}()

type pageView struct {
	Title   string
	Nav     string
	User    string
	Version string
	TZ      int
	Data    any
}

func (a *App) renderPage(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	user := a.whoFrom(r)
	if user == "token" {
		user = ""
	}
	nav := name
	if name == "link" {
		nav = "links"
	}
	render(w, name, "layout", pageView{Title: title, Nav: nav, User: user, Version: version, TZ: tzOf(r), Data: data})
}

func render(w http.ResponseWriter, page, block string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages[page].ExecuteTemplate(w, block, data); err != nil {
		log.Printf("template %s/%s: %v", page, block, err)
	}
}

// toast asks the page to show a notice once htmx has swapped the response.
func toast(w http.ResponseWriter, kind, msg string) {
	b, _ := json.Marshal(map[string]any{"toast": map[string]string{"kind": kind, "msg": msg}})
	w.Header().Set("HX-Trigger", string(b))
}

func toastErr(w http.ResponseWriter, err error) { toast(w, "err", err.Error()) }

// tzOf reads the viewer's UTC offset (minutes east), which layout.html keeps
// in a cookie, so charts bucket by the viewer's days and hours.
func tzOf(r *http.Request) int {
	c, err := r.Cookie("tz")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(c.Value)
	if err != nil || n < -840 || n > 840 {
		return 0
	}
	return n
}

// currentQuery recovers the page's own query string for a fragment request:
// the links filter, the stats range. htmx sends the page URL along.
func currentQuery(r *http.Request) url.Values {
	if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil {
		return u.Query()
	}
	return url.Values{}
}

func currentPath(r *http.Request) string {
	if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil {
		return u.Path
	}
	return ""
}

func (a *App) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/links", http.StatusFound) })
	mux.HandleFunc("GET /links", a.pageLinks)
	mux.HandleFunc("GET /links/{id}", a.pageLink)
	mux.HandleFunc("GET /analytics", a.pageAnalytics)
	mux.HandleFunc("GET /domains", a.pageDomains)
	mux.HandleFunc("GET /import", a.pageImport)
	mux.HandleFunc("GET /settings", a.pageSettings)
	mux.HandleFunc("GET /export.csv", a.exportCSV)

	ui := http.NewServeMux()
	ui.HandleFunc("POST /ui/links", a.uiCreateLink)
	ui.HandleFunc("POST /ui/links/{id}", a.uiSaveLink)
	ui.HandleFunc("POST /ui/links/{id}/password", a.uiPassword)
	ui.HandleFunc("POST /ui/links/{id}/toggle", a.uiToggle)
	ui.HandleFunc("POST /ui/links/{id}/check", a.uiRecheck)
	ui.HandleFunc("DELETE /ui/links/{id}", a.uiDeleteLink)
	ui.HandleFunc("POST /ui/bulk", a.uiBulk)
	ui.HandleFunc("POST /ui/domains", a.uiConnectDomain)
	ui.HandleFunc("PUT /ui/domains/{host}", a.uiSaveDomain)
	ui.HandleFunc("POST /ui/domains/{host}/check", a.uiDomainAction(a.RecheckDomain, "Checking DNS for %s"))
	ui.HandleFunc("DELETE /ui/domains/{host}", a.uiDomainAction(a.DeleteDomain, "Removed %s"))
	ui.HandleFunc("POST /ui/import", a.uiImport)
	ui.HandleFunc("POST /ui/settings", a.uiSettings)
	// htmx sends X-Signpost-UI on every request (hx-headers on <body>). A
	// cross-site form cannot set a custom header, so requiring it is the CSRF
	// guard for these cookie-authenticated routes. Page GETs above change
	// nothing, so they need no guard.
	mux.Handle("/ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Signpost-UI") != "1" {
			http.Error(w, "htmx only", http.StatusForbidden)
			return
		}
		ui.ServeHTTP(w, r)
	}))
}

// ---- Links list ----

type formView struct {
	Host, Path, Target, Note, Tags, Expires string
	HostOther                               bool // Host is typed in, not one of Hosts
	Status                                  int
	KeepQuery                               bool
	Msg                                     string
	Conflict                                bool
	Hosts                                   []string
}

type linkRow struct {
	Link
	State string
	Spark template.HTML
	Week  int
}

type linksView struct {
	Filter  LinkFilter
	Rows    []linkRow
	Total   int
	Domains []Domain
	Tags    []string
	Polling bool
	Self    string // this list's URL, for polling
	Form    formView
	Err     string
	OOB     bool // swapped in out of band, behind the new-link drawer
}

func filterFrom(q url.Values) LinkFilter {
	return LinkFilter{Q: strings.TrimSpace(q.Get("q")), Host: q.Get("host"), State: q.Get("state"), Tag: q.Get("tag")}
}

func (f LinkFilter) Query() string {
	v := url.Values{}
	for k, s := range map[string]string{"q": f.Q, "host": f.Host, "state": f.State, "tag": f.Tag} {
		if s != "" {
			v.Set(k, s)
		}
	}
	return v.Encode()
}

func (f LinkFilter) Active() bool { return f.Q != "" || f.Host != "" || f.State != "" || f.Tag != "" }

func (a *App) linksView(r *http.Request, f LinkFilter) linksView {
	v := linksView{Filter: f, Self: "/links"}
	if q := f.Query(); q != "" {
		v.Self += "?" + q
	}
	links, err := a.store.FindLinks(f)
	if err != nil {
		v.Err = err.Error()
	}
	var total int
	a.store.db.QueryRow(`SELECT COUNT(*) FROM links`).Scan(&total)
	v.Total = total
	sparks, _ := a.store.Sparklines(7, time.Now(), tzOf(r))
	for _, l := range links {
		row := linkRow{Link: l, State: l.State(), Spark: sparkline(sparks[l.ID])}
		for _, n := range sparks[l.ID] {
			row.Week += n
		}
		v.Rows = append(v.Rows, row)
		v.Polling = v.Polling || l.Check == "pending"
	}
	v.Domains, _ = a.store.Domains()
	v.Tags = a.store.Tags()
	return v
}

func (a *App) newForm() formView {
	f := formView{Host: a.defaultHost(), Status: 302, KeepQuery: true}
	if ds, err := a.store.Domains(); err == nil {
		for _, d := range ds {
			f.Hosts = append(f.Hosts, d.Host)
		}
	}
	return f
}

func (a *App) pageLinks(w http.ResponseWriter, r *http.Request) {
	v := a.linksView(r, filterFrom(r.URL.Query()))
	v.Form = a.newForm()
	a.renderPage(w, r, "links", "Links", v)
}

// renderTable answers a mutation made from the links list with the list as
// the page currently filters it.
func (a *App) renderTable(w http.ResponseWriter, r *http.Request) {
	render(w, "links", "links-table", a.linksView(r, filterFrom(currentQuery(r))))
}

func parseLocal(s string, tz int) (*int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.FixedZone("", tz*60))
	if err != nil {
		return nil, fail(400, "expiry must be a date and time")
	}
	u := t.Unix()
	return &u, nil
}

func (a *App) uiCreateLink(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	status, _ := strconv.Atoi(r.FormValue("status"))
	keep := r.FormValue("keep_query") == "1"
	host := r.FormValue("host")
	if host == "__other__" {
		host = r.FormValue("host_other")
	}
	fv := formView{
		Host: host, Path: r.FormValue("path"), Target: r.FormValue("target"), Note: r.FormValue("note"),
		Tags: r.FormValue("tags"), Expires: r.FormValue("expires_at"), Status: status, KeepQuery: keep,
	}
	fv.Hosts = a.newForm().Hosts
	fv.HostOther = !contains(fv.Hosts, normHost(fv.Host))
	exp, err := parseLocal(fv.Expires, tzOf(r))
	if err == nil && exp != nil && *exp <= time.Now().Unix() {
		err = fail(400, "expiry must be in the future")
	}
	var l Link
	if err == nil {
		l, err = a.CreateLink(r.Context(), linkInput{
			Host: fv.Host, Path: fv.Path, Target: fv.Target, Status: status, KeepQuery: &keep, Note: fv.Note,
			Tags: []string{fv.Tags}, Password: r.FormValue("password"), ExpiresAt: exp, Force: r.FormValue("force") == "1",
		})
	}
	if err != nil {
		var ce *conflictErr
		fv.Conflict = errors.As(err, &ce)
		fv.Msg = err.Error()
		render(w, "links", "linkform", fv)
		return
	}
	// A fresh form for the next link, and the list re-rendered behind the
	// drawer. The drawer closes on the "link-created" event.
	b, _ := json.Marshal(map[string]any{
		"toast":        map[string]string{"kind": "ok", "msg": "Created " + l.URL() + ". Traefik picks it up within a few seconds."},
		"link-created": map[string]string{"url": l.Href()},
	})
	w.Header().Set("HX-Trigger", string(b))
	nf := a.newForm()
	nf.Host = l.Host
	render(w, "links", "linkform", nf)
	if currentPath(r) == "/links" {
		v := a.linksView(r, filterFrom(currentQuery(r)))
		v.OOB = true
		render(w, "links", "links-table", v)
	}
}

func (a *App) uiToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	l, err := a.store.Link(id)
	if err == nil {
		off := !l.Disabled
		_, err = a.UpdateLink(id, linkPatch{Disabled: &off})
		if err == nil {
			if off {
				toast(w, "ok", "Disabled "+l.URL()+". It now answers 410 Gone.")
			} else {
				toast(w, "ok", "Enabled "+l.URL())
			}
		}
	}
	if err != nil {
		toastErr(w, err)
	}
	a.afterLinkChange(w, r, id)
}

func (a *App) uiRecheck(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	if err := a.RecheckLink(id); err != nil {
		toastErr(w, err)
	} else {
		toast(w, "ok", "Checking the link again")
	}
	a.afterLinkChange(w, r, id)
}

// afterLinkChange re-renders whichever view the change came from: the list,
// or a link's own page.
func (a *App) afterLinkChange(w http.ResponseWriter, r *http.Request, id int64) {
	if strings.HasPrefix(currentPath(r), "/links/") {
		w.Header().Set("HX-Refresh", "true")
		return
	}
	a.renderTable(w, r)
}

func (a *App) uiDeleteLink(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	l, _ := a.store.Link(id)
	if err := a.DeleteLink(id); err != nil {
		toastErr(w, err)
		a.renderTable(w, r)
		return
	}
	if strings.HasPrefix(currentPath(r), "/links/") {
		w.Header().Set("HX-Redirect", "/links")
		return
	}
	toast(w, "ok", "Deleted "+l.URL())
	a.renderTable(w, r)
}

func (a *App) uiBulk(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var ids []int64
	for _, s := range r.Form["ids"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	action := r.FormValue("action")
	n, err := a.BulkAction(ids, action, r.FormValue("arg"))
	if err != nil {
		toastErr(w, err)
	} else {
		verb := map[string]string{"delete": "Deleted", "enable": "Enabled", "disable": "Disabled", "status": "Updated", "tag": "Tagged", "untag": "Untagged"}[action]
		toast(w, "ok", fmt.Sprintf("%s %s", verb, plural(n, "link")))
	}
	a.renderTable(w, r)
}

// ---- One link ----

type linkView struct {
	TZ     int
	Link   Link
	State  string
	Stats  *Stats
	Chart  barChart
	Heat   []heatRow
	Lists  map[string][]barListRow
	Ranges []statRange
	Msg    string
}

func (a *App) statsBundle(r *http.Request, id int64) (*Stats, barChart, []heatRow, map[string][]barListRow, error) {
	rg := parseRange(r.URL.Query().Get("range"))
	st, err := a.store.Stats(statsQuery{LinkID: id, Range: rg, Now: time.Now(), TZ: tzOf(r)})
	if err != nil {
		return nil, barChart{}, nil, nil, err
	}
	lists := map[string][]barListRow{
		"Referrers": makeBarList(st.Referrers, st.Clicks, nil),
		"Countries": makeBarList(st.Countries, st.Clicks, nil),
		"Devices":   makeBarList(st.Devices, st.Clicks, nil),
		"Browsers":  makeBarList(st.Browsers, st.Clicks, nil),
		"OS":        makeBarList(st.OSes, st.Clicks, nil),
		"Links":     makeBarList(st.Links, st.Clicks, func(s statRow) string { return "/links/" + strconv.FormatInt(s.ID, 10) }),
	}
	return st, makeBarChart(st), makeHeatmap(st), lists, nil
}

func (a *App) pageLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	l, lerr := a.store.Link(id)
	if err != nil || lerr != nil {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	st, chart, heat, lists, err := a.statsBundle(r, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.renderPage(w, r, "link", l.URL(), linkView{TZ: tzOf(r), Link: l, State: l.State(), Stats: st, Chart: chart, Heat: heat, Lists: lists, Ranges: statRanges})
}

func (a *App) uiSaveLink(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	r.ParseForm()
	target := r.FormValue("target")
	status, _ := strconv.Atoi(r.FormValue("status"))
	keep := r.FormValue("keep_query") == "1"
	note := r.FormValue("note")
	tags := []string{r.FormValue("tags")}
	disabled := r.FormValue("enabled") != "1"
	p := linkPatch{Target: &target, Status: &status, KeepQuery: &keep, Note: &note, Tags: &tags, Disabled: &disabled}
	exp, err := parseLocal(r.FormValue("expires_at"), tzOf(r))
	if err == nil {
		zero := int64(0)
		if exp == nil {
			exp = &zero
		}
		p.ExpiresAt = exp
		_, err = a.UpdateLink(id, p)
	}
	l, lerr := a.store.Link(id)
	if lerr != nil {
		toastErr(w, lerr)
		return
	}
	v := linkView{TZ: tzOf(r), Link: l, State: l.State()}
	if err != nil {
		v.Msg = err.Error()
		// Keep what was typed, so a typo is not lost.
		v.Link.Target, v.Link.Note = target, note
	} else {
		toast(w, "ok", "Saved")
	}
	render(w, "link", "settings-card", pageView{TZ: tzOf(r), Data: v})
	render(w, "link", "link-header-oob", pageView{TZ: tzOf(r), Data: v})
}

func (a *App) uiPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	r.ParseForm()
	pw := r.FormValue("password")
	if r.FormValue("remove") == "1" {
		pw = ""
	} else if len(pw) < 4 {
		toast(w, "err", "Use at least 4 characters")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := a.UpdateLink(id, linkPatch{Password: &pw}); err != nil {
		toastErr(w, err)
	} else if pw == "" {
		toast(w, "ok", "Password removed. The link redirects straight away again.")
	} else {
		toast(w, "ok", "Password set. Visitors are asked for it before the redirect.")
	}
	l, err := a.store.Link(id)
	if err != nil {
		return
	}
	v := linkView{TZ: tzOf(r), Link: l, State: l.State()}
	render(w, "link", "password-card", pageView{Data: v})
	render(w, "link", "link-header-oob", pageView{TZ: tzOf(r), Data: v})
}

// ---- Analytics ----

type analyticsView struct {
	Stats    *Stats
	Chart    barChart
	Heat     []heatRow
	Lists    map[string][]barListRow
	Ranges   []statRange
	Links    int
	Selected *Link  // nil: all links
	Options  []Link // for the link picker
	PerLink  []perLinkRow
	LinkID   int64 // kept in the range picker's links; 0 for all links
	Active   int   // links with at least one click in the range
}

type perLinkRow struct {
	linkStat
	Spark template.HTML
	Delta string
	Sign  int
	Share float64 // percent of all clicks in the range
	Bar   float64 // bar width, percent of the busiest link, so small shares stay visible
}

func (a *App) pageAnalytics(w http.ResponseWriter, r *http.Request) {
	v := analyticsView{Ranges: statRanges}
	v.Options, _ = a.store.Links()
	v.Links = len(v.Options)
	var id int64
	if s := r.URL.Query().Get("link"); s != "" {
		id, _ = strconv.ParseInt(s, 10, 64)
		if l, err := a.store.Link(id); err == nil {
			v.Selected = &l
			v.LinkID = id
		} else {
			id = 0
		}
	}
	st, chart, heat, lists, err := a.statsBundle(r, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v.Stats, v.Chart, v.Heat, v.Lists = st, chart, heat, lists
	if id == 0 {
		rows, err := a.store.PerLink(statsQuery{Range: st.Range, Now: time.Now(), TZ: tzOf(r)})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		top := 1
		for _, ls := range rows {
			top = max(top, ls.Clicks)
		}
		for _, ls := range rows {
			row := perLinkRow{linkStat: ls, Spark: sparkline(ls.Series), Bar: float64(ls.Clicks) / float64(top) * 100}
			row.Delta, row.Sign = delta(ls.Clicks, ls.PrevClicks)
			if st.Clicks > 0 {
				row.Share = float64(ls.Clicks) / float64(st.Clicks) * 100
			}
			if ls.Clicks > 0 {
				v.Active++
			}
			v.PerLink = append(v.PerLink, row)
		}
	}
	title := "Analytics"
	if v.Selected != nil {
		title = "Analytics · " + v.Selected.URL()
	}
	a.renderPage(w, r, "analytics", title, v)
}

// ---- Domains ----

type domainsView struct {
	Domains []Domain
	Polling bool
}

func (a *App) domainsView() domainsView {
	v := domainsView{}
	v.Domains, _ = a.store.Domains()
	for _, d := range v.Domains {
		v.Polling = v.Polling || d.DNS == "pending"
	}
	return v
}

func (a *App) pageDomains(w http.ResponseWriter, r *http.Request) {
	a.renderPage(w, r, "domains", "Domains", a.domainsView())
}

func (a *App) uiDomainAction(op func(string) error, okMsg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := normHost(r.PathValue("host"))
		if err := op(host); err != nil {
			toastErr(w, err)
		} else {
			toast(w, "ok", fmt.Sprintf(okMsg, host))
		}
		render(w, "domains", "domains-table", a.domainsView())
	}
}

func (a *App) uiConnectDomain(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	host := normHost(r.FormValue("host"))
	// Connecting an already-known host must not wipe its fallback.
	resolver, fallback := "", ""
	if d, err := a.store.Domain(host); err == nil {
		resolver, fallback = d.CertResolver, d.Fallback
	}
	if _, err := a.PutDomain(host, resolver, fallback); err != nil {
		toastErr(w, err)
	} else {
		toast(w, "ok", "Connected "+host+". Checking its DNS now.")
	}
	render(w, "domains", "domains-table", a.domainsView())
}

func (a *App) uiSaveDomain(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	host := r.PathValue("host")
	if _, err := a.PutDomain(host, r.FormValue("cert_resolver"), r.FormValue("fallback")); err != nil {
		toastErr(w, err)
	} else {
		toast(w, "ok", "Saved "+normHost(host))
	}
	render(w, "domains", "domains-table", a.domainsView())
}

// ---- Import ----

type importView struct {
	CSV       string
	CommitCSV string // the previewed rows with generated slugs filled in, so the commit creates exactly what was shown
	Rows      []bulkRow
	Counts    map[string]int
	Committed bool
	Created   int
	SkipProbe bool
	Force     bool
	Err       string
	Hosts     []string
}

func (a *App) pageImport(w http.ResponseWriter, r *http.Request) {
	a.renderPage(w, r, "import", "Import", importView{Hosts: a.newForm().Hosts})
}

func (a *App) uiImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		toastErr(w, fail(400, "upload too large or unreadable"))
		return
	}
	v := importView{CSV: r.FormValue("csv"), SkipProbe: r.FormValue("skip_probe") == "1", Force: r.FormValue("force") == "1"}
	if f, _, err := r.FormFile("file"); err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		if len(strings.TrimSpace(string(b))) > 0 {
			v.CSV = string(b)
		}
	}
	commit := r.FormValue("mode") == "commit"
	ins, err := parseCSV(strings.NewReader(v.CSV))
	if err == nil {
		var n int
		v.Rows, n, err = a.BulkCreate(r.Context(), ins, bulkOpts{DryRun: !commit, Force: v.Force, SkipProbe: v.SkipProbe})
		v.Committed, v.Created = commit, n
	}
	if err != nil {
		v.Err = err.Error()
	}
	if !commit {
		v.CommitCSV = rowsCSV(v.Rows)
	}
	v.Counts = map[string]int{}
	for _, row := range v.Rows {
		v.Counts[row.Result]++
	}
	if commit && err == nil {
		toast(w, "ok", "Imported "+plural(v.Created, "link"))
	}
	render(w, "import", "import-result", v)
}

// ---- Settings ----

type settingsView struct {
	DefaultHost string
	Hosts       []string
	Routes      int
	Built       time.Time
	Dropped     int64
	Queue       int
	AdminHost   string
	TraefikFile string
	Links       int
	Clicks      int
}

func (a *App) pageSettings(w http.ResponseWriter, r *http.Request) {
	t := a.table.Load()
	v := settingsView{
		DefaultHost: a.store.Setting("default_host"), Hosts: a.newForm().Hosts,
		Routes: len(t.links), Built: t.built, Queue: len(a.clicks), Dropped: a.dropped.Load(),
		AdminHost: a.cfg.AdminHost, TraefikFile: a.cfg.TraefikFile,
	}
	a.store.db.QueryRow(`SELECT COUNT(*) FROM links`).Scan(&v.Links)
	a.store.db.QueryRow(`SELECT COUNT(*) FROM clicks`).Scan(&v.Clicks)
	a.renderPage(w, r, "settings", "Settings", v)
}

func (a *App) uiSettings(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if err := a.SetDefaultHost(r.FormValue("default_host")); err != nil {
		toastErr(w, err)
	} else {
		toast(w, "ok", "Settings saved")
	}
	w.WriteHeader(http.StatusNoContent)
}
