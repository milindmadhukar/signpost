package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// whoFrom says who is asking: "token" for the CLI, the Authelia user for a
// browser, or "" for nobody. A request that carries Authorization is judged on
// the token alone, so the token-only Traefik router cannot be used to forge
// Remote-User.
func (a *App) whoFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if ok && a.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) == 1 {
			return "token"
		}
		return ""
	}
	if a.cfg.TrustRemoteUser {
		return r.Header.Get("Remote-User")
	}
	return ""
}

func (a *App) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.whoFrom(r) == "" {
			if r.Header.Get("Authorization") != "" {
				jsonErr(w, http.StatusUnauthorized, "bad token")
			} else if strings.HasPrefix(r.URL.Path, "/api/") {
				jsonErr(w, http.StatusUnauthorized, "sign in through Authelia or send a bearer token")
			} else {
				http.Error(w, "sign in through Authelia", http.StatusUnauthorized)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) adminRoutes() http.Handler {
	in := http.NewServeMux()
	// JSON API, for the CLI.
	in.HandleFunc("GET /api/links", a.apiLinks)
	in.HandleFunc("GET /api/links.csv", a.exportCSV)
	in.HandleFunc("POST /api/links", a.apiCreateLink)
	in.HandleFunc("POST /api/links/bulk", a.apiBulkCreate)
	in.HandleFunc("POST /api/links/bulk-action", a.apiBulkAction)
	in.HandleFunc("GET /api/links/{id}/stats", a.apiStats)
	in.HandleFunc("PATCH /api/links/{id}", a.apiUpdateLink)
	in.HandleFunc("DELETE /api/links/{id}", a.apiDeleteLink)
	in.HandleFunc("GET /api/links/{id}/clicks", a.apiClicks)
	in.HandleFunc("POST /api/links/{id}/check", a.apiRecheckLink)
	in.HandleFunc("GET /api/domains", a.apiDomains)
	in.HandleFunc("PUT /api/domains/{host}", a.apiPutDomain)
	in.HandleFunc("DELETE /api/domains/{host}", a.apiDeleteDomain)
	in.HandleFunc("POST /api/domains/{host}/check", a.apiRecheckDomain)
	// htmx dashboard.
	a.uiRoutes(in)

	mux := http.NewServeMux()
	mux.Handle("/", a.authed(in))
	// Unauthenticated on purpose: Traefik's HTTP provider polls this over the
	// docker network. It holds hostnames and paths, nothing secret. Targets
	// are not in it.
	// Traefik polls every few seconds; the ETag lets an unchanged config
	// answer 304 with no body.
	mux.HandleFunc("GET /traefik.yml", func(w http.ResponseWriter, r *http.Request) {
		t := a.table.Load()
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("ETag", t.etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == t.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(t.rendered)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	a.pwaRoutes(mux)
	static, _ := fs.Sub(webFS, "web/static")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fonts never change; app.css is cache-busted by ?v= per build.
		if strings.HasPrefix(r.URL.Path, "/static/fonts/") || r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	}))
	return mux
}

func apiFail(w http.ResponseWriter, err error) {
	var ce *conflictErr
	if errors.As(err, &ce) {
		jsonErr(w, http.StatusConflict, ce.msg+" (pass force to take it anyway)")
		return
	}
	jsonErr(w, errCode(err), err.Error())
}

func (a *App) apiLinks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	links, err := a.store.FindLinks(LinkFilter{Q: q.Get("q"), Host: q.Get("host"), State: q.Get("state"), Tag: q.Get("tag")})
	if err != nil {
		apiFail(w, err)
		return
	}
	writeJSON(w, links)
}

func (a *App) apiCreateLink(w http.ResponseWriter, r *http.Request) {
	var in linkInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	l, err := a.CreateLink(r.Context(), in)
	if err != nil {
		apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, l)
}

func (a *App) apiUpdateLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	var p linkPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	l, err := a.UpdateLink(id, p)
	if err != nil {
		apiFail(w, err)
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
	if err := a.DeleteLink(id); err != nil {
		apiFail(w, err)
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
		apiFail(w, err)
		return
	}
	writeJSON(w, c)
}

func (a *App) apiRecheckLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	if err := a.RecheckLink(id); err != nil {
		apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) apiDomains(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.Domains()
	if err != nil {
		apiFail(w, err)
		return
	}
	writeJSON(w, d)
}

func (a *App) apiPutDomain(w http.ResponseWriter, r *http.Request) {
	var in Domain
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	d, err := a.PutDomain(r.PathValue("host"), in.CertResolver, in.Fallback)
	if err != nil {
		apiFail(w, err)
		return
	}
	writeJSON(w, d)
}

func (a *App) apiRecheckDomain(w http.ResponseWriter, r *http.Request) {
	if err := a.RecheckDomain(normHost(r.PathValue("host"))); err != nil {
		apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) apiDeleteDomain(w http.ResponseWriter, r *http.Request) {
	if err := a.DeleteDomain(normHost(r.PathValue("host"))); err != nil {
		apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) exportCSV(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	links, err := a.store.FindLinks(LinkFilter{Q: q.Get("q"), Host: q.Get("host"), State: q.Get("state"), Tag: q.Get("tag")})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="signpost-links.csv"`)
	writeCSV(w, links)
}

type bulkRequest struct {
	bulkOpts
	Links []linkInput `json:"links"`
	CSV   string      `json:"csv"` // alternative to links
}

func (a *App) apiBulkCreate(w http.ResponseWriter, r *http.Request) {
	var req bulkRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	ins := req.Links
	if req.CSV != "" {
		var err error
		if ins, err = parseCSV(strings.NewReader(req.CSV)); err != nil {
			apiFail(w, err)
			return
		}
	}
	rows, n, err := a.BulkCreate(r.Context(), ins, req.bulkOpts)
	if err != nil {
		apiFail(w, err)
		return
	}
	writeJSON(w, map[string]any{"created": n, "dry_run": req.DryRun, "rows": rows})
}

func (a *App) apiBulkAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"`
		Arg    string  `json:"arg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, 400, "bad json")
		return
	}
	n, err := a.BulkAction(req.IDs, req.Action, req.Arg)
	if err != nil {
		apiFail(w, err)
		return
	}
	writeJSON(w, map[string]int{"changed": n})
}

func (a *App) apiStats(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		jsonErr(w, 400, "bad id")
		return
	}
	if _, err := a.store.Link(id); err != nil {
		apiFail(w, err)
		return
	}
	rg := parseRange(r.URL.Query().Get("range"))
	st, err := a.store.Stats(statsQuery{LinkID: id, Range: rg, Now: time.Now()})
	if err != nil {
		apiFail(w, err)
		return
	}
	writeJSON(w, st)
}
