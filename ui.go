package main

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// The dashboard: Go templates rendered server-side, swapped in by htmx. Every
// action answers with the HTML fragment it changed, so there is no client
// state to keep in sync.

var version = strconv.FormatInt(time.Now().Unix(), 36) // cache-busts app.css per build start

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"codes": func() []int { return []int{302, 301, 307, 308} },
	"shown": func(p string) string {
		if p == "*" {
			return "/*"
		}
		return p
	},
	"ago": func(ts *int64) string {
		if ts == nil {
			return "never"
		}
		s := time.Since(time.Unix(*ts, 0))
		switch {
		case s < time.Minute:
			return "just now"
		case s < time.Hour:
			return strconv.Itoa(int(s.Minutes())) + "m ago"
		case s < 24*time.Hour:
			return strconv.Itoa(int(s.Hours())) + "h ago"
		}
		return strconv.Itoa(int(s.Hours()/24)) + "d ago"
	},
	"when": func(ts int64) string { return time.Unix(ts, 0).Format("Jan 02, 15:04") },
	"checkClass": func(s string) string {
		return map[string]string{"live": "b-ok", "pending": "b-wait", "dns": "b-warn", "error": "b-bad"}[s]
	},
	"dnsClass": func(s string) string {
		return map[string]string{"ok": "b-ok", "proxied": "b-ok", "pending": "b-wait", "elsewhere": "b-warn", "missing": "b-bad", "error": "b-bad"}[s]
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}).ParseFS(webFS, "web/templates/*.html"))

type formView struct {
	Host, Path, Target, Note string
	Status                   int
	KeepQuery                bool
	Msg                      string
	Ok, Conflict             bool
}

type listsView struct {
	Links   []Link
	Domains []Domain
	Err     string
	Polling bool // keep refreshing while any check is still running
	OOB     bool
}

func (a *App) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.uiPage)
	ui := http.NewServeMux()
	ui.HandleFunc("GET /ui/lists", func(w http.ResponseWriter, r *http.Request) { a.renderLists(w, "") })
	ui.HandleFunc("POST /ui/links", a.uiCreateLink)
	ui.HandleFunc("GET /ui/links/{id}/edit", a.uiTargetFragment("target-edit"))
	ui.HandleFunc("GET /ui/links/{id}/target", a.uiTargetFragment("target"))
	ui.HandleFunc("PATCH /ui/links/{id}", a.uiUpdateLink)
	ui.HandleFunc("DELETE /ui/links/{id}", a.uiLinkAction(a.DeleteLink))
	ui.HandleFunc("POST /ui/links/{id}/check", a.uiLinkAction(a.RecheckLink))
	ui.HandleFunc("GET /ui/links/{id}/stats", a.uiStats)
	ui.HandleFunc("POST /ui/domains", a.uiConnectDomain)
	ui.HandleFunc("PUT /ui/domains/{host}", a.uiSaveDomain)
	ui.HandleFunc("POST /ui/domains/{host}/check", a.uiDomainAction(a.RecheckDomain))
	ui.HandleFunc("DELETE /ui/domains/{host}", a.uiDomainAction(a.DeleteDomain))
	// htmx sends X-Signpost-UI on every request (hx-headers on <body>). A
	// cross-site form cannot set a custom header, so requiring it is the CSRF
	// guard for these cookie-authenticated routes.
	mux.Handle("/ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Signpost-UI") != "1" {
			http.Error(w, "htmx only", http.StatusForbidden)
			return
		}
		ui.ServeHTTP(w, r)
	}))
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

func (a *App) lists(errMsg string) listsView {
	v := listsView{Err: errMsg}
	var err error
	if v.Links, err = a.store.Links(); err != nil {
		v.Err = err.Error()
	}
	if v.Domains, err = a.store.Domains(); err != nil {
		v.Err = err.Error()
	}
	for _, l := range v.Links {
		v.Polling = v.Polling || l.Check == "pending"
	}
	for _, d := range v.Domains {
		v.Polling = v.Polling || d.DNS == "pending"
	}
	return v
}

func (a *App) renderLists(w http.ResponseWriter, errMsg string) {
	render(w, "lists", a.lists(errMsg))
}

