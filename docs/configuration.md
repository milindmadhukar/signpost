# Configuration

signpost is configured entirely through environment variables. Everything
else, such as domains, fallbacks and the default host for new links, is set
in the dashboard or through the API and stored in the database.

## Server

| Variable | Default | Description |
|---|---|---|
| `SIGNPOST_DB` | `/data/signpost.db` in the image, `signpost.db` otherwise | The SQLite file. Created and migrated on start. |
| `SIGNPOST_GEOIP_DB` | DB-IP's country database in the image, none otherwise | A MaxMind-format (`.mmdb`) country database, used for clicks that arrive without Cloudflare's `CF-IPCountry`. Any file with `country.iso_code` works: DB-IP's [IP to Country Lite](https://db-ip.com/db/download/ip-to-country-lite) or MaxMind's GeoLite2 Country. If it cannot be opened, signpost logs a warning and carries on without it. |
| `SIGNPOST_LISTEN` | `:8080` | Address for redirects. This is what Traefik forwards links to. |
| `SIGNPOST_ADMIN_LISTEN` | `:8081` | Address for the dashboard, API, `/traefik.yml` and `/healthz`. |

## Authentication

| Variable | Default | Description |
|---|---|---|
| `SIGNPOST_TOKEN` | | Bearer token for the API and CLI. If empty, the API accepts only `Remote-User`. |
| `SIGNPOST_TRUST_REMOTE_USER` | `false` | Accept the `Remote-User` header set by forward auth (Authelia or similar) for the dashboard. Only enable it when port 8081 is reachable through an authenticated router alone; see [self-hosting](self-hosting.md#3-put-the-dashboard-behind-authentication). |
| `SIGNPOST_ADMIN_HOST` | | The dashboard's own hostname. signpost refuses to put links on it. |

## Traefik

| Variable | Default | Description |
|---|---|---|
| `SIGNPOST_UPSTREAM` | `http://signpost:8080` | Where the generated Traefik service sends link traffic. Must be reachable from Traefik. |
| `SIGNPOST_SERVICE` | `signpost` | Name of the generated Traefik service, and the prefix of every generated router (`signpost-link-12`). Change it to run two instances against one Traefik. |
| `SIGNPOST_ENTRYPOINTS` | `websecure` | Comma-separated entrypoints for the generated routers. |
| `SIGNPOST_CERT_RESOLVER` | | Default cert resolver for new domains. Each domain can override it in the dashboard. |
| `SIGNPOST_TRAEFIK_FILE` | | Also write the configuration to this path, for Traefik's file provider. The HTTP endpoint keeps working either way. |

## Checks

| Variable | Default | Description |
|---|---|---|
| `SIGNPOST_PROBE_ADDR` | | Traefik's address from signpost's point of view, such as `traefik:443`. Links are then probed through Traefik directly as well as over the internet, which separates "DNS does not point here" from "Traefik is not routing it". |
| `SIGNPOST_PUBLIC_IPS` | | Comma-separated public IPv4 and IPv6 addresses of the server. The DNS check uses them to tell a record that points straight here from one that points elsewhere. |

## Settings in the dashboard

These live in the database rather than the environment:

- **Default host** (Settings): the host preselected for new links.
- **Cert resolver per domain** (Domains): overrides `SIGNPOST_CERT_RESOLVER`.
- **Fallback per domain** (Domains): a URL, optionally with `{path}`, for
  requests to that host that match no link. Meant for hosts signpost owns
  outright, where nothing else is served.

## Routes and priorities

signpost generates these routers, all pointing at its own service:

| Router | Rule | Priority |
|---|---|---|
| `signpost-link-<id>` | `Host(h) && (Path(p) \|\| Path(p/))`, one per exact link | 10000 |
| `signpost-link-<id>` | `Host(h)`, for a `/*` link | 9000 |
| `signpost-verify-<host>` | `Host(h) && PathPrefix(/.well-known/signpost/)`, one per domain | 10001 |
| `signpost-fallback-<host>` | `Host(h)`, for a domain with a fallback | 1 |

Docker-label routers usually sit around 15-30, so an exact link always wins
over the site on the same host, and every other path still goes to the site.
A host with a `/*` link already sends everything to signpost, which then picks
exact paths over `/*` in memory, so its exact links need no routers of their
own: one router covers the host however many short links it carries. A
fallback has the lowest priority possible and only catches requests that no
other router on the host matches.

The verify router is what lets the DNS check fetch a secret token through
Traefik on hosts that otherwise belong to a site, and so prove the full round
trip.
