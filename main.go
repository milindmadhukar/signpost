// signpost: short links and redirects for any domain behind Traefik.
//
// It keeps links in SQLite, answers the redirects itself (so it can count
// clicks), and tells Traefik to send exactly those paths its way, either by
// serving dynamic config for Traefik's HTTP provider to poll, or by writing it
// into the file provider's directory. Every other path on the host stays with
// whatever already serves it.
package main

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	DB              string
	Listen          string // public redirects, what Traefik forwards links to
	AdminListen     string // UI, API and /traefik.yml
	Token           string
	AdminHost       string
	TrustRemoteUser bool
	UpstreamURL     string
	ServiceName     string
	EntryPoints     []string
	CertResolver    string
	TraefikFile     string
	ProbeAddr       string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	return Config{
		DB:              env("SIGNPOST_DB", "signpost.db"),
		Listen:          env("SIGNPOST_LISTEN", ":8080"),
		AdminListen:     env("SIGNPOST_ADMIN_LISTEN", ":8081"),
		Token:           os.Getenv("SIGNPOST_TOKEN"),
		AdminHost:       strings.ToLower(os.Getenv("SIGNPOST_ADMIN_HOST")),
		TrustRemoteUser: env("SIGNPOST_TRUST_REMOTE_USER", "false") == "true",
		UpstreamURL:     env("SIGNPOST_UPSTREAM", "http://signpost:8080"),
		ServiceName:     env("SIGNPOST_SERVICE", "signpost"),
		EntryPoints:     strings.Split(env("SIGNPOST_ENTRYPOINTS", "websecure"), ","),
		CertResolver:    os.Getenv("SIGNPOST_CERT_RESOLVER"),
		TraefikFile:     os.Getenv("SIGNPOST_TRAEFIK_FILE"),
		ProbeAddr:       os.Getenv("SIGNPOST_PROBE_ADDR"),
	}
}

const usage = `signpost: short links and redirects for any domain behind Traefik

server
  signpost serve

client (talks to SIGNPOST_URL with SIGNPOST_TOKEN)
  signpost ls
  signpost add <host/path> <target> [-301|-302|-307|-308] [-drop-query] [-note text] [-force]
  signpost set <host/path|id> <target>
  signpost rm  <host/path|id>
  signpost clicks <host/path|id>
  signpost check <host/path|id>
  signpost domains
  signpost domain <host> [-resolver name] [-fallback url|-no-fallback]
  signpost domain-rm <host>

  <host/path> may end in /* to send every path on that host to <target>.
  Targets and fallbacks may contain {path}, replaced with the request path.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(loadConfig())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = runClient(os.Args[1], os.Args[2:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "signpost:", err)
		os.Exit(1)
	}
}