func (a *App) uiPage(w http.ResponseWriter, r *http.Request) {
	user := a.whoFrom(r)
	if user == "token" {
		user = ""
	}
	render(w, "page", map[string]any{
		"User":    user,
		"Version": version,
		"Form":    formView{Status: 302, KeepQuery: true},
		"Lists":   a.lists(""),
	})
}

func (a *App) uiCreateLink(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	status, _ := strconv.Atoi(r.FormValue("status"))
	keep := r.FormValue("keep_query") == "1"
	in := linkInput{
		Host: r.FormValue("host"), Path: r.FormValue("path"), Target: r.FormValue("target"),
		Status: status, KeepQuery: &keep, Note: r.FormValue("note"), Force: r.FormValue("force") == "1",
	}
	fv := formView{Host: in.Host, Path: in.Path, Target: in.Target, Note: in.Note, Status: status, KeepQuery: keep}
	l, err := a.CreateLink(r.Context(), in)
	if err != nil {
		var ce *conflictErr
		fv.Conflict = errors.As(err, &ce)
		fv.Msg = err.Error()
		render(w, "linkform", fv)
		return
	}
	// Fresh form keeps the host, since the next link usually shares it.
	render(w, "linkform", formView{
		Host: l.Host, Status: 302, KeepQuery: true, Ok: true,
		Msg: "added https://" + l.URL() + ". traefik picks it up within a few seconds; status flips to live once it answers.",
	})
	v := a.lists("")
	v.OOB = true
	render(w, "lists", v)
}

func (a *App) uiTargetFragment(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		l, err := a.store.Link(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		render(w, name, l)
	}
}

func (a *App) uiUpdateLink(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	r.ParseForm()
	p := linkPatch{}
	if t := r.FormValue("target"); t != "" {
		p.Target = &t
	}
	if s, err := strconv.Atoi(r.FormValue("status")); err == nil {
		p.Status = &s
	}
	errMsg := ""
	if _, err := a.UpdateLink(id, p); err != nil {
		errMsg = err.Error()
	}
	a.renderLists(w, errMsg)
}

func (a *App) uiLinkAction(op func(int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		errMsg := ""
		if err := op(id); err != nil {
			errMsg = err.Error()
		}
		a.renderLists(w, errMsg)
	}
}

func (a *App) uiDomainAction(op func(string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		errMsg := ""
		if err := op(normHost(r.PathValue("host"))); err != nil {
			errMsg = err.Error()
		}
		a.renderLists(w, errMsg)
	}
}

func (a *App) uiConnectDomain(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	host := normHost(r.FormValue("host"))
	errMsg := ""
	// Connecting an already-known host must not wipe its fallback.
	resolver, fallback := "", ""
	if d, err := a.store.Domain(host); err == nil {
		resolver, fallback = d.CertResolver, d.Fallback
	}
	if _, err := a.PutDomain(host, resolver, fallback); err != nil {
		errMsg = err.Error()
	}
	a.renderLists(w, errMsg)
}

func (a *App) uiSaveDomain(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	errMsg := ""
	if _, err := a.PutDomain(r.PathValue("host"), r.FormValue("cert_resolver"), r.FormValue("fallback")); err != nil {
		errMsg = err.Error()
	}
	a.renderLists(w, errMsg)
}

type bar struct {
	Key   string
	N     int
	Width int
}

func tally(cs []Click, key func(Click) string) []bar {
	m := map[string]int{}
	for _, c := range cs {
		m[key(c)]++
	}
	out := make([]bar, 0, len(m))
	max := 1
	for k, n := range m {
		out = append(out, bar{Key: k, N: n})
		if n > max {
			max = n
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N || (out[i].N == out[j].N && out[i].Key < out[j].Key) })
	if len(out) > 8 {
		out = out[:8]
	}
	for i := range out {
		out[i].Width = out[i].N * 120 / max
	}
	return out
}

func (a *App) uiStats(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	l, err := a.store.Link(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cs, _ := a.store.Clicks(id, 500)
	refs := tally(cs, func(c Click) string {
		if u, err := url.Parse(c.Referrer); err == nil && u.Host != "" {
			return u.Host
		}
		return "direct"
	})
	countries := tally(cs, func(c Click) string {
		if c.Country == "" {
			return "unknown"
		}
		return c.Country
	})
	if len(cs) > 50 {
		cs = cs[:50]
	}
	render(w, "stats", map[string]any{"Link": l, "Clicks": cs, "Referrers": refs, "Countries": countries})
}
