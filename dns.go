package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Cloudflare's published edge ranges (cloudflare.com/ips). A proxied record
// resolves to these, never to the origin, so "points at Cloudflare" plus a
// successful round trip is as good as "points here".
var cloudflareRanges = mustPrefixes(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// publicResolver asks 1.1.1.1 directly rather than the container's resolver,
// so the answer is what the internet sees, not Docker's embedded DNS or a
// stale cache on the box.
var publicResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, network, "1.1.1.1:53")
	},
}

const wellKnown = "/.well-known/signpost/"

// checkDomain classifies a host's DNS, then proves the round trip by fetching
// the domain's well-known token over the public internet. The verdict only
// counts as reachable when that fetch comes back with this instance's token.
func (a *App) checkDomain(host string) {
	d, err := a.store.Domain(host)
	if err != nil {
		return
	}
	state, msg := a.dnsVerdict(d)
	a.store.SetDNS(host, state, msg)
}

func (a *App) dnsVerdict(d Domain) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cname, _ := publicResolver.LookupCNAME(ctx, d.Host)
	addrs, err := publicResolver.LookupNetIP(ctx, "ip", d.Host)
	if err != nil || len(addrs) == 0 {
		return "missing", "no A/AAAA records on public DNS. Add one pointing at this box (or proxied through Cloudflare)"
	}
	var here, cf, other []string
	for _, ip := range addrs {
		ip = ip.Unmap()
		switch {
		case a.isOurs(ip):
			here = append(here, ip.String())
		case inAny(ip, cloudflareRanges):
			cf = append(cf, ip.String())
		default:
			other = append(other, ip.String())
		}
	}
	via := ""
	if c := strings.TrimSuffix(cname, "."); c != "" && c != d.Host {
		via = " via CNAME " + c
	}

	if len(other) > 0 {
		return "elsewhere", fmt.Sprintf("resolves to %s%s, which is not this box. Links here will not work until DNS moves", strings.Join(other, ", "), via)
	}

	// Records look right; now prove it end to end.
	if err := a.roundTrip(d); err != nil {
		kind := "points at this box"
		if len(cf) > 0 {
			kind = "is proxied through Cloudflare"
		}
		return "error", fmt.Sprintf("DNS %s%s, but the round trip failed: %v", kind, via, err)
	}
	if len(cf) > 0 {
		return "proxied", "proxied through Cloudflare" + via + ", and it reaches signpost"
	}
	return "ok", "points straight at this box (" + strings.Join(here, ", ") + ")" + via + ", and it reaches signpost"
}

func (a *App) isOurs(ip netip.Addr) bool {
	for _, p := range a.cfg.PublicIPs {
		if p == ip {
			return true
		}
	}
	return false
}

func inAny(ip netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *App) roundTrip(d Domain) error {
	var lastErr error
	// Traefik needs one poll to learn the route and a new hostname may need a
	// certificate first, so give it a little while before calling it broken.
	for i := 0; i < 8; i++ {
		if i > 0 {
			time.Sleep(time.Duration(3*i) * time.Second)
		}
		resp, err := publicClient.Get("https://" + d.Host + wellKnown + d.Token)
		if err != nil {
			lastErr = shortErr(err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && string(body) == "signpost:"+d.Token {
			return nil
		}
		lastErr = fmt.Errorf("got %d from something that is not this signpost", resp.StatusCode)
	}
	return lastErr
}

// dnsLoop re-checks every domain now and then, so a record that moves away
// (or finally lands) shows up without anyone pressing a button.
func (a *App) dnsLoop() {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		if ds, err := a.store.Domains(); err == nil {
			for _, d := range ds {
				a.checkDomain(d.Host)
			}
		}
		<-t.C
	}
}

// shortErr turns Go's `Get "https://...": dial tcp ...: i/o timeout` into
// something that fits under a badge.
func shortErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("timed out")
	case strings.Contains(err.Error(), "certificate"):
		return errors.New("TLS certificate not valid for this host yet")
	case strings.Contains(err.Error(), "connection refused"):
		return errors.New("connection refused")
	case strings.Contains(err.Error(), "no such host"):
		return errors.New("host does not resolve")
	}
	return err
}
