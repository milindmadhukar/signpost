package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Link passwords are stored as pbkdf2$<iterations>$<salt>$<key>, base64url.
// PBKDF2-SHA256 at OWASP's 600k iterations, from the standard library, so no
// extra dependency.
const pbkdf2Iter = 600_000

var b64 = base64.RawURLEncoding

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iter, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// hashSlots caps how many password checks run at once. Each costs a few
// hundred milliseconds of CPU by design, so without a cap a burst of guesses
// would slow every redirect on the box.
var hashSlots = make(chan struct{}, 2)

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err1 := strconv.Atoi(parts[1])
	salt, err2 := b64.DecodeString(parts[2])
	want, err3 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil || iter < 1 {
		return false
	}
	hashSlots <- struct{}{}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	<-hashSlots
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

const unlockTTL = 24 * time.Hour

// unlockToken binds a cookie to one link and one password hash: changing or
// removing the password changes the hash, so every old cookie stops working.
func (a *App) unlockToken(id int64, passHash string, exp int64) string {
	m := hmac.New(sha256.New, a.secret)
	fmt.Fprintf(m, "unlock|%d|%s|%d", id, passHash, exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func unlockCookie(id int64) string { return "sp_unlock_" + strconv.FormatInt(id, 10) }

func (a *App) unlocked(r *http.Request, rt *route) bool {
	c, err := r.Cookie(unlockCookie(rt.id))
	if err != nil {
		return false
	}
	expS, _, ok := strings.Cut(c.Value, ".")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.unlockToken(rt.id, rt.passHash, exp)))
}

// limiter is a token bucket per key, here (client IP, link): a few guesses a
// minute, refilling slowly. Old buckets are swept so the map cannot grow
// without bound.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perMinute, burst int) *limiter {
	return &limiter{buckets: map[string]*bucket{}, rate: float64(perMinute) / 60, burst: float64(burst)}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) > 10000 {
			l.sweepLocked(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) sweepLocked(now time.Time) {
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}

// clientIP prefers what the edge saw. signpost only ever sits behind Traefik
// (and maybe Cloudflare), so these headers are set by infrastructure, not by
// the visitor.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// The pages a visitor can meet on the public side: the password prompt and
// "this link is gone". They are self-contained (inline CSS, no assets) since
// the redirect listener serves nothing else.
var publicPage = template.Must(template.New("public").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--bg:#fafafa;--card:#fff;--fg:#18181b;--mute:#71717a;--line:#e4e4e7;--btn:#18181b;--btnfg:#fff;--err:#b42318}
@media (prefers-color-scheme:dark){:root{--bg:#09090b;--card:#121214;--fg:#fafafa;--mute:#a1a1aa;--line:#27272a;--btn:#fafafa;--btnfg:#18181b;--err:#f97066}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);
font:15px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;padding:24px}
.card{width:100%;max-width:380px;background:var(--card);border:1px solid var(--line);border-radius:14px;padding:28px}
h1{font-size:18px;font-weight:600;margin:0 0 6px;letter-spacing:-.01em}p{margin:0 0 20px;color:var(--mute);font-size:14px}
.host{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;color:var(--fg)}
input{width:100%;font:inherit;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit}
input:focus{outline:2px solid #3987e5;outline-offset:1px}
button{margin-top:12px;width:100%;font:inherit;font-weight:500;padding:10px 12px;border:0;border-radius:8px;background:var(--btn);color:var(--btnfg);cursor:pointer}
.err{color:var(--err);font-size:13px;margin:10px 0 0}
</style></head><body><main class="card">
{{if .Gone}}<h1>This link is no longer active</h1><p><span class="host">{{.Link}}</span> has expired or been switched off.</p>
{{else}}<h1>This link is password protected</h1><p>Enter the password to continue to <span class="host">{{.Link}}</span>.</p>
<form method="post" autocomplete="off"><input type="password" name="password" placeholder="Password" aria-label="Password" required autofocus>
{{if .Err}}<p class="err" role="alert">{{.Err}}</p>{{end}}<button type="submit">Continue</button></form>{{end}}
</main></body></html>`))

type publicView struct {
	Title, Link, Err string
	Gone             bool
}

func servePublic(w http.ResponseWriter, code int, v publicView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.WriteHeader(code)
	publicPage.Execute(w, v)
}
