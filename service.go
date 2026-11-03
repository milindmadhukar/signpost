package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// The operations below are shared by the JSON API (the CLI) and the htmx UI,
// so both get exactly the same validation and side effects.

// opErr carries the HTTP status an operation failure maps to.
type opErr struct {
	code int
	msg  string
}

func (e *opErr) Error() string { return e.msg }

func fail(code int, msg string) error { return &opErr{code, msg} }

func errCode(err error) int {
	var oe *opErr
	if errors.As(err, &oe) {
		return oe.code
	}
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// isConflict marks a 409 that force can override, as opposed to a duplicate.
type conflictErr struct{ msg string }

func (e *conflictErr) Error() string { return e.msg }

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
		return fail(400, "target must be an absolute http(s) URL")
	}
	return nil
}

func validStatus(s int) bool { return s == 301 || s == 302 || s == 307 || s == 308 }

// normHost accepts what people paste: a scheme, a path or a trailing dot.
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	return strings.TrimSuffix(h, ".")
}

func (a *App) CreateLink(ctx context.Context, in linkInput) (Link, error) {
	l := Link{
		Host:   normHost(in.Host),
		Path:   strings.TrimSpace(in.Path),
		Target: strings.TrimSpace(in.Target),
		Status: in.Status,
		Note:   strings.TrimSpace(in.Note),
	}
	if l.Path == "/*" || l.Path == "*" {
		l.Path = "*"
	} else {
		if !strings.HasPrefix(l.Path, "/") {
			l.Path = "/" + l.Path
		}
		if len(l.Path) > 1 {
			l.Path = strings.TrimSuffix(l.Path, "/")
		}
	}
	l.KeepQuery = in.KeepQuery == nil || *in.KeepQuery
	if l.Status == 0 {
		l.Status = 302
	}
	switch {
	case !hostRe.MatchString(l.Host):
		return l, fail(400, "host must be a plain hostname like milind.dev")
	case l.Host == a.cfg.AdminHost:
		return l, fail(400, "that host is signpost's own dashboard")
	case l.Path != "*" && (l.Path == "/" || !pathRe.MatchString(l.Path)):
		return l, fail(400, "path may use letters, digits and . _ ~ - / only, and cannot be the root")
	case !validStatus(l.Status):
		return l, fail(400, "status must be 301, 302, 307 or 308")
	}
	if err := validTarget(l.Target); err != nil {
		return l, err
	}
	if !in.Force {
		if msg := a.conflict(ctx, l); msg != "" {
			return l, &conflictErr{msg}
		}
	}
	newHost := !a.store.HasDomain(l.Host)
	l, err := a.store.CreateLink(l, a.cfg.CertResolver)
	if err != nil {
		return l, fail(409, err.Error())
	}
	if err := a.reload(); err != nil {
		return l, err
	}
	go a.verify(l.ID)
	if newHost {
		go a.checkDomain(l.Host)
	}
	return l, nil
}

type linkPatch struct {
	Target    *string `json:"target"`
	Status    *int    `json:"status"`
	KeepQuery *bool   `json:"keep_query"`
	Note      *string `json:"note"`
}

func (a *App) UpdateLink(id int64, p linkPatch) (Link, error) {
	l, err := a.store.Link(id)
	if err != nil {
		return l, err
	}
	if p.Target != nil {
		if err := validTarget(strings.TrimSpace(*p.Target)); err != nil {
			return l, err
		}
		l.Target = strings.TrimSpace(*p.Target)
	}
	if p.Status != nil {
		if !validStatus(*p.Status) {
			return l, fail(400, "status must be 301, 302, 307 or 308")
		}
		l.Status = *p.Status
	}
	if p.KeepQuery != nil {
		l.KeepQuery = *p.KeepQuery
	}
	if p.Note != nil {
		l.Note = strings.TrimSpace(*p.Note)
	}
	if l, err = a.store.UpdateLink(l); err != nil {
		return l, err
	}
	return l, a.reload()
}

func (a *App) DeleteLink(id int64) error {
	if err := a.store.DeleteLink(id); err != nil {
		return err
	}
	return a.reload()
}

func (a *App) RecheckLink(id int64) error {
	if _, err := a.store.Link(id); err != nil {
		return err
	}
	a.store.SetCheck(id, "pending", "")
	go a.verify(id)
	return nil
}

// PutDomain connects a host (or updates one) and starts its DNS check.
func (a *App) PutDomain(host, resolver, fallback string) (Domain, error) {
	host = normHost(host)
	if !hostRe.MatchString(host) {
		return Domain{}, fail(400, "host must be a plain hostname like links.example.com")
	}
	if host == a.cfg.AdminHost {
		return Domain{}, fail(400, "that host is signpost's own dashboard")
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		if err := validTarget(fallback); err != nil {
			return Domain{}, fail(400, "fallback: "+err.Error())
		}
	}
	resolver = strings.TrimSpace(resolver)
	if resolver == "" {
		resolver = a.cfg.CertResolver
	}
	if err := a.store.PutDomain(Domain{Host: host, CertResolver: resolver, Fallback: fallback}); err != nil {
		return Domain{}, err
	}
	if err := a.reload(); err != nil {
		return Domain{}, err
	}
	a.store.SetDNS(host, "pending", "")
	go a.checkDomain(host)
	return a.store.Domain(host)
}

func (a *App) RecheckDomain(host string) error {
	if !a.store.HasDomain(host) {
		return ErrNotFound
	}
	a.store.SetDNS(host, "pending", "")
	go a.checkDomain(host)
	return nil
}

func (a *App) DeleteDomain(host string) error {
	if err := a.store.DeleteDomain(host); err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fail(409, err.Error())
	}
	return a.reload()
}
