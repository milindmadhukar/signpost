package main

import (
	"strings"
	"testing"
)

func TestWorldMapData(t *testing.T) {
	if world.ViewBox == "" || len(world.Shapes) < 200 {
		t.Fatalf("world map: viewBox %q, %d shapes", world.ViewBox, len(world.Shapes))
	}
	seen := map[string]bool{}
	for _, s := range world.Shapes {
		if !strings.HasPrefix(s.D, "M") || !strings.HasSuffix(s.D, "z") {
			t.Errorf("%s: bad path %.30q", s.Code, s.D)
		}
		if s.Code == "" {
			continue
		}
		if seen[s.Code] {
			t.Errorf("%s drawn twice", s.Code)
		}
		seen[s.Code] = true
		if countryName(s.Code) == s.Code {
			t.Errorf("%s has no name", s.Code)
		}
	}
	for _, c := range []string{"IN", "US", "SG", "NL", "MT", "BH"} {
		if !seen[c] {
			t.Errorf("%s is not on the map", c)
		}
	}
}

func TestGeoLevel(t *testing.T) {
	for _, c := range []struct{ n, max, want int }{
		{0, 100, 0}, {1, 1, 5}, {1, 100, 1}, {100, 100, 5}, {10, 100, 3}, {99, 100, 5}, {2, 3, 4},
	} {
		if got := geoLevel(c.n, c.max); got != c.want {
			t.Errorf("geoLevel(%d, %d) = %d, want %d", c.n, c.max, got, c.want)
		}
	}
}

func TestMakeWorldMap(t *testing.T) {
	st := &Stats{Clicks: 20, GeoCountries: []statRow{
		{Key: "IN", N: 10}, {Key: "", N: 4}, {Key: "XX", N: 1}, {Key: "us", N: 3}, {Key: "GI", N: 2},
	}}
	v := makeWorldMap(st)
	if v.Max != 10 || v.Unknown != 5 || v.Total != 20 {
		t.Fatalf("max %d unknown %d total %d", v.Max, v.Unknown, v.Total)
	}
	got := map[string]mapShape{}
	for _, s := range v.Shapes {
		got[s.Code] = s
	}
	if in := got["IN"]; in.N != 10 || in.Level != 5 || in.Name != "India" || in.Pct != "50%" {
		t.Errorf("IN: %+v", in)
	}
	if us := got["US"]; us.N != 3 || us.Level == 0 {
		t.Errorf("US (sent lower-case): %+v", us)
	}
	if fr := got["FR"]; fr.N != 0 || fr.Level != 0 {
		t.Errorf("FR: %+v", fr)
	}
	// Gibraltar is too small for the 1:50m map.
	if _, ok := got["GI"]; ok || v.Unmapped != 2 {
		t.Errorf("GI on map %v, unmapped %d", ok, v.Unmapped)
	}
}
