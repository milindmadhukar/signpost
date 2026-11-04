package main

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// testGeoDB writes a tiny MaxMind-format database shaped like DB-IP's city
// file: {"country": {"iso_code": "GB"}, "city": {"names": {"en": "London"}}}
// per network. An empty city leaves the key out, as a country file would.
func testGeoDB(t *testing.T) *geoDB {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "DBIP-City-Lite", RecordSize: 24})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, at := range map[string]place{
		"81.2.69.0/24":   {"GB", "London"},
		"49.36.0.0/16":   {"IN", "Navi Mumbai (Ghansoli)"},
		"106.51.0.0/16":  {"IN", "Bengaluru"},
		"2a00:1450::/32": {"US", ""},
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		rec := mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(at.Country)}}
		if at.City != "" {
			rec["city"] = mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(at.City)}}
		}
		if err := tree.Insert(n, rec); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "city.mmdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.WriteTo(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	g, err := openGeo(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestGeoLookup(t *testing.T) {
	g := testGeoDB(t)
	for ip, want := range map[string]place{
		"81.2.69.142":           {"GB", "London"},
		" 49.36.12.7 ":          {"IN", "Navi Mumbai"}, // neighbourhood dropped
		"::ffff:81.2.69.1":      {"GB", "London"},      // IPv4-mapped, as some proxies write it
		"2a00:1450:4001:82a::1": {"US", ""},            // country only
		"8.8.8.8":               {},                    // not in this database
		"192.168.1.20":          {},                    // private
		"127.0.0.1":             {},
		"not an ip":             {},
		"":                      {},
	} {
		if got := g.Lookup(ip); got != want {
			t.Errorf("Lookup(%q) = %+v, want %+v", ip, got, want)
		}
	}
	var none *geoDB
	if got := none.Lookup("81.2.69.142"); got != (place{}) {
		t.Errorf("nil database returned %+v", got)
	}
}

func TestCleanCity(t *testing.T) {
	for in, want := range map[string]string{
		"Navi Mumbai (Ghansoli)": "Navi Mumbai",
		"  Pune ":                "Pune",
		"(Unknown)":              "(Unknown)", // nothing left of it to keep
		"Frankfurt am Main":      "Frankfurt am Main",
		"":                       "",
		strings.Repeat("é", 50):  strings.Repeat("é", 40), // cut on a character, not mid-byte
	} {
		if got := cleanCity(in); got != want {
			t.Errorf("cleanCity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClickPlacePrefersCloudflare(t *testing.T) {
	g := testGeoDB(t)
	cases := []struct {
		name, cf, cfCity, realIP string
		geo                      *geoDB
		want                     place
	}{
		{"cloudflare wins", "de", "Berlin", "81.2.69.142", g, place{"DE", "Berlin"}},
		{"cloudflare country, database city in it", "IN", "", "106.51.3.4", g, place{"IN", "Bengaluru"}},
		{"cloudflare country, database disagrees", "DE", "", "81.2.69.142", g, place{"DE", ""}},
		{"no header: database", "", "", "81.2.69.142", g, place{"GB", "London"}},
		{"cloudflare unknown: database", "XX", "", "49.36.1.1", g, place{"IN", "Navi Mumbai"}},
		{"cloudflare unknown, database too", "XX", "", "8.8.8.8", g, place{"XX", ""}},
		{"nothing known", "", "", "8.8.8.8", g, place{}},
		{"no database", "", "", "81.2.69.142", nil, place{}},
		{"no database, cloudflare", "GB", "", "81.2.69.142", nil, place{"GB", ""}},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.cf != "" {
			r.Header.Set("Cf-Ipcountry", c.cf)
		}
		if c.cfCity != "" {
			r.Header.Set("Cf-Ipcity", c.cfCity)
		}
		r.Header.Set("X-Real-Ip", c.realIP)
		if got := clickPlace(r, c.geo); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A click on a domain that is not behind Cloudflare gets its country and
// city from the database, and the IP itself is still not stored.
func TestClickCountryRecorded(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev")
	a.geo = testGeoDB(t)
	l := mustCreate(t, a, Link{Host: "milind.dev", Path: "/portfolio", Target: "https://example.com"})[0]
	req := httptest.NewRequest("GET", "/portfolio", nil)
	req.Host = "milind.dev"
	req.RemoteAddr = "172.18.0.2:41000" // Traefik, on the Docker network
	req.Header.Set("X-Real-Ip", "49.36.200.9")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 453.0.0.39.58")
	a.redirect(httptest.NewRecorder(), req)
	close(a.clicks)
	a.clickWriter(time.Hour, 500)
	cs, err := a.store.Clicks(l.ID, 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("clicks %v, err %v", cs, err)
	}
	if cs[0].Country != "IN" || cs[0].City != "Navi Mumbai" {
		t.Fatalf("place = %q, %q, want IN, Navi Mumbai", cs[0].Country, cs[0].City)
	}
}

// Cities are counted with their country, so two places sharing a name stay
// apart, and clicks with no city are left out of the list.
func TestStatsCities(t *testing.T) {
	a, _ := newTestApp(t, "milind.dev")
	l := mustCreate(t, a, Link{Host: "milind.dev", Path: "/ade", Target: "https://example.com"})[0]
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Unix()
	var evs []clickEvent
	for _, c := range []place{
		{"IN", "Hyderabad"}, {"IN", "Hyderabad"}, {"IN", "Hyderabad"},
		{"PK", "Hyderabad"},
		{"IN", "Pune"}, {"IN", "Pune"},
		{"IN", ""}, {"", ""},
	} {
		evs = append(evs, clickEvent{l.ID, Click{At: at, Country: c.Country, City: c.City}})
	}
	evs = append(evs, clickEvent{l.ID, Click{At: at, Country: "IN", City: "Pune", Bot: true}})
	if err := a.store.RecordClicks(evs); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Stats(statsQuery{Range: parseRange("7d"), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range st.Cities {
		got = append(got, fmt.Sprintf("%s=%d", r.Label, r.N))
	}
	want := []string{"Hyderabad, India=3", "Pune, India=2", "Hyderabad, Pakistan=1"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("cities = %v, want %v", got, want)
	}
	cities := 0
	for _, r := range st.Recent {
		if r.City != "" {
			cities++
		}
	}
	if cities != 6 {
		t.Fatalf("%d recent clicks have a city, want 6", cities)
	}
}
