package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Click analytics, computed on demand from the raw clicks table. With an
// index on (at) and (link_id, at) these stay fast well into the millions of
// rows, which is far past what a personal link box sees, so there are no
// rollup tables to keep in sync. Bots are always left out.

type statRange struct {
	Key    string
	Label  string
	Span   int           // number of buckets
	Bucket time.Duration // hour or day
}

var statRanges = []statRange{
	{"24h", "Last 24 hours", 24, time.Hour},
	{"7d", "Last 7 days", 7, 24 * time.Hour},
	{"30d", "Last 30 days", 30, 24 * time.Hour},
	{"90d", "Last 90 days", 90, 24 * time.Hour},
}

func parseRange(s string) statRange {
	for _, r := range statRanges {
		if r.Key == s {
			return r
		}
	}
	return statRanges[2]
}

type statsQuery struct {
	LinkID int64
	Range  statRange
	Now    time.Time
	TZ     int // viewer's offset from UTC in minutes, east positive
}

type statPoint struct {
	Start int64 `json:"start"` // bucket start, unix seconds
	N     int   `json:"clicks"`
}

type statRow struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	N     int    `json:"clicks"`
	ID    int64  `json:"id,omitempty"` // for top links
}

type recentClick struct {
	Click
	LinkID  int64  `json:"link_id"`
	LinkURL string `json:"link"`
}

type Stats struct {
	From, To     int64 `json:"-"`
	Clicks       int   `json:"clicks"`
	Visitors     int   `json:"visitors"`
	PrevClicks   int   `json:"prev_clicks"`
	PrevVisitors int   `json:"prev_visitors"`
	Bots         int   `json:"bots"`

	Series []statPoint `json:"series"`
	// Heat is clicks by weekday (Monday first) and hour of day, in the
	// viewer's time zone.
	Heat    [7][24]int `json:"heatmap"`
	HeatMax int        `json:"-"`

	Links     []statRow `json:"top_links"`
	Referrers []statRow `json:"referrers"`
	Countries []statRow `json:"countries"`
	// GeoCountries is every country, for the map; Countries is its top 8.
	GeoCountries []statRow     `json:"-"`
	Cities       []statRow     `json:"cities"`
	Devices      []statRow     `json:"devices"`
	Browsers     []statRow     `json:"browsers"`
	OSes         []statRow     `json:"os"`
	Recent       []recentClick `json:"recent"`

	Range  statRange `json:"-"`
	Offset int64     `json:"-"` // TZ in seconds
}

// window works out the current period's bounds, aligned to whole local hours
// or days so every bar covers a full bucket.
func (q statsQuery) window() (from, to, off int64) {
	off = int64(q.TZ) * 60
	b := int64(q.Range.Bucket / time.Second)
	now := q.Now.Unix()
	cur := (now+off)/b*b - off // start of the current local bucket
	from = cur - int64(q.Range.Span-1)*b
	return from, cur + b, off
}

