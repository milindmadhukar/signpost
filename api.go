package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
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
	in.HandleFunc("POST /api/links", a.apiCreateLink)
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
	mux.HandleFunc("GET /traefik.yml", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		body := a.rendered
		a.mu.RUnlock()
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(body)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
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
	links, err := a.store.Links()
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
