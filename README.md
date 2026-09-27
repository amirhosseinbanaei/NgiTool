# nginx-edge

One nginx for every project on the server. It runs in Docker Compose (nothing
is installed on the host except Docker and Node) and is managed with the
`edge` CLI. Everything that serving depends on lives in this directory:
config, certificates, static sites, secrets, the project list, and the CLI.

```
$ edge

  nginx-edge  ● running  2 domains · 7 routes · 4 apps  network proxy

? What do you want to do?
  ❯ Add a subdomain   serve a project on api.example.com
    Add a path        serve a project on example.com/admin
    Add a domain      certificate + DNS for a new domain
    Link an app       symlink a project's docker compose into apps/
  manage
    Status            stack, certificates, routes, apps
    Domains           list, add, remove
    Routes            enable, disable, replace, remove
    Apps              start, stop, logs, scan, remove
    Certificates      Let's Encrypt (certbot), renew, remove
    Stack             reload, restart, stop, logs
    Remove…           a domain, host, path, app, certificate, static folder — or everything
    Quit
  ↑↓ move · ↵ select · esc back · type to filter
```

- **Cloudflare**: the CLI creates DNS records (proxied or DNS-only) and restores visitors' real IPs. Authenticated Origin Pulls is optional.
- **Certificates, chosen per host**: Let's Encrypt through certbot (HTTP challenge on port 80, or DNS challenge through Cloudflare; auto-renewed either way), Cloudflare Origin CA, your own files, or self-signed.
- **Subdomains or paths**: `api.example.com` or `example.com/admin`, sending traffic to a linked app, any container, a port on the host, or static files.
- **`apps/`**: one folder per project, holding a symlink to its compose file. Start, stop and read logs with `edge app …`.
- **Safe changes**: every change is checked with `nginx -t` before nginx reloads. If the check fails, the change is rolled back.
- **Everything can be removed again**: domains, hosts, paths, apps, certificates and static folders. The CLI shows what goes with the thing you remove and asks before deleting anything else that is left unused.

---

## 1. Architecture

```
Browser ──HTTPS──► Cloudflare (edge TLS, WAF, cache)
                        │  HTTPS, "Full (strict)"
                        ▼
      ┌──────────── host :80 / :443 ───────────────────────────────────┐
      │  nginx container      routes by hostname, then by path         │
      │  certbot container    issues + renews Let's Encrypt certs      │
      └───────┬──────────────────┬───────────────────────┬─────────────┘
              │ shared network   │ network gateway IP    │ ./www (read-only)
              │ (EDGE_NETWORK)   │                       │
              ▼                  ▼                       ▼
     app containers         host processes          static builds
     my-app:3000            node on :3001           www/blog/
     shop-api:8000          python on :8000         www/example.com/
```

1. **Only nginx publishes ports** (80 and 443). App containers use `expose:`, never `ports:`.
2. **Containers are reached by name** on one shared Docker network, `EDGE_NETWORK` (for example `proxy`).
3. **Host processes are reached at that network's gateway IP.** They listen there, where the internet can't reach them.
4. **Upstreams are resolved per request.** If an app is down, only that app answers 502. nginx keeps starting and running.
5. **`edge.json` is the source of truth.** nginx files are generated from it, `nginx -t` checks them, and nginx reloads only if the check passes.

---

## 2. Install and first run

Requirements: Docker with Compose v2, and Node ≥ 18 (for the CLI). Ports 80 and 443 must be free.

```bash
mv nginx-edge /opt/nginx-edge                   # anywhere works; everything is relative
cd /opt/nginx-edge
./edge install        # optional: puts `edge` on PATH (/usr/local/bin/edge → ./edge)
edge init
```

`edge init` asks for four things:

| Question | Why |
|---|---|
| Let's Encrypt email | expiry notices |
| Docker network | the network nginx shares with your apps. Pick an existing one your apps already use (for example `proxy`) or create `edge`. |
| Cloudflare API token | for DNS records, Origin CA and Let's Encrypt DNS challenges. Create it at **My Profile → API Tokens → Create Token → "Edit zone DNS"**. For Origin CA certificates, also add *Zone → SSL and Certificates → Edit*. Press Enter to skip: Let's Encrypt still works through the HTTP challenge. |
| Public IP | where DNS records point (detected automatically) |

Then it fetches Cloudflare's IP ranges, writes the config and offers to start the stack.

