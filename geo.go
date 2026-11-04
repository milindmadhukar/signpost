package main

import (
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// geoDB answers "where is this IP" from a local MaxMind-format database
// (DB-IP's free city file in the image). It is only a fallback: a domain
// proxied by Cloudflare already says so in CF-IPCountry. The lookup never
// leaves the process, and only the country code and city name are kept.
type geoDB struct {
	r     *maxminddb.Reader
	path  string
	built time.Time
	desc  string // "DBIP-City-Lite" and the like
}

func openGeo(path string) (*geoDB, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	g := &geoDB{r: r, path: path, built: time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC(), desc: r.Metadata.DatabaseType}
	return g, nil
}

// place is where a click came from: an ISO 3166-1 alpha-2 country code and,
// when the database has one, a city name in English. Either may be empty.
type place struct {
	Country, City string
}

// geoRecord is the part of a GeoIP2-shaped record signpost reads. A
// country-only database simply has no city.
type geoRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	City struct {
		Names struct {
			EN string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"city"`
}

// Lookup places ip, or returns the zero place when the address is unknown,
// private, or there is no database. Safe on a nil receiver, so callers need
// not check whether one is loaded.
func (g *geoDB) Lookup(ip string) place {
	if g == nil {
		return place{}
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return place{}
	}
	var rec geoRecord
	if err := g.r.Lookup(addr.Unmap()).Decode(&rec); err != nil {
		return place{}
	}
	p := place{Country: strings.ToUpper(rec.Country.ISOCode), City: cleanCity(rec.City.Names.EN)}
	if p.Country == "" {
		p.City = "" // a city with no country cannot be shown or grouped
	}
	return p
}

// cleanCity trims a name to the city itself. DB-IP sometimes names the
// neighbourhood too, "Navi Mumbai (Ghansoli)", which splits one city's
// clicks into many rows and claims more precision than an IP lookup has.
func cleanCity(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[:i])
	}
	return strings.ToValidUTF8(trunc(s, 80), "") // never half a character
}

func (g *geoDB) Close() error {
	if g == nil {
		return nil
	}
	return g.r.Close()
}

// clickPlace prefers what Cloudflare saw and falls back to the local
// database. "XX" is Cloudflare's own "unknown", so the database gets a go at
// it too. Cloudflare only sends CF-IPCity with its visitor location headers
// turned on; without it, the database's city is used when the two agree on
// the country, so a click is never put in a city outside its own country.
func clickPlace(r *http.Request, geo *geoDB) place {
	cf := strings.ToUpper(strings.TrimSpace(r.Header.Get("Cf-Ipcountry")))
	if cf != "" && cf != "XX" {
		p := place{Country: cf, City: cleanCity(r.Header.Get("Cf-Ipcity"))}
		if p.City == "" {
			if db := geo.Lookup(clientIP(r)); db.Country == cf {
				p.City = db.City
			}
		}
		return p
	}
	if p := geo.Lookup(clientIP(r)); p.Country != "" {
		return p
	}
	return place{Country: cf}
}
