package main

import (
	"database/sql"
	"errors"
	"fmt"
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
	ID        int64  `json:"id"`
	Host      string `json:"host"`
	Path      string `json:"path"` // "/x", or "*" for every path on the host
	Target    string `json:"target"`
	Status    int    `json:"status"`
	KeepQuery bool   `json:"keep_query"`
	Note      string `json:"note"`
	Clicks    int64  `json:"clicks"`
	LastClick *int64 `json:"last_click"`
	// Check is what the last probe found: pending, live, dns (Traefik routes
	// it but public DNS does not reach this box) or error.
	Check     string `json:"check"`
	CheckMsg  string `json:"check_msg"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (l Link) URL() string {
	if l.Path == "*" {
		return l.Host + "/*"
	}
	return l.Host + l.Path
}

type Click struct {
	At       int64  `json:"at"`
	Referrer string `json:"referrer"`
	Country  string `json:"country"`
	UA       string `json:"ua"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex // serialises writes; sqlite would anyway, this keeps errors out
}

var ErrNotFound = errors.New("not found")

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
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
CREATE INDEX IF NOT EXISTS clicks_link_at ON clicks(link_id, at);
`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

const linkCols = `id, host, path, target, status, keep_query, note, clicks, last_click, check_state, check_msg, created_at, updated_at`

func scanLink(row interface{ Scan(...any) error }) (Link, error) {
	var l Link
	var keep int
	var last sql.NullInt64
	err := row.Scan(&l.ID, &l.Host, &l.Path, &l.Target, &l.Status, &keep, &l.Note, &l.Clicks, &last, &l.Check, &l.CheckMsg, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	l.KeepQuery = keep == 1
	if last.Valid {
		l.LastClick = &last.Int64
	}
	return l, err
}

func (s *Store) Links() ([]Link, error) {
	rows, err := s.db.Query(`SELECT ` + linkCols + ` FROM links ORDER BY host, path`)
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

func (s *Store) ensureDomain(tx *sql.Tx, host, resolver string) error {
	_, err := tx.Exec(`INSERT INTO domains (host, cert_resolver, created_at) VALUES (?, ?, ?) ON CONFLICT(host) DO NOTHING`,
		host, resolver, time.Now().Unix())
	return err
}

func (s *Store) CreateLink(l Link, defaultResolver string) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return l, err
	}
	defer tx.Rollback()
	if err := s.ensureDomain(tx, l.Host, defaultResolver); err != nil {
		return l, err
	}
	now := time.Now().Unix()
	res, err := tx.Exec(`INSERT INTO links (host, path, target, status, keep_query, note, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Host, l.Path, l.Target, l.Status, b2i(l.KeepQuery), l.Note, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return l, fmt.Errorf("%s already exists", l.URL())
		}
		return l, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return l, err
	}
	return s.Link(id)
}

func (s *Store) UpdateLink(l Link) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE links SET target = ?, status = ?, keep_query = ?, note = ?, updated_at = ? WHERE id = ?`,
		l.Target, l.Status, b2i(l.KeepQuery), l.Note, time.Now().Unix(), l.ID)
	if err != nil {
		return l, err
	}
	return s.Link(l.ID)
}

func (s *Store) DeleteLink(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM links WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetCheck(id int64, state, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`UPDATE links SET check_state = ?, check_msg = ? WHERE id = ?`, state, msg, id)
}

func (s *Store) RecordClick(id int64, c Click) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO clicks (link_id, at, referrer, country, ua) VALUES (?, ?, ?, ?, ?)`,
		id, c.At, c.Referrer, c.Country, c.UA); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE links SET clicks = clicks + 1, last_click = ? WHERE id = ?`, c.At, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Clicks(id int64, limit int) ([]Click, error) {
	rows, err := s.db.Query(`SELECT at, referrer, country, ua FROM clicks WHERE link_id = ? ORDER BY at DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Click{}
	for rows.Next() {
		var c Click
		if err := rows.Scan(&c.At, &c.Referrer, &c.Country, &c.UA); err != nil {
			return nil, err
		}
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

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