In the Cloudflare dashboard, set **SSL/TLS → encryption mode → Full (strict)** once.
`edge` never changes that setting.

---

## 3. Serving projects

### Add a domain: `edge domain add example.com`

1. **Certificate**. *Let's Encrypt · DNS* gives one wildcard (`example.com` + `*.example.com`) that covers every subdomain (Cloudflare needed). *Let's Encrypt · HTTP* covers `example.com` + `www.example.com` through port 80 (no Cloudflare needed). Origin CA, custom and self-signed are also offered.
2. **What the domain itself shows**: a placeholder page (`www/example.com/`), a project, or nothing yet.
3. **Cloudflare DNS** for `example.com` and `www.example.com`: proxied, DNS only, or leave alone.

### Add a subdomain: `edge site add api.example.com`

The CLI asks, in order:

1. **Cloudflare DNS**
   - *Proxied* (orange cloud): traffic goes through Cloudflare
   - *DNS only* (grey cloud): visitors connect straight to the server
   - *Leave DNS alone*: you manage the record yourself
2. **Certificate**
   - *Use \<existing\>*: every certificate that already covers the host is listed with its type and days left, for example the domain's wildcard.
   - *Let's Encrypt · HTTP*: a new certificate for this host from certbot, validated over port 80. Free, renews itself, no Cloudflare needed. The DNS record is created before the request, because Let's Encrypt checks it.
   - *Let's Encrypt · DNS*: the same, validated through a Cloudflare TXT record. Works behind the orange cloud and for wildcards.
   - *Cloudflare Origin CA*: a 15-year certificate that only Cloudflare trusts. Not offered for DNS-only hosts, because browsers would reject it.
   - *Custom certificate*: import a fullchain and key you already have, from file paths or pasted. The CLI checks that the key matches and that the certificate hasn't expired.
   - *Self-signed*: for testing only.
3. **Where traffic goes**: see below.
4. **Plain HTTP (port 80)**: *Redirect to HTTPS* (the default) or *Serve over HTTP too* (see below).
5. **A summary**, then *Go ahead?*

### Add a path: `edge path add example.com/admin`

Pick a host that is already served, a path, and where traffic goes. For apps, you
also choose whether the prefix is kept or stripped:

| | App receives | Use when |
|---|---|---|
| **Keep** (default) | `/admin/users` | the app is built with base path `/admin` (Vite `base`, Next.js `basePath`, React Router `basename`, FastAPI `root_path`, Django `FORCE_SCRIPT_NAME`) |
| **Strip** | `/users` | APIs whose responses contain no absolute links |

### Where traffic goes

| Choice | Flag | What happens |
|---|---|---|
| **Linked app** | `--app NAME/SERVICE[:PORT]` | Pick an app from `apps/`, a service, and a port (read from its compose file). If the service isn't on the nginx network yet, pick **edge override** (`apps/<name>/edge.override.yaml`; the project's own file is untouched) or **I'll edit the compose file** (the CLI prints the lines to add). |
| **Running container** | `--container NAME:PORT` | Any container. If it isn't on the network, the CLI connects it (until the container is recreated; link its compose to make it permanent). |
| **Host process** | `--port PORT` | A process on this server, reached at the network gateway. The CLI warns if nothing listens there and prints the exact `ufw` rule. |
| **Static files** | `--static DIR` | `www/DIR/`. A new folder gets a placeholder page, and unknown paths fall back to `index.html` (SPA). |

### Managing routes

```bash
edge ls                          # every route with a live/down/disabled dot
edge disable example.com/admin   # stop serving it; the entry stays in edge.json
edge enable  example.com/admin
edge rm      api.example.com     # the host and its paths (see "Removing things")
```

In the menu, **Routes → (route) → Change where it points** runs the add flow again for that route.

### Serving plain HTTP too

By default port 80 only redirects to HTTPS. To make a host work on `http://` as
well, without the redirect:

```bash
edge http api.example.com serve       # http:// and https:// both work
edge http api.example.com redirect    # back to the default
edge site add api.example.com --http serve …
```

Or use the menu: **Routes → (host) → Serve over HTTP too**. The host's generated
file then has a second `server` on port 80 that serves the same app, folder and
path routes. The app gets `X-Forwarded-Proto: http` on those requests, so it
doesn't build `https://` links or redirects.

- **HSTS**: every other host sends `Strict-Transport-Security` for a year, which
  makes browsers switch to `https://` on their own. A host served over HTTP sends
  `max-age=0` on HTTPS instead. A browser that stored the old policy keeps
  switching until it loads the `https://` page once more (or you clear it at
  `chrome://net-internals/#hsts`).
