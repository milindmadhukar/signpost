package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Domain is a host signpost may put routes on. Links create their domain on
// the fly, so a row here mostly exists to carry per-host settings.
type Domain struct {
	Host         string `json:"host"`
	CertResolver string `json:"cert_resolver"`
	// Fallback, when set, makes signpost the owner of the whole host: any path
	// without a link redirects here, with {path} replaced by the request path.
	// Leave it empty on hosts that already serve a real site.
	Fallback  string `json:"fallback"`
	CreatedAt int64  `json:"created_at"`
	Links     int    `json:"links"`
	// DNS is what the last domain check found: pending, ok, proxied (fine,
	// through Cloudflare), elsewhere, missing or error. DNSMsg says why.
	DNS        string `json:"dns"`
	DNSMsg     string `json:"dns_msg"`
	DNSChecked int64  `json:"dns_checked_at"`
	// Token names the /.well-known/signpost/<token> path the check fetches.
	Token string `json:"-"`
}

type Link struct {
	ID        int64    `json:"id"`
	Host      string   `json:"host"`
	Path      string   `json:"path"` // "/x", or "*" for every path on the host
	Target    string   `json:"target"`
	Status    int      `json:"status"`
	KeepQuery bool     `json:"keep_query"`
	Note      string   `json:"note"`
	Tags      []string `json:"tags"`
	Clicks    int64    `json:"clicks"`
	LastClick *int64   `json:"last_click"`
	// Check is what the last probe found: pending, live, dns (Traefik routes
	// it but public DNS does not reach this box) or error.
	Check     string `json:"check"`
	CheckMsg  string `json:"check_msg"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// ExpiresAt, when set, turns the link into a 410 from that moment on.
	ExpiresAt *int64 `json:"expires_at"`
	Disabled  bool   `json:"disabled"`
	Protected bool   `json:"protected"` // has a password; the hash never leaves the store
	PassHash  string `json:"-"`
}

func (l Link) URL() string {
	if l.Path == "*" {
		return l.Host + "/*"
	}
	return l.Host + l.Path
}

// Href is the clickable form of the link: a /* link points at the host root.
func (l Link) Href() string {
	if l.Path == "*" {
		return "https://" + l.Host + "/"
	}
	return "https://" + l.Host + l.Path
}

func (l Link) Expired(now int64) bool { return l.ExpiresAt != nil && now >= *l.ExpiresAt }

// State folds the probe result and the link's own switches into the one
// word the dashboard shows.
func (l Link) State() string {
	switch {
	case l.Disabled:
		return "disabled"
	case l.Expired(time.Now().Unix()):
		return "expired"
	}
	return l.Check
}

type Click struct {
	At       int64  `json:"at"`
	Referrer string `json:"referrer"`
	Country  string `json:"country"`
	UA       string `json:"ua"`
	RefHost  string `json:"ref_host"`
	Device   string `json:"device"`
	Browser  string `json:"browser"`
	OS       string `json:"os"`
	Bot      bool   `json:"bot"`
	Visitor  string `json:"-"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex // serialises writes; sqlite would anyway, this keeps errors out
}

var ErrNotFound = errors.New("not found")

