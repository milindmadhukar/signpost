# CLI and API

The `signpost` binary is both the server and a client. As a client it talks
to the JSON API over HTTPS with a bearer token, so anything the CLI does, a
script can do with `curl`.

- [CLI](#cli)
- [CSV format](#csv-format)
- [HTTP API](#http-api)

## CLI

Point it at your dashboard host and token:

```console
$ export SIGNPOST_URL=https://links.example.com
$ export SIGNPOST_TOKEN=<the value of SIGNPOST_TOKEN on the server>
```

Get the binary from the container (`docker cp signpost:/signpost .`), or
build it with `go build .`. Flags can go before or after the arguments.

### Links

```console
$ signpost ls
$ signpost add <host/path> <target> [flags]
$ signpost add -short <host> <target>
$ signpost set <host/path|id> <target>
$ signpost rm <host/path|id>
$ signpost clicks <host/path|id>
$ signpost check <host/path|id>
```

| Command | What it does |
|---|---|
| `ls` | Every link with its state, redirect type and click count. |
| `add` | Creates a link. The path must be free on the live internet unless `-force` is given. |
| `add -short` | Creates a link on a generated 7-character slug, with no look-alike characters. |
| `set` | Changes a link's destination. |
| `rm` | Deletes a link and its clicks. |
| `clicks` | The total, and the most recent clicks with time, country and referrer. |
| `check` | Probes the link again and updates its state. |

Flags for `add`:

| Flag | Meaning |
|---|---|
| `-301`, `-302`, `-307`, `-308` | Redirect type. Defaults to 302, which browsers do not cache, so every click is counted and the destination can change later. |
| `-drop-query` | Do not pass the visitor's query string on to the target. |
| `-note text` | A note, shown in the dashboard. |
| `-tags a,b` | Comma-separated tags. |
| `-password pw` | Ask visitors for a password before redirecting. |
| `-force` | Take the path even if something already answers on it. |

A link may end in `/*` to send every path on that host to the target, and a
target may contain `{path}`, replaced with the visited path:

```console
$ signpost add example.com/ade https://amsterdamdanceevent.com/plusxhub -note "ADE"
$ signpost add -short go.example.com https://example.com/a/long/url -tags launch
$ signpost add 'events.example.com/*' 'https://luma.com/{path}' -301
$ signpost add example.com/press https://drive.google.com/... -password hunter2
$ signpost set example.com/ade https://new.target
```

### Domains

```console
$ signpost domains
$ signpost domain <host> [-resolver name] [-fallback url | -no-fallback]
$ signpost domain-rm <host>
```

A domain is connected automatically the first time a link uses it. `domain`
sets its cert resolver or its fallback, the URL that requests matching no
link are sent to (for hosts signpost owns outright):

```console
$ signpost domain example.com -fallback 'https://www.example.com/{path}'
```

### Import and export

```console
$ signpost import links.csv -dry-run     # check every row, create nothing
$ signpost import links.csv              # create them, all or nothing
$ signpost export > links.csv
```

| Flag | Meaning |
|---|---|
| `-dry-run` | Validate and report each row without creating anything. |
| `-force` | Create rows whose path something else already answers. |
| `-skip-check` | Do not probe each path on the live internet first. Faster, but cannot spot a path the site already serves. |

`signpost import -` reads from standard input.

## CSV format

```csv
source,target,status,note,tags
example.com/podcast,https://open.spotify.com/show/example,302,Podcast,social
go.example.com/,https://github.com/example/repo,302,Generated slug,launch
events.example.com/*,https://luma.com/{path},301,,
example.com/press,https://drive.google.com/...,,Press kit,"press,media"
```

| Column | Required | Meaning |
|---|---|---|
| `source` | yes | Host and path. Leave the path empty (`go.example.com/`) for a generated slug, or use `/*` for the whole host. |
| `target` | yes | Where it redirects. `{path}` is replaced with the visited path. |
| `status` | no | 301, 302 (the default), 307 or 308. |
| `note` | no | Free text. |
| `tags` | no | Comma-separated; quote the cell. |

The header row is optional. Up to 5,000 rows per import. Rows are checked for
validity, duplicates within the file, links that already exist and, unless
skipped, paths already served on the live internet. A commit creates exactly
the rows the preview showed, generated slugs included, or nothing at all.
Export writes the same format, so a backup round-trips.

## HTTP API

All endpoints live on the admin host and take and return JSON. Authenticate
with the token:

```console
$ curl -H "Authorization: Bearer $SIGNPOST_TOKEN" https://links.example.com/api/links
```

Errors come back as `{"error": "..."}` with a matching status code. A path
that something already serves is `409 Conflict`; send `"force": true` to take
it anyway.

### Links

| Method and path | Description |
|---|---|
| `GET /api/links` | List links. Optional query: `q` (search), `host`, `state` (`live`, `pending`, `dns`, `error`, `disabled`, `expired`), `tag`. |
| `POST /api/links` | Create a link. Returns `201` and the link. |
| `PATCH /api/links/{id}` | Change any of `target`, `status`, `keep_query`, `note`, `tags`, `password` (empty string removes it), `expires_at`, `disabled`. |
| `DELETE /api/links/{id}` | Delete a link and its clicks. |
| `POST /api/links/{id}/check` | Probe the link again. |
| `GET /api/links/{id}/clicks` | The 500 most recent clicks. |
| `GET /api/links/{id}/stats?range=30d` | Aggregated analytics. `range` is `24h`, `7d`, `30d` (default) or `90d`. |
| `GET /api/links.csv` | Export every link as CSV. |

Creating a link:

```json
POST /api/links
{
  "host": "example.com",
  "path": "/launch",
  "target": "https://example.org/a/very/long/url",
  "status": 302,
  "keep_query": true,
  "note": "Launch post",
  "tags": ["launch"],
  "password": "",
  "expires_at": 1767225600,
  "disabled": false,
  "force": false
}
```

Only `host` and `target` are required. Leave `path` empty for a generated
slug, or use `"*"` for the whole host. `expires_at` is a Unix timestamp; after
it the link answers `410 Gone`.

A link as returned:

```json
{
  "id": 12,
  "host": "example.com",
  "path": "/launch",
  "target": "https://example.org/a/very/long/url",
  "status": 302,
  "keep_query": true,
  "note": "Launch post",
  "tags": ["launch"],
  "clicks": 1521,
  "last_click": 1762160000,
  "check": "live",
  "check_msg": "answers publicly",
  "created_at": 1757664000,
  "updated_at": 1757664000,
  "expires_at": 1767225600,
  "disabled": false,
  "protected": false
}
```

`check` is `pending`, `live`, `dns` or `error`. The password hash never
leaves the server; `protected` says whether there is one.

### Bulk

| Method and path | Description |
|---|---|
| `POST /api/links/bulk` | Create many links at once, from `links` (an array of link objects as above) or `csv` (the CSV text). With `"dry_run": true`, only reports. Also takes `force` and `skip_probe`. |
| `POST /api/links/bulk-action` | Apply `action` to the links in `ids`: `enable`, `disable`, `delete`, `status` (with `arg` a redirect code), `tag` or `untag` (with `arg` the tag). |

```json
POST /api/links/bulk
{ "csv": "example.com/a,https://a.example\nexample.com/b,https://b.example", "dry_run": true }
```

The response lists every row with its `result` (`ok`, `duplicate`,
`conflict`, `invalid`, or `created` after a commit) and a message.

### Domains

| Method and path | Description |
|---|---|
| `GET /api/domains` | List connected domains, with their DNS check results. |
| `PUT /api/domains/{host}` | Connect a domain or update it: `{"cert_resolver": "...", "fallback": "..."}`. |
| `DELETE /api/domains/{host}` | Disconnect a domain. Refused with `409` while it still has links. |
| `POST /api/domains/{host}/check` | Run the DNS check again. |

### Unauthenticated endpoints

| Method and path | Description |
|---|---|
| `GET /traefik.yml` | Traefik's dynamic configuration, with an ETag. Hostnames and paths only. |
| `GET /healthz` | `ok` while the server is up. Used by the container's health check. |
