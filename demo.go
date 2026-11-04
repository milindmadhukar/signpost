package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// demoSeed fills a development database with believable links and about 90
// days of clicks, so the dashboard and its charts can be looked at (and
// screenshotted) without real traffic. It refuses a database that already
// has links, so it can never be pointed at production data by accident.
func demoSeed(args []string) error {
	fs := flag.NewFlagSet("demo-seed", flag.ExitOnError)
	dbPath := fs.String("db", "dev.db", "SQLite file to fill")
	clicks := fs.Int("clicks", 15000, "roughly how many clicks to generate")
	fs.Parse(args)

	st, err := OpenStore(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM links`).Scan(&n)
	if n > 0 {
		return fmt.Errorf("%s already has %d links; demo-seed only fills an empty database", *dbPath, n)
	}

	now := time.Now()
	ago := func(d int) *int64 { t := now.AddDate(0, 0, -d).Unix(); return &t }
	soon := now.AddDate(0, 0, 12).Unix()
	past := now.AddDate(0, 0, -3).Unix()
	type demoLink struct {
		l      Link
		weight float64
		check  string
		msg    string
	}
	pw, _ := hashPassword("demo")
	demo := []demoLink{
		{Link{Host: "milind.dev", Path: "/ade", Target: "https://www.amsterdam-dance-event.nl/en/program/", Note: "ADE set announcement", Tags: []string{"events", "instagram"}}, 9, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/cv", Target: "https://milind.dev/files/resume-2026.pdf", Note: "Resume", Tags: []string{"personal"}}, 5, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/yt", Target: "https://www.youtube.com/@milindmadhukar", Status: 301, Tags: []string{"social"}}, 4, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/gh", Target: "https://github.com/milindmadhukar", Status: 301, Tags: []string{"social"}}, 3, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/mix-oct", Target: "https://soundcloud.com/milind/october-mix", Note: "October mix", Tags: []string{"music", "instagram"}}, 6, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/k3Fq9aR", Target: "https://docs.google.com/forms/d/e/1FAIpQLSf-demo/viewform", Note: "Feedback form", Tags: []string{"forms"}}, 2, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/press", Target: "https://drive.google.com/drive/folders/press-kit", Note: "Press kit, password protected", PassHash: pw}, 1, "live", "answers publicly"},
		{Link{Host: "milind.dev", Path: "/giveaway", Target: "https://instagram.com/p/C9xyz", Note: "Giveaway reel", Tags: []string{"instagram"}, ExpiresAt: &past}, 3, "live", "answers publicly"},
		{Link{Host: "plusxhub.com", Path: "/ade", Target: "https://amsterdamdanceevent.com/plusxhub", Note: "ADE partner page", Tags: []string{"events"}}, 7, "live", "answers publicly"},
		{Link{Host: "plusxhub.com", Path: "/tickets", Target: "https://luma.com/qwo2votr", Note: "Winter showcase tickets", Tags: []string{"events"}, ExpiresAt: &soon}, 8, "live", "answers publicly"},
		{Link{Host: "plusxhub.com", Path: "/discord", Target: "https://discord.gg/plusxhub", Status: 301, Tags: []string{"community"}}, 3, "live", "answers publicly"},
		{Link{Host: "plusxhub.com", Path: "/apply", Target: "https://tally.so/r/plusx-artists", Note: "Artist applications"}, 2, "dns", "Traefik routes it, but the public address does not reach this box: timed out"},
		{Link{Host: "events.plusxhub.com", Path: "*", Target: "https://luma.com/{path}", Status: 301, Note: "Every event path goes to Luma"}, 5, "live", "answers publicly"},
		{Link{Host: "go.milind.dev", Path: "/Xp4mT2q", Target: "https://www.notion.so/milind/Reading-list-2026-8f2c", Tags: []string{"notes"}}, 1.5, "live", "answers publicly"},
		{Link{Host: "go.milind.dev", Path: "/meet", Target: "https://cal.com/milind/30min", Note: "Booking link", Tags: []string{"personal"}}, 2.5, "live", "answers publicly"},
		{Link{Host: "go.milind.dev", Path: "/old-blog", Target: "https://milind.dev/blog", Disabled: true, Note: "Retired"}, 0.5, "live", "answers publicly"},
		{Link{Host: "go.milind.dev", Path: "/wip", Target: "https://staging.milind.dev/preview"}, 0, "pending", ""},
		{Link{Host: "go.milind.dev", Path: "/broken", Target: "https://example.com/gone"}, 0.2, "error", "answered 404 by something other than this link"},
	}
	for i := range demo {
		l := &demo[i].l
		if l.Status == 0 {
			l.Status = 302
		}
		l.KeepQuery = true
	}
	links := make([]Link, len(demo))
	for i, d := range demo {
		links[i] = d.l
	}
	created, err := st.CreateLinks(links, "")
	if err != nil {
		return err
	}
	born := make([]int64, len(created))
	for i, l := range created {
		st.SetCheck(l.ID, demo[i].check, demo[i].msg)
		born[i] = *ago(88 - i*4)
		st.db.Exec(`UPDATE links SET created_at = ?, updated_at = ? WHERE id = ?`, born[i], born[i], l.ID)
	}
	for _, h := range []string{"milind.dev", "plusxhub.com", "events.plusxhub.com", "go.milind.dev"} {
		st.SetDNS(h, "ok", "A record points at this box, and the round trip came back")
	}
	st.SetDNS("plusxhub.com", "proxied", "proxied by Cloudflare, and the round trip came back")
	st.SetDNS("go.milind.dev", "elsewhere", "AAAA 2606:50c0:8000::153 is not this box")

	r := rand.New(rand.NewPCG(uint64(now.UnixNano()), 7))
	pick := func(opts []string, weights []float64) string {
		total := 0.0
		for _, w := range weights {
			total += w
		}
		x := r.Float64() * total
		for i, w := range weights {
			if x < w {
				return opts[i]
			}
			x -= w
		}
		return opts[len(opts)-1]
	}
	refs := []string{"", "https://l.instagram.com/", "https://t.co/abc", "https://www.linkedin.com/feed/", "https://www.google.com/",
		"https://news.ycombinator.com/item?id=1", "https://www.reddit.com/r/edmproduction/", "https://web.whatsapp.com/", "https://discord.com/channels/1"}
	refW := []float64{30, 26, 8, 9, 7, 3, 4, 6, 4}
	// "" is a click no one could place, which the map reports separately.
	countriesW := []string{"IN", "NL", "US", "GB", "DE", "FR", "BE", "AE", "SG", "CA", "ES", "AU", "JP", "BR", "PL", "ZA", "MX", "KE", ""}
	countryW := []float64{34, 20, 14, 8, 6, 4, 3, 3, 2, 2, 2, 2, 1.5, 1.5, 1, 0.6, 0.6, 0.4, 2}
	uas := []string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Instagram 350.0.0.0 (iPhone15,3; iOS 18_1; en_US)",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.6; rv:132.0) Gecko/20100101 Firefox/132.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15",
		"Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/26.0 Chrome/122.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1",
		"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
		"WhatsApp/2.24.21.80 A",
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	}
	uaW := []float64{30, 18, 14, 12, 6, 4, 5, 4, 2, 2, 2, 1}
	// Evenings and weekends are busier; traffic grows over the period, with
	// a spike around a launch three weeks ago.
	hourW := []float64{2, 1.2, .8, .5, .4, .5, 1, 2, 3, 3.5, 3.6, 3.8, 4.2, 4, 3.8, 3.9, 4.3, 5, 6, 7, 7.5, 7, 5.5, 3.5}
	hours := make([]string, 24)
	for i := range hours {
		hours[i] = fmt.Sprint(i)
	}
	totalW := 0.0
	for _, d := range demo {
		totalW += d.weight
	}
	var evs []clickEvent
	secret, _ := st.Secret()
	app := &App{secret: secret}
	for day := 89; day >= 0; day-- {
		date := now.AddDate(0, 0, -day)
		growth := 0.45 + 0.55*float64(89-day)/89
		spike := 1 + 2.2*math.Exp(-math.Pow(float64(day-21), 2)/6)
		wk := 1.0
		if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
			wk = 1.25
		}
		perDay := float64(*clicks) / 90 / 0.78 * growth * spike * wk
		count := int(perDay * (0.85 + 0.3*r.Float64()))
		for range count {
			// pick a link by weight, never one created after this day
			x := r.Float64() * totalW
			idx := 0
			for i, d := range demo {
				if x < d.weight {
					idx = i
					break
				}
				x -= d.weight
			}
			l := created[idx]
			if demo[idx].weight == 0 {
				continue
			}
			var h int
			fmt.Sscan(pick(hours, hourW), &h)
			at := time.Date(date.Year(), date.Month(), date.Day(), h, r.IntN(60), r.IntN(60), 0, time.Local)
			if at.After(now) || at.Unix() < born[idx] {
				continue
			}
			ua := pick(uas, uaW)
			if r.IntN(3) > 0 { // returning visitors keep their device
				ua = uas[r.IntN(len(uas)-3)]
			}
			ref := pick(refs, refW)
			if parseUA(ua).Browser == "Instagram" {
				ref = "https://l.instagram.com/"
			}
			p := parseUA(ua)
			// A pool of returning visitors: most people click more than once.
			ip := fmt.Sprintf("10.0.%d.%d", r.IntN(8), r.IntN(250))
			evs = append(evs, clickEvent{l.ID, Click{
				At: at.Unix(), Referrer: ref, RefHost: refHost(ref), Country: pick(countriesW, countryW), UA: ua,
				Device: p.Device, Browser: p.Browser, OS: p.OS, Bot: p.Bot, Visitor: app.visitor(at, ip, ua),
			}})
		}
	}
	for i := 0; i < len(evs); i += 2000 {
		if err := st.RecordClicks(evs[i:min(i+2000, len(evs))]); err != nil {
			return err
		}
	}
	fmt.Printf("seeded %s: %d links, %d clicks (password for /press is \"demo\")\n", *dbPath, len(created), len(evs))
	return nil
}
