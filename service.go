package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
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
	var de *DuplicateErr
	if errors.As(err, &de) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// conflictErr marks a 409 that force can override, as opposed to a duplicate.
type conflictErr struct{ msg string }

func (e *conflictErr) Error() string { return e.msg }

type linkInput struct {
	Host      string   `json:"host"`
	Path      string   `json:"path"` // empty: generate a short slug
	Target    string   `json:"target"`
	Status    int      `json:"status"`
	KeepQuery *bool    `json:"keep_query"`
	Note      string   `json:"note"`
	Tags      []string `json:"tags"`
	Password  string   `json:"password"`
	ExpiresAt *int64   `json:"expires_at"`
	Disabled  bool     `json:"disabled"`
	Force     bool     `json:"force"`
}

func validTarget(t string) error {
	u, err := url.Parse(strings.ReplaceAll(t, "{path}", "x"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fail(400, "destination must be an absolute http(s) URL")
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

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "/*" || p == "*" {
		return "*"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// normTags lowercases, dedupes and validates tags; "Launch Week" becomes
// "launch-week".
func normTags(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		for _, part := range strings.Split(t, ",") {
			part = strings.Join(strings.Fields(strings.ToLower(part)), "-")
			if part == "" || seen[part] {
				continue
			}
			if !tagRe.MatchString(part) {
				return nil, fail(400, fmt.Sprintf("tag %q: use letters, digits, - and _ (up to 32)", part))
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out, nil
}

// Slugs skip 0/O and 1/l/I, so a link read aloud or off a poster survives.
const slugAlphabet = "23456789abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"
const slugLen = 7

func randomSlug() string {
	b := make([]byte, slugLen)
	rand.Read(b)
	for i := range b {
		// 256 % 56 != 0, so reject the top of the byte range to keep it uniform.
		for b[i] >= 224 {
			var one [1]byte
			rand.Read(one[:])
			b[i] = one[0]
		}
		b[i] = slugAlphabet[int(b[i])%len(slugAlphabet)]
	}
	return string(b)
}

// newSlug picks a path no link on host uses yet, nor any in taken (the rest
// of a batch being imported).
func (a *App) newSlug(host string, taken map[string]bool) (string, error) {
	for range 10 {
		p := "/" + randomSlug()
		if !taken[host+p] && !a.store.PathTaken(host, p) {
			return p, nil
		}
	}
	return "", fail(500, "could not find a free slug; try again")
}

// prepareLink validates input and turns it into a Link, without touching the
// database or the network.
func (a *App) prepareLink(in linkInput, taken map[string]bool) (Link, error) {
	l := Link{
		Host:      normHost(in.Host),
		Path:      normPath(in.Path),
		Target:    strings.TrimSpace(in.Target),
		Status:    in.Status,
		Note:      strings.TrimSpace(in.Note),
		ExpiresAt: in.ExpiresAt,
		Disabled:  in.Disabled,
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
	case l.Path != "" && l.Path != "*" && (l.Path == "/" || !pathRe.MatchString(l.Path)):
		return l, fail(400, "path may use letters, digits and . _ ~ - / only, and cannot be the root")
	case !validStatus(l.Status):
		return l, fail(400, "status must be 301, 302, 307 or 308")
	}
	if err := validTarget(l.Target); err != nil {
		return l, err
	}
	var err error
	if l.Tags, err = normTags(in.Tags); err != nil {
		return l, err
	}
	if l.Path == "" {
		if l.Path, err = a.newSlug(l.Host, taken); err != nil {
			return l, err
		}
	}
	if err := checkLoop(l); err != nil {
		return l, err
	}
	if in.Password != "" {
		if l.PassHash, err = hashPassword(in.Password); err != nil {
			return l, err
		}
	}
	return l, nil
}

// checkLoop refuses a destination that is the link itself, which would
// bounce a visitor until the browser gives up.
func checkLoop(l Link) error {
	u, err := url.Parse(strings.ReplaceAll(l.Target, "{path}", "x"))
	if err != nil || strings.ToLower(u.Hostname()) != l.Host {
		return nil
	}
	p := u.Path
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if l.Path == "*" || p == l.Path {
		return fail(400, "the destination points back at this link, which would loop forever")
	}
	return nil
}

func (a *App) CreateLink(ctx context.Context, in linkInput) (Link, error) {
	l, err := a.prepareLink(in, nil)
	if err != nil {
		return l, err
	}
	if !in.Force {
		if msg := a.conflict(ctx, l); msg != "" {
			return l, &conflictErr{msg}
		}
	}
	newHost := !a.store.HasDomain(l.Host)
	l, err = a.store.CreateLink(l, a.cfg.CertResolver)
	if err != nil {
		return l, err
	}
	if err := a.reload(); err != nil {
		return l, err
	}
	a.queueVerify(l.ID)
	if newHost {
		go a.checkDomain(l.Host)
	}
	return l, nil
}

type linkPatch struct {
	Target    *string   `json:"target"`
	Status    *int      `json:"status"`
	KeepQuery *bool     `json:"keep_query"`
	Note      *string   `json:"note"`
	Tags      *[]string `json:"tags"`
	// Password: a new password, or "" to remove it.
	Password *string `json:"password"`
	// ExpiresAt: a unix time; 0 removes the expiry.
	ExpiresAt *int64 `json:"expires_at"`
	Disabled  *bool  `json:"disabled"`
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
		if err := checkLoop(l); err != nil {
			return l, err
		}
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
	if p.Tags != nil {
		if l.Tags, err = normTags(*p.Tags); err != nil {
			return l, err
		}
	}
	if p.Password != nil {
		l.PassHash = ""
		if *p.Password != "" {
			if l.PassHash, err = hashPassword(*p.Password); err != nil {
				return l, err
			}
		}
	}
	if p.ExpiresAt != nil {
		l.ExpiresAt = nil
		if *p.ExpiresAt > 0 {
			l.ExpiresAt = p.ExpiresAt
		}
	}
	if p.Disabled != nil {
		l.Disabled = *p.Disabled
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
	a.queueVerify(id)
	return nil
}

// Bulk actions on a selection of links: one transaction, one reload.
func (a *App) BulkAction(ids []int64, action, arg string) (int, error) {
	if len(ids) == 0 {
		return 0, fail(400, "select at least one link")
	}
	var n int
	var err error
	switch action {
	case "delete":
		n, err = a.store.DeleteLinks(ids)
	case "enable", "disable":
		off := action == "disable"
		n, err = a.store.UpdateLinks(ids, func(l *Link) { l.Disabled = off })
	case "status":
		var code int
		fmt.Sscan(arg, &code)
		if !validStatus(code) {
			return 0, fail(400, "status must be 301, 302, 307 or 308")
		}
		n, err = a.store.UpdateLinks(ids, func(l *Link) { l.Status = code })
	case "tag":
		tags, terr := normTags([]string{arg})
		if terr != nil {
			return 0, terr
		}
		if len(tags) == 0 {
			return 0, fail(400, "enter a tag")
		}
		n, err = a.store.UpdateLinks(ids, func(l *Link) {
			for _, t := range tags {
				if !contains(l.Tags, t) {
					l.Tags = append(l.Tags, t)
				}
			}
		})
	case "untag":
		n, err = a.store.UpdateLinks(ids, func(l *Link) {
			out := l.Tags[:0]
			for _, t := range l.Tags {
				if t != arg {
					out = append(out, t)
				}
			}
			l.Tags = out
		})
	default:
		return 0, fail(400, "unknown action "+action)
	}
	if err != nil {
		return 0, err
	}
	return n, a.reload()
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// bulkRow is one line of an import, after validation (and maybe the live
// check), and after the commit if there was one.
type bulkRow struct {
	Row    int       `json:"row"` // 1-based line in the input
	Input  linkInput `json:"input"`
	Link   *Link     `json:"link,omitempty"`
	Result string    `json:"result"` // ok, conflict, invalid, duplicate, created
	Msg    string    `json:"msg,omitempty"`
}

type bulkOpts struct {
	DryRun    bool `json:"dry_run"`
	Force     bool `json:"force"`      // create rows whose path is already answered by something else
	SkipProbe bool `json:"skip_probe"` // do not ask the live internet first
}

// BulkCreate validates every row, probes the live ones in parallel, and
// (unless dry run) creates every acceptable row in one transaction. It never
// half-applies: a database error rolls the whole batch back.
func (a *App) BulkCreate(ctx context.Context, ins []linkInput, o bulkOpts) ([]bulkRow, int, error) {
	if len(ins) > 5000 {
		return nil, 0, fail(400, "at most 5000 rows per import")
	}
	rows := make([]bulkRow, len(ins))
	links := make([]Link, len(ins))
	taken := map[string]bool{}
	for i, in := range ins {
		rows[i] = bulkRow{Row: i + 1, Input: in}
		in.Password = "" // imports do not carry passwords
		l, err := a.prepareLink(in, taken)
		switch {
		case err != nil:
			rows[i].Result, rows[i].Msg = "invalid", err.Error()
		case taken[l.Host+l.Path]:
			rows[i].Result, rows[i].Msg = "duplicate", "appears earlier in this import"
		case a.store.PathTaken(l.Host, l.Path):
			rows[i].Result, rows[i].Msg = "duplicate", l.URL()+" already exists"
		default:
			rows[i].Result = "ok"
			taken[l.Host+l.Path] = true
		}
		links[i] = l
		rows[i].Input.Host, rows[i].Input.Path = l.Host, l.Path
	}

	if !o.SkipProbe {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i := range rows {
			if rows[i].Result != "ok" {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer func() { <-sem; wg.Done() }()
				if msg := a.conflict(ctx, links[i]); msg != "" {
					rows[i].Result, rows[i].Msg = "conflict", msg
				}
			}(i)
		}
		wg.Wait()
	}

	var accept []int
	for i, r := range rows {
		if r.Result == "ok" || (o.Force && r.Result == "conflict") {
			accept = append(accept, i)
		}
	}
	if o.DryRun || len(accept) == 0 {
		return rows, 0, nil
	}
	batch := make([]Link, len(accept))
	newHosts := map[string]bool{}
	for j, i := range accept {
		batch[j] = links[i]
		if !a.store.HasDomain(links[i].Host) {
			newHosts[links[i].Host] = true
		}
	}
	created, err := a.store.CreateLinks(batch, a.cfg.CertResolver)
	if err != nil {
		return rows, 0, err
	}
	for j, i := range accept {
		l := created[j]
		rows[i].Link, rows[i].Result = &l, "created"
	}
	if err := a.reload(); err != nil {
		return rows, len(created), err
	}
	for _, l := range created {
		a.queueVerify(l.ID)
	}
	for h := range newHosts {
		go a.checkDomain(h)
	}
	return rows, len(created), nil
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

// SetDefaultHost picks the host the new-link form starts on.
func (a *App) SetDefaultHost(host string) error {
	host = normHost(host)
	if host != "" && !hostRe.MatchString(host) {
		return fail(400, "host must be a plain hostname like milind.dev")
	}
	return a.store.SetSetting("default_host", host)
}

func (a *App) defaultHost() string {
	if h := a.store.Setting("default_host"); h != "" {
		return h
	}
	var h string
	a.store.db.QueryRow(`SELECT host FROM links ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&h)
	return h
}

func unixOrNil(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	u := t.Unix()
	return &u
}
