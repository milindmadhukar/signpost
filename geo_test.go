package main

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// testGeoDB writes a tiny MaxMind-format country database shaped like
// DB-IP's: {"country": {"iso_code": "GB"}} per network.
func testGeoDB(t *testing.T) *geoDB {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "DBIP-Country-Lite", RecordSize: 24})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, code := range map[string]string{
		"81.2.69.0/24":   "GB",
		"49.36.0.0/16":   "IN",
		"2a00:1450::/32": "US",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		rec := mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(code)}}
		if err := tree.Insert(n, rec); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "country.mmdb")
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

func TestGeoCountry(t *testing.T) {
	g := testGeoDB(t)
	for ip, want := range map[string]string{
		"81.2.69.142":           "GB",
		" 49.36.12.7 ":          "IN",
		"::ffff:81.2.69.1":      "GB", // IPv4-mapped, as some proxies write it
		"2a00:1450:4001:82a::1": "US",
		"8.8.8.8":               "", // not in this database
		"192.168.1.20":          "", // private
		"127.0.0.1":             "",
		"not an ip":             "",
		"":                      "",
	} {
		if got := g.Country(ip); got != want {
			t.Errorf("Country(%q) = %q, want %q", ip, got, want)
		}
	}
	var none *geoDB
	if got := none.Country("81.2.69.142"); got != "" {
		t.Errorf("nil database returned %q", got)
	}
}

func TestClickCountryPrefersCloudflare(t *testing.T) {
	g := testGeoDB(t)
	cases := []struct {
		name, cf, realIP, want string
		geo                    *geoDB
	}{
		{"cloudflare wins", "de", "81.2.69.142", "DE", g},
		{"no header: database", "", "81.2.69.142", "GB", g},
		{"cloudflare unknown: database", "XX", "49.36.1.1", "IN", g},
		{"cloudflare unknown, database too", "XX", "8.8.8.8", "XX", g},
		{"nothing known", "", "8.8.8.8", "", g},
		{"no database", "", "81.2.69.142", "", nil},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.cf != "" {
			r.Header.Set("Cf-Ipcountry", c.cf)
		}
		r.Header.Set("X-Real-Ip", c.realIP)
		if got := clickCountry(r, c.geo); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A click on a domain that is not behind Cloudflare gets its country from
// the database, and the IP itself is still not stored.
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
	if cs[0].Country != "IN" {
		t.Fatalf("country = %q, want IN", cs[0].Country)
	}
}