- **Cloudflare-proxied hosts**: Cloudflare's *Always Use HTTPS* redirects before
  the request reaches this server. Turn it off for HTTP to work.
- Nothing on `http://` is encrypted, including logins and cookies.

### Removing things

Everything you can add can be removed again: from the menu (**Remove…**, or
the Domains / Routes / Apps / Certificates menus), or with a command:

```bash
edge rm                          # pick what to remove
edge rm api.example.com          # a host, with its paths
edge rm example.com/admin        # one path
edge domain rm example.com       # the domain, its hosts and paths
edge app rm shop                 # unlink apps/shop and remove the routes that use it
edge cert rm old-cert            # switch its hosts to another covering cert, or remove them
edge www ls                      # static folders and the routes serving them
edge www rm blog                 # a folder in www/, with the routes serving it
edge reset                       # every route, domain, certificate and app link
```

Each removal works in three steps:

1. **It shows what goes with it.** Removing a domain takes its hosts and paths
   with it. Hosts of a more specific domain you added (`shop.example.com`) stay.
   Removing an app or a static folder takes the routes that point at it.
2. **It asks.** `edge reset` asks you to type `reset`.
3. **It asks about leftovers.** Some things are only left unused by the removal:
   certificates, `www/` folders, Cloudflare DNS records, and the app's running
   containers. You tick the ones to delete. Certificates are ticked by default.

Nginx is changed in one step, checked with `nginx -t`, and rolled back if the
check fails. Files and DNS records are deleted only after the check passes. A
project folder is never deleted. `edge app rm` removes only the symlink and
`edge.override.yaml`, and stops the containers only if you tick that. A
domain is never removed together with its certificate: `edge cert rm` points
the domain at the certificate you switch to, or at none.

Without a terminal: removing more than the target itself needs `--force`,
leftovers are deleted only with `--purge` (`--purge-dns` deletes only DNS records), and
`edge reset` needs `--yes --force`. `edge cert rm old --cert new` moves the hosts
to `new` instead of removing them.

### Scripting (no prompts)

Every question has a flag. Off a terminal, the CLI fails with the flag it needs instead of prompting.

```bash
edge domain add example.com --cert letsencrypt --challenge dns --apex placeholder --dns proxied -y
edge site add app.example.org --port 3001 --cert letsencrypt --challenge http --dns skip -y
edge site add api.example.com --app shop/api:8000 --cert auto --dns proxied -y
edge site add legacy.example.com --container old_app:8080 --cert custom --cert-file f.pem --key-file k.pem -y
edge path add example.com/api --app shop/api:8000 --strip -y
edge path add example.com/docs --static docs -y
```

---

## 4. Apps: `apps/`

```
apps/
├── my-site/
│   └── compose.yaml → /home/you/my-site/compose.yaml
├── blog/
│   └── compose.yaml → /home/you/Blog/compose.yaml
└── shop/
    ├── docker-compose.yml → /home/you/shop/docker-compose.yml
    └── edge.override.yaml        # only if you let edge attach a service to the network
```

```bash
edge app link ~/my-site                     # or a compose file path; --name to rename
edge app scan                               # finds compose projects under /home/* — tick which to link
edge app ls                                 # running/total containers, compose path, routes using it
edge app up|down|restart|ps|logs|pull|build <name> [service…]
edge app rm <name>                          # unlink, with its routes; the project stays
```

- **Compose always runs against the real file**, with the project folder as the working directory. The build context, `./data` mounts, `.env` and the project name are exactly what they are when you run `docker compose` inside the project.
- **Only safe fields are read from a project's config**: the CLI reads `docker compose config` for service names, ports and networks only. Resolved configs contain secrets (env values), and the CLI never prints or stores them.
- **If a service joined the network through an edge override**, start that app with `edge app up`. A plain `docker compose up` in the project folder doesn't know about the override. To make it independent of `edge`, add the network to the project's own compose file (see `examples/docker-app.compose.yaml`).

---

## 5. Certificates

