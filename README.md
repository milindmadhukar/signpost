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
- Short links: leave the path empty and signpost picks a free 7-character
  slug, with no look-alike characters (`0 O 1 l I`)
- `{path}` in targets: `go.test/*` to `https://luma.com/{path}`
- Per-domain fallback for hosts signpost owns outright (unknown paths go to
  e.g. `https://www.plusxhub.com/{path}`)
- 301/302/307/308, query strings passed through by default
- Expiry (a 410 page from then on), an on/off switch, tags and notes
- Password-protected links: visitors get a prompt (PBKDF2-SHA256, rate
  limited per IP and link) and a 24-hour cookie that dies with a password
  change. Only clicks that get through are counted.
- Bulk redirects: CSV import with a dry-run preview (validation, duplicates,
  live conflict checks) and an all-or-nothing commit, export in the same
  format, and bulk enable/disable/redirect type/tag/delete in the table
- Click analytics: clicks and unique visitors against the previous period,
  a time series, a weekday-by-hour heatmap in your time zone, and top
  referrers, countries, devices, browsers, OSes and links. Link previews
  and crawlers (Slack, WhatsApp, facebookexternalhit, Googlebot, curl…) are
  recorded but not counted. Visitors are a daily-rotating hash; no IP is
  stored. Country comes from Cloudflare's `CF-IPCountry`.
- DNS checks when a domain is connected: records are read from 1.1.1.1 and
  classified (straight here, proxied by Cloudflare, elsewhere, missing), then
  proven with a real round trip to `https://<host>/.well-known/signpost/<token>`,
  since a proxied record never shows this box's IP. Re-run every 6 hours.
- Dashboard in Go templates + htmx, Tailwind and a little TypeScript, all
  compiled at build time and served from the binary (no CDN): a sidebar,
  a page per link, light/dark/system themes, keyboard shortcuts (`/`, `n`).
  Behind Authelia. A CLI in the same binary.
- Everything in one SQLite file, `/data/signpost.db`, upgraded in place

## Performance

- Every link lives in memory in an immutable table behind an atomic
  pointer, so a redirect never touches the database or takes a lock. The
  table is rebuilt on every change made through signpost, and within 2s of
  a change made to the file from anywhere else (`PRAGMA data_version`), with
  a full rebuild every 5 minutes as a backstop. Click counts stay out of it,
  so nothing in the cache goes stale as traffic arrives.
- Clicks are queued and written in batches (up to 500, or once a second):
  about 20x cheaper per click than one transaction each. A full queue drops
  the click rather than slow the redirect, and says so in the log.
- `/traefik.yml` answers Traefik's 5-second polls with a 304 when nothing
  changed, and a host with a `/*` link gets one router however many short
  links it carries.

## CLI

```
export SIGNPOST_URL=https://links.milind.dev SIGNPOST_TOKEN=...
signpost ls
signpost add plusxhub.com/ade https://amsterdamdanceevent.com/plusxhub -note "ADE"
signpost add -short go.milind.dev https://example.com/a/long/url -tags launch
signpost add 'events.plusxhub.com/*' https://luma.com/qwo2votr -301
signpost add milind.dev/press https://drive.google.com/... -password hunter2
signpost set plusxhub.com/ade https://new.target
signpost clicks plusxhub.com/ade
signpost rm plusxhub.com/ade
signpost import links.csv -dry-run        # then without -dry-run
signpost export > links.csv
signpost domain plusxhub.com -fallback 'https://www.plusxhub.com/{path}'
```

CSV columns are `source,target,status,note,tags`; a header row is optional,
and an empty path in `source` (`go.milind.dev/`) generates a slug.

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
and the TypeScript with [esbuild's standalone binary](https://www.npmjs.com/package/@esbuild/linux-x64)
(`TAILWIND=` and `ESBUILD=` point at them if they are not on PATH), then
serves on :8080/:8081. `make demo` first fills an empty `dev.db` with links
and 90 days of clicks. `make test` runs vet and the tests; `make check-ts`
type-checks the client code with tsc (CI runs both).

## Deploying

See `docker-compose.yml`.
