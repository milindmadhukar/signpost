package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

func noFollow(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

var publicClient = &http.Client{Timeout: 8 * time.Second, CheckRedirect: noFollow}

// viaTraefik dials Traefik directly (SIGNPOST_PROBE_ADDR) while still sending
// the real SNI and Host. That tests routing and the certificate without
// depending on DNS, so a link can be told apart as "Traefik is fine, DNS is not".
func (a *App) viaTraefik() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: noFollow,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.DialContext(ctx, network, a.cfg.ProbeAddr)
			},
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

func probeURL(l Link) string {
	if l.Path == "*" {
		return "https://" + l.Host + "/signpost-probe"
	}
	return "https://" + l.Host + l.Path
}

// conflict asks the live internet whether the path is already taken, before
// signpost claims it. Anything but a 404 from something other than signpost
// counts, so /about on the portfolio can never be shadowed by accident.
func (a *App) conflict(ctx context.Context, l Link) string {
	u := probeURL(l)
	if l.Path == "*" {
		u = "https://" + l.Host + "/"
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("X-Signpost-Probe", a.probeID)
	resp, err := publicClient.Do(req)
	if err != nil {
		return "" // nothing answers, so nothing to shadow
	}
	resp.Body.Close()
	if resp.Header.Get("X-Signpost") != "" || resp.StatusCode == http.StatusNotFound {
		return ""
	}
	if l.Path == "*" {
		return fmt.Sprintf("%s already serves something (%d at /), and a /* link would take over the whole host", l.Host, resp.StatusCode)
	}
	return fmt.Sprintf("%s already answers %d", u, resp.StatusCode)
}

func (a *App) probe(c *http.Client, l Link) error {
	req, _ := http.NewRequest(http.MethodGet, probeURL(l), nil)
	req.Header.Set("X-Signpost-Probe", a.probeID)
	resp, err := c.Do(req)
	if err != nil {
		return shortErr(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Signpost-Link"); got != strconv.FormatInt(l.ID, 10) {
		return fmt.Errorf("answered %d by something other than this link", resp.StatusCode)
	}
	if resp.StatusCode != l.Status {
		return fmt.Errorf("answered %d, want %d", resp.StatusCode, l.Status)
	}
	return nil
}

// verify polls until the link answers publicly. Traefik needs a poll interval
// to see the route, and a brand-new hostname needs a certificate, which can
// take a minute or two over DNS-01, hence the long patience.
func (a *App) verify(id int64) {
	var lastPub, lastTr error
	for i := 0; i < 36; i++ {
		time.Sleep(time.Duration(min(2+i, 10)) * time.Second)
		l, err := a.store.Link(id)
		if err != nil {
			return // deleted meanwhile
		}
		if lastPub = a.probe(publicClient, l); lastPub == nil {
			a.store.SetCheck(id, "live", "answers publicly")
			return
		}
		if a.cfg.ProbeAddr != "" {
			lastTr = a.probe(a.viaTraefik(), l)
		}
	}
	switch {
	case a.cfg.ProbeAddr != "" && lastTr == nil:
		a.store.SetCheck(id, "dns", "Traefik routes it, but the public address does not reach this box: "+lastPub.Error())
	default:
		a.store.SetCheck(id, "error", lastPub.Error())
	}
}