| Kind | Stored in | Renewal | Notes |
|---|---|---|---|
| Let's Encrypt · HTTP | `data/letsencrypt/live/<name>/` | automatic: certbot runs every 12h, and nginx reloads every 6h | HTTP-01: port 80 must reach this server. No Cloudflare needed. No wildcards. |
| Let's Encrypt · DNS | `data/letsencrypt/live/<name>/` | the same | DNS-01 through Cloudflare. Wildcards, and the proxy doesn't get in the way. |
| Cloudflare Origin CA | `data/certs/<name>/` | valid for 15 years | host must be proxied; the token needs *SSL and Certificates → Edit* |
| Custom | `data/certs/<name>/` | yours | key match and expiry are checked on import |
| Self-signed | `data/certs/<name>/` | 90 days | testing only; Full (strict) rejects it (Cloudflare error 526) |

```bash
edge cert ls                     # type, names, days left (red under 14 days)
edge cert add api.x.com,*.x.com  # a certificate outside the domain/site flows
edge cert renew                  # renew what's due now, then reload
edge cert renew <name> --force   # renew one now, even if it isn't due
edge cert rm <name>              # switch its hosts to another certificate, or remove them
edge cert aop <name> on|off      # Authenticated Origin Pulls (below)
edge certbot certificates        # any certbot command, run in the certbot container
```

### How certbot is wired in

The `certbot` service in `compose.yaml` runs `certbot renew` every 12 hours.
nginx picks up renewed certificates on its 6-hour reload, or immediately after
`edge cert renew`. New certificates are requested by the CLI with
`docker compose run certbot certonly …`.

For the HTTP challenge, the certbot service and nginx share `data/acme/`.
certbot writes the challenge file there, and nginx serves
`/.well-known/acme-challenge/` from it on port 80 for every hostname. Generated
sites serve it on 443 too. `conf/snippets/acme-challenge.conf` holds that
location block.

Before it asks Let's Encrypt, the CLI does its own check. It puts a test file
in `data/acme/` and fetches it from the internet, the way Let's Encrypt will.
DNS pointing elsewhere, a closed port 80, or an nginx started before
`data/acme` existed are reported before any rate limit is used. Add `--force`
to ask Let's Encrypt anyway.

**HTTP challenge behind Cloudflare's orange cloud.** Cloudflare's *Always Use
HTTPS* redirects the challenge to HTTPS before it reaches nginx. The first
request then fails (Cloudflare 525), because the host has no HTTPS site yet.
Turn *Always Use HTTPS* off while the certificate is issued, or use the DNS
challenge. Renewals work either way, because the site then exists and serves
the challenge on 443.

Wildcards cover one level only: `*.example.com` covers `api.example.com` but
not `v2.api.example.com`. Cloudflare's free edge certificate has the same limit.
The CLI warns when you add a host that is two levels deep.

---

## 6. Cloudflare details

- **DNS records**: `edge site add` and `edge domain add` create or update an `A` record → `SERVER_IP`, proxied or not. The record carries the comment *managed by nginx-edge*. An existing CNAME is never replaced.
- **Real visitor IP**: `conf/conf.d/cloudflare-realip.conf` trusts Cloudflare's ranges and reads `CF-Connecting-IP`. Refresh it with `edge cf-sync` (safe to run from cron).
- **Accepting traffic only from Cloudflare**: Docker writes its own iptables rules, so ufw can't filter ports that Docker publishes. Use Authenticated Origin Pulls instead:
  1. Cloudflare → SSL/TLS → Origin Server → **Authenticated Origin Pulls: On**
  2. `edge cert aop example.com on`

  Do step 1 first. Otherwise every request to hosts using that certificate fails.
- **Unknown hostnames and the raw IP**: `conf/sites/00-default.conf` refuses the TLS handshake and redirects port 80 to HTTPS.

---

## 7. Directory structure

