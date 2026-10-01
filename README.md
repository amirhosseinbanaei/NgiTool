# NgiTool

NgiTool is one command, `ngitool`, that finds every nginx on a Linux server —
installed on the host, in Docker containers, in Docker Compose projects — and
manages them for reverse proxy and load balancing. It can also run its own
nginx + certbot stack, issue certificates, and keep Cloudflare DNS in step.

Every change is shown as a diff, tested with `nginx -t`, reloaded, and checked
with a real request; anything that fails is rolled back on its own.

- [Install](#install)
- [Quickstart](#quickstart-60-seconds)
- [Commands](#commands)
- [Updates and versions](#updates-and-versions)
- [Reverse proxy and load balancing](#reverse-proxy-and-load-balancing)
- [The edge stack](#the-edge-stack)
- [Certificates and Cloudflare](#certificates-and-cloudflare)
- [Safety model](#safety-model)
- [Troubleshooting](#troubleshooting)
- [Migrating from nginx-edge](#migrating-from-nginx-edge)

## Install

Linux on amd64, arm64 or armv7, as root (NgiTool reads every nginx and writes
to the ones you allow):

```bash
curl -fsSL https://raw.githubusercontent.com/amirhosseinbanaei/NgiTool/main/install.sh | sh
```

It installs `/usr/local/bin/ngitool` and a short alias, `ngt`, after checking
the download's SHA-256. Then:

```bash
ngitool doctor     # what this server has: root, Docker, Compose v2, ports, configs
ngitool            # the menu; every action is also a command with flags
```

| | |
|---|---|
| A specific version | `curl -fsSL …/install.sh \| NGITOOL_VERSION=v1.0.0 sh` |
| Somewhere else | `curl -fsSL …/install.sh \| NGITOOL_PREFIX=$HOME/.local sh` |
| Uninstall | `ngitool uninstall` (binary and alias) or `ngitool uninstall --purge` (also `/etc/ngitool`, `/var/lib/ngitool`, `/var/cache/ngitool`). nginx, its files and containers stay. |

NgiTool needs Docker with Compose v2 for containers and the edge stack; host
nginx works without Docker.

## Quickstart (60 seconds)

**1. Look.** `ngitool scan` lists every nginx, who owns ports 80 and 443 (the
front door), and what NgiTool may do to each one.

**2. Choose the nginx NgiTool writes to.** Adopt one you already run:

```bash
ngitool instance adopt            # pick it; the plan is shown before anything is written
```

or create NgiTool's own stack (nginx + certbot in Docker Compose):

```
$ ngitool edge init /opt/ngitool/edge --network edge --email admin@example.com --start

  Edge stack  /opt/ngitool/edge
✔ Creating Docker network edge
✔ Wrote the stack (12 written, 0 already there, 0 kept as they are)
✔ Fetched Cloudflare IP ranges (22 ranges)

  Ready
    Directory    /opt/ngitool/edge
    Project      edge
    Network      edge  172.30.0.0/24 · host apps listen on 172.30.0.1 · interface br-edge
    Cloudflare   no token — DNS records, Let's Encrypt DNS-01 and Origin CA are off (HTTP-01 still works)
    Public IP    203.0.113.10
    Ports        80, 443

✔ Starting nginx and certbot
…
✔ Testing the new config (nginx -t)
✔ Reloading edge-nginx-1
✔ Applied to edge-nginx-1 · 0 files changed, 2 added, 0 removed · snapshot 20261001T072524.629Z
```

**3. Link an app** (any Compose project on the server):

```
$ ngitool app link /srv/shop --no-serve

  Link shop
    Project      shop  -p shop
    Directory    /srv/shop
    Files        1. /srv/shop/compose.yaml
    Services     web
    Owner        root

✔ Linked shop → /srv/shop
```

**4. Route a hostname to it.** The app joins nginx's network through
NgiTool's own override file — your compose files are never edited:

```
$ ngitool route add shop.example.com --to app:shop/web:80 --http-only
✔ Wrote /var/lib/ngitool/overrides/shop.yaml  (web joins edge as shop-web)
…
  Add route
    Route        shop.example.com
    Instance     edge-nginx-1  edge:/opt/ngitool/edge
    Target       shop/web:80  compose-service
    TLS          none — HTTP only

  Changes  edge-nginx-1
    +++ /opt/ngitool/edge/conf/ngitool/upstreams/shop_example_com.conf
    +upstream ngt_shop_example_com {
    +    zone ngt_shop_example_com 64k;
    +    resolver 127.0.0.11 valid=10s ipv6=off;
    +    server shop-web:80 resolve;
    +}
    …
✔ Testing the new config (nginx -t)
✔ Reloading edge-nginx-1
✔ http://shop.example.com/ → 200 (4ms)
```

**5. Load-balance.** Two or more targets make a pool:

```
$ ngitool cert add api.example.com --kind self-signed
$ ngitool route add api.example.com --to container:api1:80 --to container:api2:80 --method least_conn --cert api.example.com

  Add route
    Route        api.example.com
    Pool         api1:80, api2:80
    Method       least connections
    TLS          api.example.com  /etc/edge-certs/api.example.com/fullchain.pem
    Port 80      redirect
…
✔ https://api.example.com/ → 200 (7ms)

$ ngitool route ls

  edge:/opt/ngitool/edge  edge layout · /etc/nginx/edge
  ├─ api.example.com  https · api.example.com · port 80 redirect
  │  └─ ● /  → api1:80, api2:80 · least connections  200 · Oct 1 09:25
  └─ shop.example.com  http only
     └─ ● /  → shop/web:80  200 · Oct 1 09:25

  ● reached its upstream  ✖ 502/503/504  ○ not probed yet
```

Run `ngitool` with no arguments for the same things as a menu. Off a
terminal, every question has a flag, and `--yes` confirms.

## Commands

`ngitool help <command>` shows every flag. Lists take `--json`.

**Routes**

| Command | |
|---|---|
| `route add [host[/path]]` | the wizard: instance, hostname, targets, method, certificate, DNS, port 80 mode |
| `route ls`, `route edit`, `route enable`, `route disable`, `route rm` | |
| `rm [host[/path]]` | remove a host, path, domain (`--domain`), app (`--app`), certificate (`--cert`) or static folder (`--www`) with what goes with it |
| `reset` | remove everything NgiTool manages |

Targets: `app:APP/SERVICE:PORT` · `service:PROJECT/SERVICE:PORT` ·
`container:NAME:PORT` · `port:PORT` (a process on the host) · `HOST:PORT` ·
`https://HOST` · `unix:/path.sock` · `static:FOLDER` (edge stack) or
`static:/dir` (host nginx), each optionally followed by
`,weight=N`, `,backup`, `,down`, `,max_fails=N`, `,fail_timeout=T`, `,max_conns=N`.

**Load balancing** — `pool ls|show|check`, `pool drain|undrain <pool> <member>`,
`pool switch` (blue/green), `pool member add|rm|weight|backup`, `pool rm`.

**Apps** — `app link|scan|ls|show`, `app up|restart|recreate|rebuild|pull|stop|down|logs|ps`,
`app attach`, `app fix`, `app unlink`.

**Edge stack** — `edge init|up|down|restart|status|logs [host]|cf-sync|upgrade-assets`, `www ls|rm`.

**Certificates & DNS** — `cert ls|add|renew|rm|aop`, `certbot [args…]`, `domain ls|rm`.

**Apply** — `diff`, `apply` (re-render from state), `rollback [instance] [snapshot]`, `test`, `reload`.

**Instances** — `scan`, `instances`, `inspect [id]`, `instance adopt|release|externalize`.

**Server** — `doctor`, `migrate edge <dir> [--dry-run]`.

**NgiTool** — `version`, `update`, `completion bash|zsh|fish`, `uninstall`.

Global flags: `--yes` (`-y`) skips confirmations, `--force` with `--yes` also
skips the typed ones, `--no-color` (or `NO_COLOR=1`) prints plain text.

## Updates and versions

```bash
ngitool --version            # or: ngitool version --json
ngitool update --check       # exit 10 when a newer release exists
ngitool update               # download, verify SHA-256, swap atomically
ngitool update --version v1.0.0   # pin (or downgrade); a plain update unpins
ngitool update --rollback    # back to the binary before the last update
```

Releases come from GitHub (`amirhosseinbanaei/NgiTool`); the previous binary
is kept as `ngitool.prev`. Once a day at most, after a command finishes,
NgiTool checks for a newer release in the background and mentions it in one
line; it never slows a command down. Turn it off with `"updateCheck": false`
in `/etc/ngitool/config.json`, or `NGITOOL_NO_UPDATE_CHECK=1`. Offline
mirrors: `NGITOOL_DOWNLOAD_BASE` (laid out as `<base>/<tag>/<asset>`).

## Reverse proxy and load balancing

A **route** sends a hostname (`api.example.com`) or a path of one
(`example.com/blog`) to a **pool** of one or more members. A single target is
a pool of one; you meet pools when you add a second member.

- Members are named, never by IP: containers by name or a compose alias on a
  network nginx shares. On nginx ≥ 1.27.3 names resolve at runtime
  (`server name resolve`), so nginx starts even when a member is down — that
  route answers 502, everything else keeps working.
- Path routes keep the prefix by default (`/blog/x` reaches the app as
  `/blog/x`); `--strip` sends `/x`. Apps without a base path usually break on
  a path — prefer a subdomain.
- Port 80 either redirects to HTTPS (with HSTS) or serves plain HTTP too
  (`--http serve`, HSTS off).

| Method | `--method` | Use it for |
|---|---|---|
| Least connections | `least_conn` | uneven or long requests — the default for two or more |
| Round robin | `round_robin` | even, stateless traffic |
| IP hash | `ip_hash` | simple stickiness when clients connect directly |
| Consistent hash | `hash --hash-key $request_uri` | caches, sticky sessions by a key |
| Random, two, least connections | `random_two` | many members, several proxies in front |

Sticky sessions behind Cloudflare: `--sticky cloudflare` (hash on the
visitor's real IP). Health is passive (`max_fails`); `ngitool pool check`
requests every member from nginx's own network. `pool drain` takes a member
out for a deploy, `pool switch` swaps a blue/green set and keeps the old one
as a backup until you confirm.

## The edge stack

When a server has no nginx in front yet, `ngitool edge init` creates one: an
nginx and a certbot container in Docker Compose, from files inside the
binary. Default directory `/opt/ngitool/edge`:

```
compose.yaml              nginx (:80/:443) + certbot (renews every 12h)
.env                      ACME_EMAIL, HTTP_PORT, HTTPS_PORT, EDGE_NETWORK, SERVER_IP
conf/                     nginx config, mounted read-only at /etc/nginx/edge
  sites/00-default.conf   unknown hosts get nothing (redirect on :80, no certificate on :443)
  sites/<host>.conf       written by NgiTool
www/<folder>/             static sites (--to static:<folder>)
data/letsencrypt|certs|acme/
secrets/cloudflare.ini    the Cloudflare token (0600)
```

Apps never publish ports: they join the stack's network (`EDGE_NETWORK`),
and processes on the host are reached at that network's gateway IP.
`ngitool edge up|down|restart|status`, `ngitool edge logs [host]` (status
codes coloured), `ngitool edge cf-sync` (Cloudflare's IP ranges, so logs and
apps see real visitor IPs) and `ngitool edge upgrade-assets` (what a newer
NgiTool would change in compose.yaml and conf/; your routes, certificates
and sites are never touched).

## Certificates and Cloudflare

`ngitool cert add`, or "Issue a new certificate…" in the route wizard:

| Kind | How | Renewal |
|---|---|---|
| Let's Encrypt, HTTP-01 | certbot through port 80; NgiTool first fetches a test file from every name over the internet, so a wrong DNS record or closed port never spends Let's Encrypt's rate limit | automatic (certbot, every 12h) |
| Let's Encrypt, DNS-01 | certbot through Cloudflare's API; the only way to get wildcards | automatic |
| Cloudflare Origin CA | 15-year certificate signed by Cloudflare; trusted only behind Cloudflare's proxy | not needed |
| Custom | your full chain and key (checked: the key must match, not expired) | yours |
| Self-signed | testing only | — |

Keys are written 0600. `cert ls` shows days left (red under 14),
`cert renew [--force]`, `cert rm` (moves the routes to another certificate
with `--cert-to`, or removes them), `cert aop <name> on` (only Cloudflare may
connect — turn Authenticated Origin Pulls on in Cloudflare first). Wildcards
cover one level: `*.example.com` does not cover `a.b.example.com`.

With a Cloudflare token (`edge init` asks; Zone → DNS → Edit, plus SSL and
Certificates → Edit for Origin CA), the route wizard also sets the host's A
record: proxied (orange cloud), DNS only, or left alone (`--dns`). On host
nginx, Let's Encrypt uses certbot on the host (`apt install certbot
python3-certbot-nginx`).

## Safety model

- **Markers.** Every file NgiTool writes starts with `# Managed by NgiTool`
  and a hash of its body. Files without it are never rewritten or deleted.
  To use an existing nginx, NgiTool adds one `include` line to it (or none,
  when `conf.d/*.conf` is already included) — that line is the only edit it
  ever makes to a file it did not write, and `instance release` removes it.
- **One transaction.** Diff → confirm → snapshot → write (all files staged,
  then swapped) → `nginx -t` → reload → a request to every changed route. A
  failure at any step puts the snapshot back and reloads the old config.
- **Hand edits** to NgiTool's files are detected by their hash: keep them
  (adopted into state), overwrite them, or stop.
- **Snapshots** live in `/var/lib/ngitool/backups/<instance>/` (the last 20);
  `ngitool rollback` restores one. `--dry-run` shows the diff and changes
  nothing.
- **State**: `/etc/ngitool/config.json`, `/var/lib/ngitool/state.json`,
  `/var/cache/ngitool/` — 0600 files in 0700 directories. Tokens and keys
  are never printed or stored there.

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| **502** | the member is down, not on nginx's network, or listens elsewhere | `ngitool route ls` (last probe), `ngitool pool check <pool>`, `ngitool app ls` |
| 502 after `docker compose up` by hand | the app was started without NgiTool's override and left nginx's network | `ngitool app fix <app>`; start it with `ngitool app up` from now on |
| **413** Request Entity Too Large | uploads bigger than `client_max_body_size` | `ngitool route edit <host> --body-size 100m` |
| 502 "upstream sent too big header" | large cookies or Link headers | already raised in NgiTool's proxy snippet (16k); hand-written routes: include it |
| Cloudflare **525** | Cloudflare cannot complete TLS with nginx (no certificate for the name, or AOP on in nginx but off in Cloudflare) | `ngitool cert ls`, `ngitool cert aop <name> off` |
| Cloudflare **526** | the certificate is not valid for Full (strict): self-signed, expired, or the wrong name | Let's Encrypt or Origin CA: `ngitool cert add` |
| Let's Encrypt HTTP-01 fails | DNS points elsewhere, port 80 closed, or Cloudflare's "Always Use HTTPS" | the preflight names the host and the reason; or `--challenge dns` |
| an app is "detached" | see 502 after `docker compose up` above | `ngitool app fix` |

Every situation NgiTool knows about is listed with its ID (`RP-07`,
`LB-03`, …) in [docs/edge-cases.md](docs/edge-cases.md); messages end with
that ID.

## Migrating from nginx-edge

An nginx-edge directory (the stack the old Node `edge` CLI managed) moves
under NgiTool in place, without downtime:

```bash
ngitool migrate edge /srv/nginx-edge --dry-run   # the mapping, nginx -t, the effective config compared
ngitool migrate edge /srv/nginx-edge             # one transaction; a full backup first
```

The dry run ends with "safe to migrate" or "N blockers". The step-by-step
runbook, including rollback and the old→new command table, is
[docs/migration.md](docs/migration.md).

## Development

Changes are listed in [CHANGELOG.md](CHANGELOG.md). Working on NgiTool:
[AGENTS.md](AGENTS.md) (architecture, paths, tests) and
[docs/ux.md](docs/ux.md) (terminal UX rules). `make check` is the gate.
