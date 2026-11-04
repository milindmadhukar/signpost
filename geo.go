package main

import (
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// geoDB answers "which country is this IP in" from a local MaxMind-format
// database (DB-IP's free country file in the image). It is only a fallback:
// a domain proxied by Cloudflare already says so in CF-IPCountry. The lookup
// never leaves the process, and only the two-letter code is kept.
type geoDB struct {
	r     *maxminddb.Reader
	path  string
	built time.Time
	desc  string // "DBIP-Country-Lite" and the like
}

func openGeo(path string) (*geoDB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	g := &geoDB{r: r, path: path, built: time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC(), desc: r.Metadata.DatabaseType}
	return g, nil
}

// Country returns the ISO 3166-1 alpha-2 code for ip, or "" when the
// address is unknown, private, or there is no database. Safe on a nil
// receiver, so callers need not check whether one is loaded.
func (g *geoDB) Country(ip string) string {
	if g == nil {
		return ""
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return ""
	}
	var code string
	if err := g.r.Lookup(addr.Unmap()).DecodePath(&code, "country", "iso_code"); err != nil {
		return ""
	}
	return strings.ToUpper(code)
}

func (g *geoDB) Close() error {
	if g == nil {
		return nil
	}
	return g.r.Close()
}

// clickCountry prefers what Cloudflare saw and falls back to the local
// database. "XX" is Cloudflare's own "unknown", so the database gets a go at
// it too.
func clickCountry(r *http.Request, geo *geoDB) string {
	cf := strings.ToUpper(strings.TrimSpace(r.Header.Get("Cf-Ipcountry")))
	if cf != "" && cf != "XX" {
		return cf
	}
	if c := geo.Country(clientIP(r)); c != "" {
		return c
	}
	return cf
}
