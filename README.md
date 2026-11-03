# signpost

Short links for any domain that already sits behind Traefik.

```
$ signpost add milind.dev/helloworld https://instagram.com/p/xyz
https://milind.dev/helloworld -> https://instagram.com/p/xyz (302)
```

`milind.dev` keeps serving the portfolio. Only `/helloworld` goes to signpost.

## How it works

signpost keeps links in SQLite and turns each one into a Traefik router that
matches one exact host and path:

```yaml
signpost-link-1:
  rule: Host(`milind.dev`) && (Path(`/helloworld`) || Path(`/helloworld/`))
  priority: 10000          # docker-label routers sit around 15-30
  service: signpost
```

Traefik polls that config from `GET /traefik.yml` through its
[HTTP provider](https://doc.traefik.io/traefik/reference/install-configuration/providers/others/http/),
or signpost can write it into the file provider's directory instead. The
request then lands on signpost, which counts the click and answers the
redirect. Any other path never reaches signpost, so the site behind it does
not know signpost exists.

Before claiming a path, signpost GETs it on the live internet. If anything
other than a 404 answers, it refuses (unless forced), so `/about` can never be
shadowed by accident. After adding a link it keeps probing until the link
answers publicly, and marks it `live`, `dns` (Traefik routes it but the public
address does not reach this box) or `error`.

### Why not a Traefik plugin, or Traefik's API?

- **The API is read-only.** It reports routers; it cannot create them.
- **A plugin is the wrong tool.** Plugins are middlewares, so catching bare
  paths would mean hanging one on the entrypoint, running interpreted Go
  (Yaegi) on every request to every service on the box. That needs a public
  GitHub repo, vendored deps and a Traefik restart on every plugin change,
  and there is nowhere for a UI or a database to live. A bug there is an
  outage for everything; a bug in signpost is an outage for short links.
- **The docker socket** would mean recreating a container per link.

## Features

- Exact paths (`milind.dev/ade`) and whole hosts (`events.plusxhub.com/*`)
- `{path}` in targets: `go.test/*` to `https://luma.com/{path}`
- Per-domain fallback for hosts signpost owns outright (unknown paths go to
  e.g. `https://www.plusxhub.com/{path}`)
- 301/302/307/308, query strings passed through by default
- Clicks with referrer, country (from Cloudflare's `CF-IPCountry`) and UA
- DNS checks when a domain is connected: records are read from 1.1.1.1 and
  classified (straight here, proxied by Cloudflare, elsewhere, missing), then
  proven with a real round trip to `https://<host>/.well-known/signpost/<token>`,
  since a proxied record never shows this box's IP. Re-run every 6 hours.
- Dashboard in Go templates + htmx + Tailwind (compiled at build time, no CDN),
  behind Authelia, and a CLI in the same binary
- Everything in one SQLite file, `/data/signpost.db`

## CLI

```
export SIGNPOST_URL=https://links.milind.dev SIGNPOST_TOKEN=...
signpost ls
signpost add plusxhub.com/ade https://amsterdamdanceevent.com/plusxhub -note "ADE"
signpost add 'events.plusxhub.com/*' https://luma.com/qwo2votr -301
signpost set plusxhub.com/ade https://new.target
signpost clicks plusxhub.com/ade
signpost rm plusxhub.com/ade
signpost domain plusxhub.com -fallback 'https://www.plusxhub.com/{path}'
```

## Config

| env | default | |
|---|---|---|
| `SIGNPOST_DB` | `/data/signpost.db` | SQLite file |
| `SIGNPOST_LISTEN` | `:8080` | redirects (what Traefik forwards links to) |
| `SIGNPOST_ADMIN_LISTEN` | `:8081` | UI, API, `/traefik.yml`, `/healthz` |
| `SIGNPOST_TOKEN` | | bearer token for the API |
| `SIGNPOST_TRUST_REMOTE_USER` | `false` | accept Authelia's `Remote-User` for the UI |
| `SIGNPOST_ADMIN_HOST` | | signpost refuses to put links on this host |
| `SIGNPOST_UPSTREAM` | `http://signpost:8080` | where the generated Traefik service points |
| `SIGNPOST_ENTRYPOINTS` | `websecure` | comma-separated |
| `SIGNPOST_CERT_RESOLVER` | | default resolver for new domains |
| `SIGNPOST_TRAEFIK_FILE` | | also write the config here (file provider mode) |
| `SIGNPOST_PROBE_ADDR` | | e.g. `traefik:443`, to probe through Traefik directly |
| `SIGNPOST_PUBLIC_IPS` | | comma-separated public IPs of the box, for the DNS check |

`/traefik.yml` is unauthenticated so Traefik can poll it over the docker
network. It holds hostnames and paths only; targets are not in it.

## Developing

`make dev` builds the CSS with the [Tailwind standalone CLI](https://github.com/tailwindlabs/tailwindcss/releases)
(`TAILWIND=/path/to/tailwindcss` if it is not on PATH) and serves on :8080/:8081.

## Deploying

See `docker-compose.yml`.