func (s *Store) Stats(q statsQuery) (*Stats, error) {
	from, to, off := q.window()
	st := &Stats{From: from, To: to, Range: q.Range, Offset: off}
	where := `bot = 0 AND at >= ? AND at < ?`
	args := []any{from, to}
	if q.LinkID != 0 {
		where += ` AND link_id = ?`
		args = append(args, q.LinkID)
	}
	one := func(dst *int, query string, a ...any) error {
		return s.db.QueryRow(query, a...).Scan(dst)
	}

	// Totals, and the same for the period just before, for the deltas.
	if err := s.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT NULLIF(visitor, '')) FROM clicks WHERE `+where, args...).
		Scan(&st.Clicks, &st.Visitors); err != nil {
		return nil, err
	}
	span := to - from
	prev := append([]any{from - span, from}, args[2:]...)
	if err := s.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT NULLIF(visitor, '')) FROM clicks WHERE `+where, prev...).
		Scan(&st.PrevClicks, &st.PrevVisitors); err != nil {
		return nil, err
	}
	botWhere := strings.Replace(where, "bot = 0", "bot = 1", 1)
	if err := one(&st.Bots, `SELECT COUNT(*) FROM clicks WHERE `+botWhere, args...); err != nil {
		return nil, err
	}

	// Time series, zero-filled.
	b := int64(q.Range.Bucket / time.Second)
	counts := map[int64]int{}
	rows, err := s.db.Query(`SELECT (at + ?) / ? AS k, COUNT(*) FROM clicks WHERE `+where+` GROUP BY k`, append([]any{off, b}, args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k int64
		var n int
		rows.Scan(&k, &n)
		counts[k] = n
	}
	rows.Close()
	for t := from; t < to; t += b {
		st.Series = append(st.Series, statPoint{Start: t, N: counts[(t+off)/b]})
	}

	// Weekday x hour. Day 0 of the epoch was a Thursday, so +3 makes Monday 0.
	rows, err = s.db.Query(`SELECT ((at + ?) / 86400 + 3) % 7 AS d, ((at + ?) / 3600) % 24 AS h, COUNT(*) FROM clicks WHERE `+where+` GROUP BY d, h`,
		append([]any{off, off}, args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d, h, n int
		rows.Scan(&d, &h, &n)
		if d >= 0 && d < 7 && h >= 0 && h < 24 {
			st.Heat[d][h] = n
			st.HeatMax = max(st.HeatMax, n)
		}
	}
	rows.Close()

	top := func(col string, limit int) ([]statRow, error) {
		rows, err := s.db.Query(`SELECT `+col+` AS k, COUNT(*) AS n FROM clicks WHERE `+where+` GROUP BY k ORDER BY n DESC, k LIMIT ?`,
			append(args, limit)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []statRow{}
		for rows.Next() {
			var r statRow
			if err := rows.Scan(&r.Key, &r.N); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	if st.Referrers, err = top("ref_host", 8); err != nil {
		return nil, err
	}
	for i := range st.Referrers {
		st.Referrers[i].Label = or(st.Referrers[i].Key, "Direct / none")
	}
	if st.GeoCountries, err = top("country", 400); err != nil {
		return nil, err
	}
	for i := range st.GeoCountries {
		st.GeoCountries[i].Label = countryName(st.GeoCountries[i].Key)
	}
	st.Countries = st.GeoCountries[:min(8, len(st.GeoCountries))]
	// Cities are grouped with their country, since names repeat (Hyderabad
	// is in India and Pakistan). Clicks with no city are left out, so the
	// list can add up to less than the total.
	rows, err = s.db.Query(`SELECT city, country, COUNT(*) AS n FROM clicks WHERE `+where+` AND city != '' AND country != ''
		GROUP BY city, country ORDER BY n DESC, city LIMIT 8`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var city, country string
		var r statRow
		if err := rows.Scan(&city, &country, &r.N); err != nil {
			rows.Close()
			return nil, err
		}
		r.Key, r.Label = city+", "+country, placeName(country, city)
		st.Cities = append(st.Cities, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if st.Devices, err = top("device", 4); err != nil {
		return nil, err
	}
	for i := range st.Devices {
		st.Devices[i].Label = titleCase(or(st.Devices[i].Key, "unknown"))
	}
	if st.Browsers, err = top("browser", 6); err != nil {
		return nil, err
	}
	if st.OSes, err = top("os", 6); err != nil {
		return nil, err
	}
	for _, rs := range [][]statRow{st.Browsers, st.OSes} {
		for i := range rs {
			rs[i].Label = or(rs[i].Key, "Unknown")
		}
	}

	if q.LinkID == 0 {
		rows, err := s.db.Query(`SELECT l.id, l.host, l.path, COUNT(*) AS n FROM clicks c JOIN links l ON l.id = c.link_id
			WHERE c.bot = 0 AND c.at >= ? AND c.at < ? GROUP BY l.id ORDER BY n DESC LIMIT 8`, from, to)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r statRow
			var host, path string
			rows.Scan(&r.ID, &host, &path, &r.N)
			r.Label = Link{Host: host, Path: path}.URL()
			st.Links = append(st.Links, r)
		}
		rows.Close()
	}

	rq := `SELECT c.at, c.referrer, c.country, c.city, c.ref_host, c.device, c.browser, c.os, l.id, l.host, l.path
		FROM clicks c JOIN links l ON l.id = c.link_id WHERE c.bot = 0`
	var rargs []any
	if q.LinkID != 0 {
		rq += ` AND c.link_id = ?`
		rargs = append(rargs, q.LinkID)
	}
	rows, err = s.db.Query(rq+` ORDER BY c.at DESC LIMIT 12`, rargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rc recentClick
		var host, path string
		rows.Scan(&rc.At, &rc.Referrer, &rc.Country, &rc.City, &rc.RefHost, &rc.Device, &rc.Browser, &rc.OS, &rc.LinkID, &host, &path)
		rc.LinkURL = Link{Host: host, Path: path}.URL()
		st.Recent = append(st.Recent, rc)
	}
	return st, rows.Err()
}

// Sparklines returns each link's daily clicks for the last `days` local
// days, oldest first, in one grouped query rather than one per row.
func (s *Store) Sparklines(days int, now time.Time, tz int) (map[int64][]int, error) {
	off := int64(tz) * 60
	today := (now.Unix() + off) / 86400
	from := (today-int64(days-1))*86400 - off
	rows, err := s.db.Query(`SELECT link_id, (at + ?) / 86400 AS d, COUNT(*) FROM clicks WHERE bot = 0 AND at >= ? GROUP BY link_id, d`, off, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int{}
	for rows.Next() {
		var id, d int64
		var n int
		if err := rows.Scan(&id, &d, &n); err != nil {
			return nil, err
		}
		i := int(d - (today - int64(days-1)))
		if i < 0 || i >= days {
			continue
		}
		if out[id] == nil {
			out[id] = make([]int, days)
		}
		out[id][i] = n
	}
	return out, rows.Err()
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// titleCase capitalises the first letter of a message, unless the message
// opens with a host or URL ("milind.dev/ade already exists"), which keeps
// its case.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	first, _, _ := strings.Cut(s, " ")
	if strings.ContainsAny(first, "./:") {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// delta formats the change against the previous period: "+12%", "-3%", or
// "new" when there was nothing before.
func delta(cur, prev int) (string, int) {
	switch {
	case prev == 0 && cur == 0:
		return "", 0
	case prev == 0:
		return "new", 1
	}
	pct := float64(cur-prev) / float64(prev) * 100
	sign := 0
	if pct >= 0.5 {
		sign = 1
	} else if pct <= -0.5 {
		sign = -1
	}
	return fmt.Sprintf("%+.0f%%", pct), sign
}

func countryName(code string) string {
	if code == "" {
		return "Unknown"
	}
	if n, ok := countries[strings.ToUpper(code)]; ok {
		return n
	}
	return strings.ToUpper(code)
}

// placeName is "Pune, India", or just the country when there is no city.
func placeName(country, city string) string {
	if city == "" || country == "" {
		return countryName(country)
	}
	return city + ", " + countryName(country)
}

// linkStat is one link's row in the per-link table on the Analytics page.
type linkStat struct {
	ID         int64
	URL        string
	Target     string
	State      string
	Clicks     int
	Visitors   int
	PrevClicks int
	Series     []int // one count per bucket of the range, oldest first
}

// PerLink returns every link's clicks, visitors and trend for the window,
// busiest first. Three grouped queries cover all links at once, however
// many there are.
func (s *Store) PerLink(q statsQuery) ([]linkStat, error) {
	from, to, off := q.window()
	b := int64(q.Range.Bucket / time.Second)
	links, err := s.Links()
	if err != nil {
		return nil, err
	}
	byID := map[int64]*linkStat{}
	out := make([]linkStat, len(links))
	for i, l := range links {
		out[i] = linkStat{ID: l.ID, URL: l.URL(), Target: l.Target, State: l.State(), Series: make([]int, q.Range.Span)}
		byID[l.ID] = &out[i]
	}

	rows, err := s.db.Query(`SELECT link_id, COUNT(*), COUNT(DISTINCT NULLIF(visitor, '')) FROM clicks
		WHERE bot = 0 AND at >= ? AND at < ? GROUP BY link_id`, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var n, v int
		rows.Scan(&id, &n, &v)
		if ls := byID[id]; ls != nil {
			ls.Clicks, ls.Visitors = n, v
		}
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT link_id, COUNT(*) FROM clicks WHERE bot = 0 AND at >= ? AND at < ? GROUP BY link_id`, from-(to-from), from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var n int
		rows.Scan(&id, &n)
		if ls := byID[id]; ls != nil {
			ls.PrevClicks = n
		}
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT link_id, (at + ?) / ? AS k, COUNT(*) FROM clicks WHERE bot = 0 AND at >= ? AND at < ? GROUP BY link_id, k`,
		off, b, from, to)
	if err != nil {
		return nil, err
	}
	first := (from + off) / b
	for rows.Next() {
		var id, k int64
		var n int
		rows.Scan(&id, &k, &n)
		if ls := byID[id]; ls != nil {
			if i := int(k - first); i >= 0 && i < len(ls.Series) {
				ls.Series[i] = n
			}
		}
	}
	rows.Close()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Clicks != out[j].Clicks {
			return out[i].Clicks > out[j].Clicks
		}
		return out[i].PrevClicks > out[j].PrevClicks
	})
	return out, nil
}
