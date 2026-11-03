package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Router priorities. Docker-label routers get a priority equal to their rule
// length (milind.dev's site router sits at 18), so an exact-path link at 10000
// always wins its one path and leaves every other path to the site. A whole
// host link beats nothing but the site's own catch-all, and a domain fallback
// sits at 1 so any other router on that host still wins.
const (
	prioLink     = 10000
	prioHostLink = 9000
	prioFallback = 1
)

var (
	hostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	// Kept to characters that are safe inside a Traefik backtick rule and do
	// not need percent-encoding in a URL.
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._~\-/]*$`)
)

type traefikRouter struct {
	Rule        string      `yaml:"rule"`
	EntryPoints []string    `yaml:"entryPoints,omitempty"`
	Priority    int         `yaml:"priority"`
	Service     string      `yaml:"service"`
	TLS         *traefikTLS `yaml:"tls"` // always present: an empty tls: {} still means "HTTPS router"
}

type traefikTLS struct {
	CertResolver string `yaml:"certResolver,omitempty"`
}

type traefikConfig struct {
	HTTP struct {
		Routers  map[string]traefikRouter `yaml:"routers"`
		Services map[string]any           `yaml:"services"`
	} `yaml:"http"`
}

// Render builds the Traefik dynamic config for every link and fallback. The
// output is a pure function of the database, so it is always rewritten whole.
func Render(cfg Config, domains []Domain, links []Link) ([]byte, error) {
	var tc traefikConfig
	tc.HTTP.Routers = map[string]traefikRouter{}
	tc.HTTP.Services = map[string]any{
		cfg.ServiceName: map[string]any{
			"loadBalancer": map[string]any{
				"servers": []map[string]string{{"url": cfg.UpstreamURL}},
			},
		},
	}
	resolver := map[string]string{}
	for _, d := range domains {
		resolver[d.Host] = d.CertResolver
	}
	router := func(host, rule string, prio int) traefikRouter {
		r := traefikRouter{Rule: rule, EntryPoints: cfg.EntryPoints, Priority: prio, Service: cfg.ServiceName}
		r.TLS = &traefikTLS{CertResolver: resolver[host]}
		return r
	}
	for _, l := range links {
		name := fmt.Sprintf("%s-link-%d", cfg.ServiceName, l.ID)
		if l.Path == "*" {
			tc.HTTP.Routers[name] = router(l.Host, fmt.Sprintf("Host(`%s`)", l.Host), prioHostLink)
		} else {
			tc.HTTP.Routers[name] = router(l.Host, fmt.Sprintf("Host(`%s`) && (Path(`%s`) || Path(`%s/`))", l.Host, l.Path, l.Path), prioLink)
		}
	}
	for _, d := range domains {
		if d.Fallback == "" {
			continue
		}
		name := cfg.ServiceName + "-fallback-" + strings.ReplaceAll(d.Host, ".", "-")
		tc.HTTP.Routers[name] = router(d.Host, fmt.Sprintf("Host(`%s`)", d.Host), prioFallback)
	}

	body, err := yaml.Marshal(tc)
	if err != nil {
		return nil, err
	}
	header := "# Written by signpost. Every change in its UI rewrites this file whole,\n# so edits made here by hand are lost. Manage links in signpost instead.\n"
	return append([]byte(header), body...), nil
}

// WriteAtomic replaces path in one rename, so Traefik's watcher never reads a
// half-written file. The temp name ends in .tmp, which the file provider
// ignores, and lives in the same directory so the rename stays on one fs.
func WriteAtomic(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
