# Self-hosting signpost

signpost is self-hosted only. You run it next to your own Traefik, and it
never talks to any service of ours, because there isn't one. This guide takes
you from nothing to a working short link on a domain you already serve.

- [Before you start](#before-you-start)
- [1. Run the container](#1-run-the-container)
- [2. Connect Traefik](#2-connect-traefik)
- [3. Put the dashboard behind authentication](#3-put-the-dashboard-behind-authentication)
- [4. Tell signpost about your server](#4-tell-signpost-about-your-server)
- [5. Add your first link](#5-add-your-first-link)
- [Backups](#backups)
- [Upgrades](#upgrades)
- [Troubleshooting](#troubleshooting)

## Before you start

You need:

- **Traefik v3**, already terminating TLS for the domains you want links on.
  signpost was built against v3.7. It uses the HTTP or file provider and
  ordinary routers, so any recent v3 works.
- **Docker** (or anything that runs an OCI image), on the same network as
  Traefik.
- **A host for the dashboard**, such as `links.example.com`. signpost will
  refuse to put links on it.
- **A Linux server** on amd64, arm64 or armv7. The published image covers all
  three, so a Raspberry Pi works as well as a VPS.

signpost does not need its own domain for links. A link on `example.com/launch`
is served from `example.com`, while every other path on `example.com` keeps
going wherever it went before.

## 1. Run the container

Create a directory for it, with a `docker-compose.yml` (the repository has the
same file as [`docker-compose.yml`](../docker-compose.yml)):

```yaml
services:
  signpost:
    image: ghcr.io/milindmadhukar/signpost:latest
    container_name: signpost
    restart: always
    environment:
      - SIGNPOST_TOKEN=${SIGNPOST_TOKEN}
      - SIGNPOST_ADMIN_HOST=links.example.com
      - SIGNPOST_TRUST_REMOTE_USER=true
      - SIGNPOST_UPSTREAM=http://signpost:8080
      - SIGNPOST_ENTRYPOINTS=websecure
      - SIGNPOST_CERT_RESOLVER=letsencrypt
    volumes:
      - data:/data
    labels:
      - traefik.enable=true
      - traefik.docker.network=proxy
      - traefik.http.services.signpost-admin.loadbalancer.server.port=8081

      # The dashboard, behind forward auth (step 3).
      - traefik.http.routers.signpost.rule=Host(`links.example.com`)
      - traefik.http.routers.signpost.entrypoints=websecure
      - traefik.http.routers.signpost.tls.certresolver=letsencrypt
      - traefik.http.routers.signpost.middlewares=authelia@docker
      - traefik.http.routers.signpost.service=signpost-admin

      # The API, for the CLI. It skips forward auth only when a bearer token
      # is sent, and signpost then checks that token itself.
      - traefik.http.routers.signpost-api.rule=Host(`links.example.com`) && PathPrefix(`/api`) && HeaderRegexp(`Authorization`, `^Bearer .+`)
      - traefik.http.routers.signpost-api.priority=100
      - traefik.http.routers.signpost-api.entrypoints=websecure
      - traefik.http.routers.signpost-api.tls.certresolver=letsencrypt
      - traefik.http.routers.signpost-api.service=signpost-admin
    networks:
      - proxy

volumes:
  data:

networks:
  proxy:
    external: true
```

Replace `links.example.com`, `letsencrypt` (your cert resolver), `websecure`
(your HTTPS entrypoint) and `proxy` (the Docker network Traefik is on) with
your own. Then generate a token and start it:

```console
$ echo "SIGNPOST_TOKEN=$(openssl rand -hex 32)" > .env
$ docker compose up -d
$ docker compose logs signpost     # "redirects on :8080, admin on :8081"
```

signpost listens on two ports inside the container, and neither needs to be
published to the host:

| Port | Serves | Reached by |
|---|---|---|
| `8080` | Redirects | Traefik, for the routes in `/traefik.yml` |
| `8081` | Dashboard, API, `/traefik.yml`, `/healthz` | Traefik, through the labels above |

The redirect routers are not in the labels. signpost generates them itself,
which is the next step.

## 2. Connect Traefik

signpost describes one router per link in Traefik's dynamic configuration
format. Traefik can read it in one of two ways.

### HTTP provider (recommended)

Add two flags to the Traefik container's `command`, once:

```yaml
- "--providers.http.endpoint=http://signpost:8081/traefik.yml"
- "--providers.http.pollInterval=5s"
```

and restart Traefik. From then on, a new link is routed within about five
seconds, and an unchanged configuration costs one `304 Not Modified` per poll.

`/traefik.yml` is deliberately unauthenticated so Traefik can fetch it over
the Docker network. It contains hostnames and paths only, never targets,
notes or passwords.

If signpost goes down, Traefik keeps the last routes it fetched. Links return
502 until signpost is back; nothing else on the server is affected.

### File provider

If you would rather not restart Traefik, signpost can write the same file
into a directory Traefik already watches:

```yaml
    environment:
      - SIGNPOST_TRAEFIK_FILE=/traefik/signpost.yml
    volumes:
      - data:/data
      - ../traefik/dynamic:/traefik
```

Traefik's file provider does not watch subdirectories, so mount the watched
directory itself, read-write.

## 3. Put the dashboard behind authentication

signpost has no login page of its own. The dashboard trusts the `Remote-User`
header that a forward-auth middleware sets after it has signed you in, and
only when `SIGNPOST_TRUST_REMOTE_USER=true`. Pick one:

**Authelia.** Authelia sets `Remote-User` out of the box. Use your existing
middleware, as in the example (`authelia@docker`).

**Another single sign-on.** Any forward-auth provider works if it passes the
signed-in username in a `Remote-User` header. Check your provider's docs for
the header it sends.

**Traefik's basic auth.** No identity provider needed. Have Traefik put the
username in `Remote-User`, and strip the `Authorization` header so signpost
does not read it as a bad API token:

```yaml
      - traefik.http.middlewares.signpost-auth.basicauth.users=admin:$$2y$$05$$...   # htpasswd -nB admin
      - traefik.http.middlewares.signpost-auth.basicauth.headerfield=Remote-User
      - traefik.http.middlewares.signpost-auth.basicauth.removeheader=true
      - traefik.http.routers.signpost.middlewares=signpost-auth
```

**No dashboard.** Leave `SIGNPOST_TRUST_REMOTE_USER` unset and use the CLI
and API with `SIGNPOST_TOKEN` alone.

> [!IMPORTANT]
> With `SIGNPOST_TRUST_REMOTE_USER=true`, anything that can reach port 8081
> directly can claim to be anyone. Do not publish 8081 on the host, and make
> sure every Traefik router that points at it either runs your auth
> middleware or, like the `signpost-api` router above, requires a bearer
> token. A request that carries `Authorization` is judged on the token alone,
> so that router cannot be used to forge `Remote-User`.

## 4. Tell signpost about your server

Two optional settings make link and DNS checks much more precise:

```yaml
      # Probe links through Traefik directly as well as over the internet, so
      # "DNS does not point here" can be told apart from "Traefik is not
      # routing it". Use your Traefik container's name and HTTPS port.
      - SIGNPOST_PROBE_ADDR=traefik:443
      # This server's public addresses. The DNS check uses them to tell
      # "points straight here" from "points somewhere else".
      - SIGNPOST_PUBLIC_IPS=203.0.113.10,2001:db8::1
```

Every setting is described in [configuration.md](configuration.md).

## 5. Add your first link

Open `https://links.example.com`, sign in, and press <kbd>N</kbd>. Enter a
destination, pick the host, and either type a path or leave it empty for a
generated 7-character slug.

From a terminal, with the same binary as a client:

```console
$ # download the binary for your OS from the Releases page, or: go build .
$ export SIGNPOST_URL=https://links.example.com SIGNPOST_TOKEN=<your token>
$ ./signpost add example.com/launch https://example.org/a/very/long/url
https://example.com/launch -> https://example.org/a/very/long/url (302)
```

What happens next:

1. Before creating the link, signpost fetches `https://example.com/launch` on
   the public internet. If anything other than a 404 answers, it refuses, so
   an existing page can never be shadowed by accident. Pass `-force` (or tick
   the box in the dashboard) to take the path anyway.
2. The host is connected as a domain if it was not already, and a DNS check
   starts.
3. Traefik picks the new router up on its next poll.
4. signpost probes the link until it answers publicly with the right redirect,
   for up to about five minutes (a brand-new hostname may need a certificate
   first), then marks it **Live**, **DNS issue** or **Error**.

## Backups

Everything lives in one SQLite file, `/data/signpost.db`, in the `data`
volume. The image is built `FROM scratch` and has no shell, so take a
consistent copy with a throwaway container:

```console
$ mkdir -p ~/backups/signpost
$ docker run --rm -v signpost_data:/data:ro -v ~/backups/signpost:/backup alpine \
    sh -c "apk add -q sqlite && sqlite3 /data/signpost.db \".backup /backup/signpost-$(date +%F).db\""
```

The volume name is the Compose project name followed by `_data`; check it
with `docker volume ls`. For a portable copy of the links themselves (without
clicks), `signpost export > links.csv` writes a CSV that `signpost import`
reads back.

## Upgrades

```console
$ docker compose pull -q && docker compose up -d
```

The database schema is migrated in place on start. Back up first, since a
migration only runs forward.

`latest` always points at the newest release. To upgrade only when you choose,
pin a version instead, such as `ghcr.io/milindmadhukar/signpost:1.2.3`, or a
line such as `:1.2` for patch releases only. `:edge` follows every commit to
`main` and is for trying unreleased changes. Releases and their notes are on
the [Releases page](https://github.com/milindmadhukar/signpost/releases).

## Troubleshooting

**A link stays on "Checking".** signpost is still probing it. A new hostname
can take a minute or two to get a certificate. If it never leaves, Traefik is
probably not reading `/traefik.yml`: check Traefik's logs for the HTTP
provider, and that `wget -qO- http://signpost:8081/traefik.yml` works from
inside the Traefik container.

**"DNS issue".** Traefik routes the link, but the public address does not
reach this server. Check the domain on the Domains page; the DNS check says
where the record actually points. This status needs `SIGNPOST_PROBE_ADDR`.

**"Error: answered 404 by something other than this link".** A request
reached a server, but not signpost. Usually the DNS record points somewhere
else, or another router with a higher priority matches the same path.
signpost's routers use priority 10000; Docker-label routers sit around 15-30.

**"already answers 200" when adding a link.** That path already serves
something. Pick another path, or force it if you really mean to replace it.

**The dashboard says "sign in through Authelia".** The request reached
signpost without a `Remote-User` header. Check that the router for the
dashboard runs your auth middleware and that `SIGNPOST_TRUST_REMOTE_USER=true`.

**The CLI says "bad token".** The token sent does not match `SIGNPOST_TOKEN`,
or the request went through a router that rewrote `Authorization`.

## Running without Docker

Each release has a static binary for Linux (amd64, arm64, armv7), macOS and
Windows on the [Releases page](https://github.com/milindmadhukar/signpost/releases).
It needs nothing else; the dashboard, fonts and scripts are built in.

```console
$ SIGNPOST_DB=/var/lib/signpost/signpost.db SIGNPOST_TOKEN=... \
  SIGNPOST_UPSTREAM=http://127.0.0.1:8080 ./signpost serve
```

Point Traefik's HTTP provider at `http://<host>:8081/traefik.yml`, and set
`SIGNPOST_UPSTREAM` to an address Traefik can reach port 8080 on.

## Building the image

To build your own, from a fork or for a platform of your choice:

```console
$ git clone https://github.com/milindmadhukar/signpost && cd signpost
$ docker buildx build --platform linux/arm64 -t signpost --load .
```

The build stage runs on your machine's own architecture (amd64 or arm64) and
Go cross-compiles for the target, so building an ARM image needs no
emulation. Outside Docker, `make build` produces a static binary.