```
nginx-edge/
├── edge                         # CLI launcher (./edge, or `edge` after ./edge install)
├── cli/                         # the CLI: zero-dependency Node ESM
│   ├── bin/edge.mjs
│   ├── src/
│   │   ├── main.mjs             #   commands, menu, status
│   │   ├── flows.mjs            #   guided flows: init, domain, site, path, apps, certs
│   │   ├── remove.mjs           #   removal plans (what goes with it) and applying them
│   │   ├── prompt.mjs           #   select / multiselect / input / confirm / paste (raw stdin)
│   │   ├── ui.mjs               #   colour, tables, spinners
│   │   ├── nginx.mjs            #   edge.json → nginx files, nginx -t, reload, rollback
│   │   ├── certs.mjs            #   certbot, Origin CA, import, self-signed, X.509 checks
│   │   ├── cloudflare.mjs       #   API: zones, DNS records, Origin CA, IP ranges
│   │   ├── apps.mjs             #   apps/ symlinks, compose services, override
│   │   ├── targets.mjs          #   hostnames, paths, cert coverage, sources (pure)
│   │   ├── config.mjs           #   paths, .env, edge.json, token
│   │   ├── exec.mjs             #   docker / compose / openssl processes
│   │   └── args.mjs             #   argv parser, help
│   └── test/run.mjs             # npm test
├── package.json
├── compose.yaml                 # nginx + certbot; rarely edited
├── .env                         # ports, network, email, public IP        (git-ignored)
├── edge.json                    # WHAT IS SERVED: certs, domains, sites, paths
│
├── apps/<name>/<compose file>   # symlinks to each project's compose file
├── conf/                        # all nginx config → /etc/nginx/edge (read-only)
│   ├── nginx.conf               #   main config
│   ├── start.sh                 #   nginx + 6h reload loop
│   ├── conf.d/                  #   tls, resolver, websocket map, cloudflare-realip (generated)
│   ├── snippets/                #   proxy.conf, security-headers.conf, cloudflare-aop.conf, acme-challenge.conf
│   │   └── ssl/<cert>.conf      #   GENERATED
│   ├── sites/                   #   00-default.conf (hand-written) + <host>.conf (GENERATED)
│   ├── locations/<host>/        #   <path>.conf (GENERATED)
│   └── certs/                   #   Cloudflare origin-pull CA
├── templates/                   # what the generated files are made from
├── www/<dir>/                   # static sites → /var/www (read-only)
├── data/
│   ├── letsencrypt/             # certbot state and certs                 (git-ignored)
│   ├── acme/                    # HTTP-01 challenge files (certbot → nginx) (git-ignored)
│   └── certs/<name>/            # Origin CA / custom / self-signed certs  (git-ignored)
├── secrets/cloudflare.ini       # API token, chmod 600                     (git-ignored)
└── examples/                    # compose + systemd examples for projects
```

**Generated files and hand-written files.** Generated files start with
`# Managed by edge` and are rewritten from `edge.json` on every change, so edit
the route through the CLI, not the file. Files without that header, such as
`00-default.conf` or anything custom you add to `conf/sites/`, are never
touched. The CLI also refuses to overwrite one of them.

`edge sync` regenerates every managed file, for example after editing a template.

---

## 8. Operations

```bash
edge status                 # stack health, certificates with days left, routes (live/down), apps
edge up | down | restart
edge reload                 # nginx -t, then graceful reload
edge test
edge logs [host]            # follow the access log, optionally one host, status codes coloured
edge cf-sync
npm test                    # unit tests for the CLI (pure logic + file generation)
```

**Back up** `.env`, `edge.json`, `secrets/`, `data/`, `www/` and `apps/`. That is
the whole directory, so `tar czf edge-$(date +%F).tgz nginx-edge/` is enough.
Put everything except `.env`, `secrets/` and `data/` in git.

### Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Red dot / 502 for one route | Upstream down or not on the network. `edge status`, `edge app ps <name>`, `edge logs <host>` (`up=` shows the address nginx tried). |
| Service on override answers 502 after a redeploy | It was started with plain `docker compose up`. Use `edge app up <name>`, or add the network to the project's compose file. |
| Cloudflare 521 | nginx isn't running: `edge up` |
| Cloudflare 525 | No site for that hostname, or AOP on in nginx but off in Cloudflare |
| Cloudflare 526 | Self-signed or expired certificate, or a host two levels deep |
| Redirect loop | Cloudflare SSL mode is "Flexible". Set it to Full (strict). |
| `http://` still redirects to HTTPS | Run `edge http <host> serve`. If it still redirects, the browser remembered HSTS: open the `https://` page once, or clear it at `chrome://net-internals/#hsts`. For proxied hosts, turn off Cloudflare's *Always Use HTTPS*. |
| Path app loads, but CSS/JS 404 | App isn't built with its base path (§3) |
| "nginx rejected the configuration" | The error names the file. Nothing was changed. A hand-written file in `conf/` is the usual cause. |
| Every visitor has the same IP | `edge cf-sync` |
| Let's Encrypt HTTP check fails | The CLI names the host and the reason. DNS must point here and port 80 must be open. For proxied hosts, turn *Always Use HTTPS* off or use the DNS challenge (§5). After updating nginx-edge, run `edge up` once so nginx gets the `data/acme` mount. |
