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
    Routes            enable, disable, replace, remove
    Apps              start, stop, logs, scan, unlink
    Certificates      list, add, renew, remove
    Stack             reload, restart, stop, logs
    Quit
  ↑↓ move · ↵ select · esc back · type to filter
```

- **Cloudflare**: the CLI creates DNS records (proxied or DNS-only) and restores visitors' real IPs. Authenticated Origin Pulls is optional.
- **Certificates, chosen per host**: Let's Encrypt (certbot + Cloudflare DNS, auto-renewed), Cloudflare Origin CA, your own files, or self-signed.
- **Subdomains or paths**: `api.example.com` or `example.com/admin`, sending traffic to a linked app, any container, a port on the host, or static files.
- **`apps/`**: one folder per project, holding a symlink to its compose file. Start, stop and read logs with `edge app …`.
- **Safe changes**: every change is checked with `nginx -t` before nginx reloads. If the check fails, the change is rolled back.

---

## 1. Architecture

```
Browser ──HTTPS──► Cloudflare (edge TLS, WAF, cache)
                        │  HTTPS, "Full (strict)"
                        ▼
      ┌──────────── host :80 / :443 ───────────────────────────────────┐
      │  nginx container      routes by hostname, then by path         │
      │  certbot container    renews Let's Encrypt certs every 12h     │
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
| Cloudflare API token | for certbot DNS challenges, DNS records and Origin CA. Create it at **My Profile → API Tokens → Create Token → "Edit zone DNS"**. For Origin CA certificates, also add *Zone → SSL and Certificates → Edit*. Press Enter to skip. |
| Public IP | where DNS records point (detected automatically) |

Then it fetches Cloudflare's IP ranges, writes the config and offers to start the stack.

In the Cloudflare dashboard, set **SSL/TLS → encryption mode → Full (strict)** once.
`edge` never changes that setting.

---

## 3. Serving projects

### Add a domain: `edge domain add example.com`

1. **Certificate** for `example.com` + `*.example.com`. Let's Encrypt wildcard is recommended; Origin CA, custom and self-signed are also offered.
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
   - *Let's Encrypt*: a new certificate for this host from certbot. Free, and it renews itself.
   - *Cloudflare Origin CA*: a 15-year certificate that only Cloudflare trusts. Not offered for DNS-only hosts, because browsers would reject it.
   - *Custom certificate*: import a fullchain and key you already have, from file paths or pasted. The CLI checks that the key matches and that the certificate hasn't expired.
   - *Self-signed*: for testing only.
3. **Where traffic goes**: see below.
4. **A summary**, then *Go ahead?*

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
edge rm      api.example.com     # asks whether to delete the DNS record too (--purge-dns)
```

In the menu, **Routes → (route) → Change where it points** runs the add flow again for that route.

### Scripting (no prompts)

Every question has a flag. Off a terminal, the CLI fails with the flag it needs instead of prompting.

```bash
edge domain add example.com --cert letsencrypt --apex placeholder --dns proxied -y
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
edge app unlink <name>                      # removes the symlink only
```

- **Compose always runs against the real file**, with the project folder as the working directory. The build context, `./data` mounts, `.env` and the project name are exactly what they are when you run `docker compose` inside the project.
- **Only safe fields are read from a project's config**: the CLI reads `docker compose config` for service names, ports and networks only. Resolved configs contain secrets (env values), and the CLI never prints or stores them.
- **If a service joined the network through an edge override**, start that app with `edge app up`. A plain `docker compose up` in the project folder doesn't know about the override. To make it independent of `edge`, add the network to the project's own compose file (see `examples/docker-app.compose.yaml`).

---

## 5. Certificates

| Kind | Stored in | Renewal | Notes |
|---|---|---|---|
| Let's Encrypt | `data/letsencrypt/live/<name>/` | automatic: certbot runs every 12h, and nginx reloads every 6h | DNS-01 through Cloudflare, so no port 80 is needed and the proxy doesn't get in the way |
| Cloudflare Origin CA | `data/certs/<name>/` | valid for 15 years | host must be proxied; the token needs *SSL and Certificates → Edit* |
| Custom | `data/certs/<name>/` | yours | key match and expiry are checked on import |
| Self-signed | `data/certs/<name>/` | 90 days | testing only; Full (strict) rejects it (Cloudflare error 526) |

```bash
edge cert ls                     # type, names, days left (red under 14 days)
edge cert add api.x.com,*.x.com  # a certificate outside the domain/site flows
edge cert renew                  # renew what's due now, then reload
edge cert rm <name>              # refused while a host still uses it
edge cert aop <name> on|off      # Authenticated Origin Pulls (below)
```

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
│   ├── snippets/                #   proxy.conf, security-headers.conf, cloudflare-aop.conf
│   │   └── ssl/<cert>.conf      #   GENERATED
│   ├── sites/                   #   00-default.conf (hand-written) + <host>.conf (GENERATED)
│   ├── locations/<host>/        #   <path>.conf (GENERATED)
│   └── certs/                   #   Cloudflare origin-pull CA
├── templates/                   # what the generated files are made from
├── www/<dir>/                   # static sites → /var/www (read-only)
├── data/
│   ├── letsencrypt/             # certbot state and certs                 (git-ignored)
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
| Path app loads, but CSS/JS 404 | App isn't built with its base path (§3) |
| "nginx rejected the configuration" | The error names the file. Nothing was changed. A hand-written file in `conf/` is the usual cause. |
| Every visitor has the same IP | `edge cf-sync` |
