package main

import (
	"regexp"
	"strings"
)

// UA is the little a click's User-Agent is reduced to. A full UA database
// would be overkill: the dashboard only ever groups by these coarse buckets.
type UA struct {
	Browser string
	OS      string
	Device  string // desktop, mobile, tablet, bot
	Bot     bool
}

// botRe catches crawlers and, just as important for a link shortener, the
// link-preview fetchers that chat apps send the moment a link is pasted.
// Those are not people, so they must not count as clicks.
var botRe = regexp.MustCompile(`(?i)bot\b|bot/|crawl|spider|slurp|preview|facebookexternalhit|facebookcatalog|meta-externalagent|embedly|` +
	`whatsapp/|telegram|discord|slack|skypeuripreview|vkshare|pinterest|redditbot|linkedinbot|mastodon|bluesky|` +
	`curl/|wget/|python-requests|python-urllib|aiohttp|httpx|go-http-client|okhttp|java/|libwww|httpclient|axios/|node-fetch|undici|` +
	`headless|lighthouse|pagespeed|gtmetrix|pingdom|uptime|monitor|statuscake|scanner|zgrab|nmap|masscan|censys|expanse|` +
	`google-inspectiontool|googleother|bingpreview|yandex|baidu|duckduckbot|applebot|petalbot|semrush|ahrefs|mj12|dotbot|bytespider|gptbot|claudebot|perplexity|ccbot`)

func parseUA(s string) UA {
	if strings.TrimSpace(s) == "" || botRe.MatchString(s) {
		return UA{Browser: "Bot", OS: "Bot", Device: "bot", Bot: true}
	}
	l := strings.ToLower(s)
	u := UA{Browser: "Other", OS: "Other", Device: "desktop"}

	switch {
	case strings.Contains(l, "iphone"), strings.Contains(l, "ipod"):
		u.OS, u.Device = "iOS", "mobile"
	case strings.Contains(l, "ipad"):
		u.OS, u.Device = "iOS", "tablet"
	case strings.Contains(l, "android"):
		u.OS, u.Device = "Android", "mobile"
		if !strings.Contains(l, "mobile") {
			u.Device = "tablet"
		}
	case strings.Contains(l, "windows"):
		u.OS = "Windows"
	case strings.Contains(l, "cros"):
		u.OS = "ChromeOS"
	case strings.Contains(l, "macintosh"), strings.Contains(l, "mac os x"):
		u.OS = "macOS"
	case strings.Contains(l, "linux"):
		u.OS = "Linux"
	}
	if u.Device == "desktop" && (strings.Contains(l, "mobi") || strings.Contains(l, "tablet")) {
		u.Device = "mobile"
		if strings.Contains(l, "tablet") {
			u.Device = "tablet"
		}
	}

	// Order matters: in-app browsers and Chromium forks also say Chrome and
	// Safari, so the specific names go first.
	switch {
	case strings.Contains(l, "instagram"):
		u.Browser = "Instagram"
	case strings.Contains(l, "fban") || strings.Contains(l, "fbav") || strings.Contains(l, "fb_iab"):
		u.Browser = "Facebook"
	case strings.Contains(l, "musical_ly") || strings.Contains(l, "bytedancewebview") || strings.Contains(l, "tiktok"):
		u.Browser = "TikTok"
	case strings.Contains(l, "edg/") || strings.Contains(l, "edga/") || strings.Contains(l, "edgios/"):
		u.Browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		u.Browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		u.Browser = "Samsung Internet"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios/"):
		u.Browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/") || strings.Contains(l, "chromium/"):
		u.Browser = "Chrome"
	case strings.Contains(l, "safari/"):
		u.Browser = "Safari"
	}
	return u
}