// migrations run in order, each in its own transaction, and PRAGMA
// user_version records how far a database has got. Version 1 is the original
// schema, written with IF NOT EXISTS so databases from before versioning
// (user_version 0, tables already there) pass through it untouched.
var migrations = []func(*sql.Tx) error{
	execMigration(`
CREATE TABLE IF NOT EXISTS domains (
  host          TEXT PRIMARY KEY,
  cert_resolver TEXT NOT NULL DEFAULT '',
  fallback      TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  token         TEXT NOT NULL DEFAULT (lower(hex(randomblob(12)))),
  dns_state     TEXT NOT NULL DEFAULT 'pending',
  dns_msg       TEXT NOT NULL DEFAULT '',
  dns_checked_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS links (
  id         INTEGER PRIMARY KEY,
  host       TEXT NOT NULL REFERENCES domains(host),
  path       TEXT NOT NULL,
  target     TEXT NOT NULL,
  status     INTEGER NOT NULL DEFAULT 302,
  keep_query INTEGER NOT NULL DEFAULT 1,
  note       TEXT NOT NULL DEFAULT '',
  clicks     INTEGER NOT NULL DEFAULT 0,
  last_click INTEGER,
  check_state TEXT NOT NULL DEFAULT 'pending',
  check_msg  TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (host, path)
);
CREATE TABLE IF NOT EXISTS clicks (
  link_id  INTEGER NOT NULL REFERENCES links(id) ON DELETE CASCADE,
  at       INTEGER NOT NULL,
  referrer TEXT NOT NULL DEFAULT '',
  country  TEXT NOT NULL DEFAULT '',
  ua       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS clicks_link_at ON clicks(link_id, at);`),

	// 2: passwords, expiry, on/off and tags on links; parsed user agents and
	// a visitor hash on clicks; a settings table.
	func(tx *sql.Tx) error {
		if err := execMigration(`
ALTER TABLE links ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE links ADD COLUMN expires_at INTEGER;
ALTER TABLE links ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE links ADD COLUMN tags TEXT NOT NULL DEFAULT '';
ALTER TABLE clicks ADD COLUMN ref_host TEXT NOT NULL DEFAULT '';
ALTER TABLE clicks ADD COLUMN device TEXT NOT NULL DEFAULT '';
ALTER TABLE clicks ADD COLUMN browser TEXT NOT NULL DEFAULT '';
ALTER TABLE clicks ADD COLUMN os TEXT NOT NULL DEFAULT '';
ALTER TABLE clicks ADD COLUMN bot INTEGER NOT NULL DEFAULT 0;
ALTER TABLE clicks ADD COLUMN visitor TEXT NOT NULL DEFAULT '';
CREATE INDEX clicks_at ON clicks(at);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)(tx); err != nil {
			return err
		}
		// Classify the clicks recorded before user agents were parsed.
		rows, err := tx.Query(`SELECT rowid, referrer, ua FROM clicks`)
		if err != nil {
			return err
		}
		type old struct {
			id      int64
			ref, ua string
		}
		var olds []old
		for rows.Next() {
			var o old
			if err := rows.Scan(&o.id, &o.ref, &o.ua); err != nil {
				rows.Close()
				return err
			}
			olds = append(olds, o)
		}
		rows.Close()
		for _, o := range olds {
			ua := parseUA(o.ua)
			if _, err := tx.Exec(`UPDATE clicks SET ref_host = ?, device = ?, browser = ?, os = ?, bot = ? WHERE rowid = ?`,
				refHost(o.ref), ua.Device, ua.Browser, ua.OS, b2i(ua.Bot), o.id); err != nil {
				return err
			}
		}
		return nil
	},
}

func execMigration(q string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(q)
		return err
	}
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One connection: writes are serialised anyway, and PRAGMA data_version
	// (the staleness check) is only meaningful on a connection that stays put.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if err := migrations[i](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// DataVersion changes whenever another connection (a sqlite3 shell, a
// restored backup) commits to the file. Commits on this process's own
// connection leave it alone, and those already reload the cache themselves.
func (s *Store) DataVersion() (int64, error) {
	var v int64
	err := s.db.QueryRow(`PRAGMA data_version`).Scan(&v)
	return v, err
}

const linkCols = `id, host, path, target, status, keep_query, note, tags, clicks, last_click, check_state, check_msg, created_at, updated_at, expires_at, disabled, password_hash`

func scanLink(row interface{ Scan(...any) error }) (Link, error) {
	var l Link
	var keep, disabled int
	var tags string
	var last, exp sql.NullInt64
	err := row.Scan(&l.ID, &l.Host, &l.Path, &l.Target, &l.Status, &keep, &l.Note, &tags, &l.Clicks, &last,
		&l.Check, &l.CheckMsg, &l.CreatedAt, &l.UpdatedAt, &exp, &disabled, &l.PassHash)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	l.KeepQuery = keep == 1
	l.Disabled = disabled == 1
	l.Protected = l.PassHash != ""
	l.Tags = splitTags(tags)
	if last.Valid {
		l.LastClick = &last.Int64
	}
	if exp.Valid {
		l.ExpiresAt = &exp.Int64
	}
	return l, err
}

// LinkFilter narrows the links list. Zero values mean "any".
type LinkFilter struct {
	Q     string // substring of host+path, target, note or tags
	Host  string
	State string // live, pending, dns, error, disabled, expired, protected
	Tag   string
}

func (s *Store) Links() ([]Link, error) { return s.FindLinks(LinkFilter{}) }

func (s *Store) FindLinks(f LinkFilter) ([]Link, error) {
	q := `SELECT ` + linkCols + ` FROM links WHERE 1=1`
	var args []any
	if f.Q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(f.Q)) + "%"
		q += ` AND (lower(host || path) LIKE ? ESCAPE '\' OR lower(target) LIKE ? ESCAPE '\' OR lower(note) LIKE ? ESCAPE '\' OR lower(tags) LIKE ? ESCAPE '\')`
		args = append(args, like, like, like, like)
	}
	if f.Host != "" {
		q += ` AND host = ?`
		args = append(args, f.Host)
	}
	if f.Tag != "" {
		q += ` AND (',' || tags || ',') LIKE ?`
		args = append(args, "%,"+f.Tag+",%")
	}
	now := time.Now().Unix()
	switch f.State {
	case "disabled":
		q += ` AND disabled = 1`
	case "expired":
		q += ` AND disabled = 0 AND expires_at IS NOT NULL AND expires_at <= ?`
		args = append(args, now)
	case "protected":
		q += ` AND password_hash != ''`
	case "live", "pending", "dns", "error":
		q += ` AND disabled = 0 AND (expires_at IS NULL OR expires_at > ?) AND check_state = ?`
		args = append(args, now, f.State)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) Link(id int64) (Link, error) {
	return scanLink(s.db.QueryRow(`SELECT `+linkCols+` FROM links WHERE id = ?`, id))
}

func (s *Store) LinkByURL(host, path string) (Link, error) {
	return scanLink(s.db.QueryRow(`SELECT `+linkCols+` FROM links WHERE host = ? AND path = ?`, host, path))
}

// PathTaken reports whether host+path already has a link, for slug retries.
func (s *Store) PathTaken(host, path string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM links WHERE host = ? AND path = ?`, host, path).Scan(&n)
	return n > 0
}

// Tags lists every tag in use, for the filter menu.
func (s *Store) Tags() []string {
	rows, err := s.db.Query(`SELECT DISTINCT tags FROM links WHERE tags != ''`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		for _, tag := range splitTags(t) {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (s *Store) ensureDomain(tx *sql.Tx, host, resolver string) error {
	_, err := tx.Exec(`INSERT INTO domains (host, cert_resolver, created_at) VALUES (?, ?, ?) ON CONFLICT(host) DO NOTHING`,
		host, resolver, time.Now().Unix())
	return err
}

// DuplicateErr is a host+path that already has a link.
type DuplicateErr struct{ URL string }

func (e *DuplicateErr) Error() string { return e.URL + " already exists" }

// CreateLinks inserts every link in one transaction: all of them land, or
// none do.
func (s *Store) CreateLinks(ls []Link, defaultResolver string) ([]Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	ids := make([]int64, 0, len(ls))
	for _, l := range ls {
		if err := s.ensureDomain(tx, l.Host, defaultResolver); err != nil {
			return nil, err
		}
		res, err := tx.Exec(`INSERT INTO links (host, path, target, status, keep_query, note, tags, expires_at, disabled, password_hash, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.Host, l.Path, l.Target, l.Status, b2i(l.KeepQuery), l.Note, joinTags(l.Tags), nullInt(l.ExpiresAt), b2i(l.Disabled), l.PassHash, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, &DuplicateErr{l.URL()}
			}
			return nil, err
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := make([]Link, 0, len(ids))
	for _, id := range ids {
		l, err := s.Link(id)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

func (s *Store) CreateLink(l Link, defaultResolver string) (Link, error) {
	ls, err := s.CreateLinks([]Link{l}, defaultResolver)
	if err != nil {
		return l, err
	}
	return ls[0], nil
}

// UpdateLink writes every editable field. Callers load, change, then save.
func (s *Store) UpdateLink(l Link) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE links SET target = ?, status = ?, keep_query = ?, note = ?, tags = ?, expires_at = ?, disabled = ?, password_hash = ?, updated_at = ? WHERE id = ?`,
		l.Target, l.Status, b2i(l.KeepQuery), l.Note, joinTags(l.Tags), nullInt(l.ExpiresAt), b2i(l.Disabled), l.PassHash, time.Now().Unix(), l.ID)
	if err != nil {
		return l, err
	}
	return s.Link(l.ID)
}

// UpdateLinks applies change to each link and saves them all in one
// transaction.
func (s *Store) UpdateLinks(ids []int64, change func(*Link)) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	n := 0
	for _, id := range ids {
		l, err := scanLink(tx.QueryRow(`SELECT `+linkCols+` FROM links WHERE id = ?`, id))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		change(&l)
		if _, err := tx.Exec(`UPDATE links SET target = ?, status = ?, keep_query = ?, note = ?, tags = ?, expires_at = ?, disabled = ?, password_hash = ?, updated_at = ? WHERE id = ?`,
			l.Target, l.Status, b2i(l.KeepQuery), l.Note, joinTags(l.Tags), nullInt(l.ExpiresAt), b2i(l.Disabled), l.PassHash, now, l.ID); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

func (s *Store) DeleteLink(id int64) error {
	n, err := s.DeleteLinks([]int64{id})
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) DeleteLinks(ids []int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, id := range ids {
		res, err := tx.Exec(`DELETE FROM links WHERE id = ?`, id)
		if err != nil {
			return 0, err
		}
		k, _ := res.RowsAffected()
		n += int(k)
	}
	return n, tx.Commit()
}

func (s *Store) SetCheck(id int64, state, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`UPDATE links SET check_state = ?, check_msg = ? WHERE id = ?`, state, msg, id)
}

type clickEvent struct {
	id int64
	c  Click
}

// RecordClicks writes a batch of clicks and bumps the counters in one
// transaction. Bots are kept for the record but do not count.
func (s *Store) RecordClicks(evs []clickEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT INTO clicks (link_id, at, referrer, country, ua, ref_host, device, browser, os, bot, visitor)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM links WHERE id = ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	type bump struct {
		n    int
		last int64
	}
	bumps := map[int64]*bump{}
	for _, ev := range evs {
		c := ev.c
		if _, err := ins.Exec(ev.id, c.At, c.Referrer, c.Country, c.UA, c.RefHost, c.Device, c.Browser, c.OS, b2i(c.Bot), c.Visitor, ev.id); err != nil {
			return err
		}
		if c.Bot {
			continue
		}
		b := bumps[ev.id]
		if b == nil {
			b = &bump{}
			bumps[ev.id] = b
		}
		b.n++
		b.last = max(b.last, c.At)
	}
	for id, b := range bumps {
		if _, err := tx.Exec(`UPDATE links SET clicks = clicks + ?, last_click = max(coalesce(last_click, 0), ?) WHERE id = ?`, b.n, b.last, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecordClick(id int64, c Click) error {
	return s.RecordClicks([]clickEvent{{id, c}})
}

func (s *Store) Clicks(id int64, limit int) ([]Click, error) {
	rows, err := s.db.Query(`SELECT at, referrer, country, ua, ref_host, device, browser, os, bot FROM clicks WHERE link_id = ? ORDER BY at DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Click{}
	for rows.Next() {
		var c Click
		var bot int
		if err := rows.Scan(&c.At, &c.Referrer, &c.Country, &c.UA, &c.RefHost, &c.Device, &c.Browser, &c.OS, &bot); err != nil {
			return nil, err
		}
		c.Bot = bot == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Domains() ([]Domain, error) {
	rows, err := s.db.Query(`SELECT d.host, d.cert_resolver, d.fallback, d.created_at, COUNT(l.id), d.dns_state, d.dns_msg, d.dns_checked_at, d.token
		FROM domains d LEFT JOIN links l ON l.host = d.host GROUP BY d.host ORDER BY d.host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Host, &d.CertResolver, &d.Fallback, &d.CreatedAt, &d.Links, &d.DNS, &d.DNSMsg, &d.DNSChecked, &d.Token); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) PutDomain(d Domain) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO domains (host, cert_resolver, fallback, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET cert_resolver = excluded.cert_resolver, fallback = excluded.fallback`,
		d.Host, d.CertResolver, d.Fallback, time.Now().Unix())
	return err
}

// HasDomain lets callers spot a brand-new host, so its first DNS check runs.
func (s *Store) HasDomain(host string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM domains WHERE host = ?`, host).Scan(&n)
	return n > 0
}

func (s *Store) Domain(host string) (Domain, error) {
	ds, err := s.Domains()
	if err != nil {
		return Domain{}, err
	}
	for _, d := range ds {
		if d.Host == host {
			return d, nil
		}
	}
	return Domain{}, ErrNotFound
}

func (s *Store) SetDNS(host, state, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`UPDATE domains SET dns_state = ?, dns_msg = ?, dns_checked_at = ? WHERE host = ?`, state, msg, time.Now().Unix(), host)
}

func (s *Store) DeleteDomain(host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM links WHERE host = ?`, host).Scan(&n)
	if n > 0 {
		return fmt.Errorf("%s still has %d link(s); remove them first", host, n)
	}
	res, err := s.db.Exec(`DELETE FROM domains WHERE host = ?`, host)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Secret is the instance key behind unlock cookies and visitor hashes,
// made on first use and kept in the database so restarts keep cookies valid.
func (s *Store) Secret() ([]byte, error) {
	if v := s.Setting("secret"); v != "" {
		return hex.DecodeString(v)
	}
	b := make([]byte, 32)
	rand.Read(b)
	s.mu.Lock()
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES ('secret', ?) ON CONFLICT(key) DO NOTHING`, hex.EncodeToString(b))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(s.Setting("secret"))
}

// refHost reduces a referrer to the site it came from. Apps bounce outbound
// links through redirector subdomains (l.instagram.com, lm.facebook.com,
// out.reddit.com), which are folded into the site so it counts once.
func refHost(ref string) string {
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range []string{"www.", "l.", "lm.", "m.", "mobile.", "out.", "web.", "old.", "new."} {
		if rest, ok := strings.CutPrefix(h, p); ok && strings.Contains(rest, ".") {
			h = rest
			break
		}
	}
	if alias, ok := refAliases[h]; ok {
		return alias
	}
	return h
}

var refAliases = map[string]string{
	"t.co": "x.com", "twitter.com": "x.com", "lnkd.in": "linkedin.com", "youtu.be": "youtube.com",
	"com.google.android.gm": "gmail", "android-app://com.google.android.gm": "gmail",
}

func splitTags(s string) []string {
	out := []string{}
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func joinTags(tags []string) string { return strings.Join(tags, ",") }

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
