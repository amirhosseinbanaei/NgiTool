# AGENTS.md — NgiTool

Project memory for anyone (human or agent) working in this repository. Read it
first; update it when you change something it describes.

## What NgiTool is

NgiTool is a single static Go binary, `ngitool` (installed with a short alias
`ngt`), that manages **every nginx on a server** for **reverse proxy and
load balancing**:

- host-installed nginx (systemd or not, several masters),
- nginx in plain Docker containers,
- nginx inside Docker Compose projects,
- its own bundled "edge" nginx + certbot stack (`ngitool edge …`, assets
  embedded in the binary).

It finds every nginx, reads its config, writes routes and load-balanced pools
to the instances it was allowed to adopt, links and runs Docker Compose
projects as apps, runs its edge stack, issues certificates (Let's Encrypt
through certbot, Cloudflare Origin CA, custom, self-signed), keeps
Cloudflare DNS records, serves static folders, removes things with their
cascade, and migrates an nginx-edge directory. It replaced the Node `edge`
CLI (deleted in prompt 5; every legacy test has a Go test, see below).
Only commands that work are registered — never a "coming soon"
placeholder. TCP/UDP `stream` proxying, rate limiting, caching, WAF,
Kubernetes ingress and NGINX Plus features are out of scope for v1.0.0
(see "Next steps").

## Layout

```
cmd/ngitool/main.go     wiring only: os.Exit(cli.Main(os.Args[1:]))
internal/cli            one file per command group; app.go (root, exit codes,
                        the ordered `commands` list), menu.go (groups + menu
                        items registry), help.go (help template), doctor.go
                        (checks registry), instances.go (scan, instances,
                        inspect, the scan cache), update.go, updatecheck.go,
                        uninstall.go, completion.go, version.go, lock.go;
                        prompt 3: txn.go (env.change — the one path to
                        apply, validate, problem errors, instance choice),
                        adopt.go (instance adopt|release), routes.go (route
                        add wizard, ls, rm, enable, disable, edit), pools.go
                        (pool …, member …, drain, switch, check), ops.go
                        (diff, apply, rollback, test, reload), picker.go
                        (member sources + checklist), certs.go (existing
                        certificates on an instance);
                        prompt 4: apps.go (the `app` group: ls, show,
                        attach, unlink, fix, the hidden per-app menu, the
                        override writer), apps_link.go (link/scan
                        checklist, file and profile steps, "Serve it now?",
                        the Linked apps member source), apps_run.go
                        (lifecycle: explain, checklists, Will run, streamed
                        output, summary table), externalize.go (instance
                        externalize, CONF-06). Named apps*.go because
                        app.go is the root command;
                        prompt 5: edge.go (edge init|up|down|restart|status|
                        logs|cf-sync|upgrade-assets, adoptEdge, stackFiles),
                        certissue.go (where certificates go per instance,
                        issue: certbot in the stack or on the host, Origin CA,
                        custom, self-signed, the HTTP-01 preflight), certcmd.go
                        (cert ls|add|renew|rm|aop, certbot pass-through),
                        remove.go (rm, reset, www, domain: one removal flow),
                        migrate.go (migrate edge, backup); routes.go gained the
                        DNS step and "Issue a new certificate…", picker.go the
                        Static files source.
internal/model          routes, pools, members, adopted instances, certs,
                        domains, edge stacks (state.json schema 4);
                        remove.go (PlanRemoval: the cascade, extras); parse.go (hostnames + punycode, paths,
                        member specs, methods, sticky presets, Plus-only
                        refusals); check.go (every validation rule with its
                        edge-case ID); store.go (Load/Save)
internal/render         go:embed text/templates (templates/*.tmpl) →
                        files per instance layout; layout.go (Plan, LayoutOf,
                        HostPath, marker + hash header, Parse); facts.go
                        (version gates, resolver, gateway, maps, IPv6)
internal/apply          the transaction: tx.go (Run), files.go (managed-file
                        scan, staged writes), snapshot.go (backups, rotate,
                        restore), drift.go (APPLY-05), probe.go (HTTP probe,
                        502 causes, Cloudflare hints, health cache)
internal/nginxconf      lexer.go, parser.go (AST, Format), load.go (nginx -T
                        splitter, Source: DumpSource/FileSource, include
                        resolver), summary.go (servers, locations, upstreams,
                        resolver, stream, hook point). No dependencies.
internal/discover       scan.go (Scanner, steps), host.go (/proc, systemd,
                        binaries), docker.go (ps, inspect, top, candidates,
                        managers, edge), composedef.go (never-started
                        services), ports.go (ss → owners, front door),
                        config.go (load, capabilities, reachability),
                        findings.go, instance.go (Driver/Raw/Test for a
                        reported instance, the cache), model.go
internal/driver         Driver interface; Host and Container (dump, test, reload)
internal/compose        find.go (patterns, roles, the walk), project.go
                        (candidates merged with container labels, raw
                        summary, docker ps), read.go (Bin v2/v1, Project
                        args, `config` → Service summary, raw fallback),
                        app.go (App, override render + meta, snippet),
                        command.go (actions, options, exact argv), drift.go
                        (DOCK-07), stream.go (compose output → per-service
                        lines). Imports only execx and yaml.v3.
internal/edge           assets/ (compose.yaml, conf/, templates/ — go:embed),
                        edge.go (layout, Write, Outdated, .env, token file,
                        www/), stack.go (compose with -p, status, networks,
                        preconditions, EDGE-01/03, Image)
internal/certs          certs.go (kinds, coverage, PEM inspect/validate, Store
                        0600, self-signed, key + CSR), acme.go (HTTP-01
                        preflight, certbot arguments, renew output)
internal/cloudflare     the API v4 client (verify, zones, A records, Origin
                        CA), public IP, cf-sync (real-IP ranges, AOP CA)
internal/migrate        legacy.go (read an nginx-edge dir, the legacy renderer
                        ported for MIG-02), plan.go (mapping onto state),
                        compare.go (effective config: per port and host),
                        dryrun.go (scratch copy, render, nginx -t, compare)
internal/ui             theme.go (the ONLY colours), text.go (width, truncate,
                        cursor), components.go (+ DaysLeft), task.go (spinner,
                        steps), diff.go, prompt.go (huh prompts), ask.go
internal/paths          every path, NGITOOL_ROOT / NGITOOL_PREFIX
internal/state          atomic writes, flock, JSON stores with schema + migrations
internal/execx          process runner (docker, docker compose, nginx)
internal/version        build info (-ldflags), semver compare
internal/update         GitHub releases, download, verify, install, rollback
install.sh              curl | sh installer (POSIX sh)
.goreleaser.yaml        release build; .github/workflows/{ci,release}.yml
docs/ux.md              terminal UX rulebook
docs/edge-cases.md      master exception list, referenced by ID
docs/migration.md       the nginx-edge → NgiTool runbook
docs/examples/          a compose project and a systemd unit joining the edge stack
CHANGELOG.md            user-facing changes per release
```

Adding things later:

- a command → a constructor in `internal/cli/<group>.go`, appended to
  `commands` in app.go, with `group` and `synopsis` annotations;
- a group → `groups` in menu.go, plus its `menuItems`;
- a doctor check → `checks` in doctor.go.

### Dependency rule

Allowed: `spf13/cobra` (+ its `spf13/pflag`), `charmbracelet/lipgloss`,
`charmbracelet/bubbletea` + `bubbles`, `charmbracelet/huh`, `gopkg.in/yaml.v3`
(imported since prompt 2: the raw-YAML fallback when `docker compose config`
fails).
Anything else needs a line here saying why the standard library is not enough:

| Dependency | Why |
|---|---|
| `github.com/muesli/termenv` | Only to force lipgloss's Ascii colour profile under NO_COLOR/`--no-color`; lipgloss's API takes a termenv profile. Already compiled in through lipgloss, so it adds no size. |
| `golang.org/x/term` | TTY detection and terminal size. Go-team maintained; already required by bubbletea. |
| `github.com/spf13/pflag` | cobra's own flag package, imported directly to walk flags for the help page. |

Docker and nginx are always driven through their CLIs (`docker`,
`docker compose`, `nginx`) via `internal/execx` — never the Docker SDK, which
would roughly double the binary.

## Paths

| Path | What |
|---|---|
| `/etc/ngitool/config.json` | settings: `updateCheck`, `channel` (stable/prerelease), `scanRoots`, `frontDoor` (default instance for route add), `pinned`, `snapshots` |
| `/var/lib/ngitool/state.json` | what NgiTool manages: `instances` (adopted), `pools`, `routes`, `apps` (schema 3) |
| `/var/lib/ngitool/.lock` | exclusive flock held by every mutating command |
| `/var/lib/ngitool/backups/<instance>/<UTC time>/` | snapshot before every apply: `manifest.json`, `files/<n>`, `state.json`; the last 20 kept (config.json `snapshots`); `adopt-include/` holds the original of a hand-written file adopt edited |
| `/var/lib/ngitool/overrides/<app>.yaml` | NgiTool's compose override per app, always the last `-f` |
| `/var/lib/ngitool/externalized/<app>/` | an nginx config copied out of its image and bind-mounted back (CONF-06) |
| `/var/lib/ngitool/backups/migrate-<UTC time>/` | `migrate edge`'s copy of the directory's conf/, apps/, edge.json and .env, taken before anything changes |
| `/var/cache/ngitool/` | `update-check.json`, `scan.json` (the last scan: summaries only, never a dump), `health.json` (last probe per route, last `pool check` per member) |
| `/opt/ngitool/edge/` | the default edge stack directory (`edge init [dir]`); its layout is in internal/edge/edge.go and the README |

`NGITOOL_ROOT=/dir` moves all of them to `/dir/etc`, `/dir/lib`, `/dir/cache`.
`NGITOOL_PREFIX=/dir` makes the installer and `uninstall` use `/dir/bin`.
Files are 0600, directories 0700. Every document has a top-level `schema`
integer; `state.Store` runs migrations on load (SYS-07).

Other environment variables: `NGITOOL_RELEASES_URL` (releases JSON instead of
GitHub's API), `NGITOOL_DOWNLOAD_BASE` (mirror laid out as
`<base>/<tag>/<asset>`), `NGITOOL_VERSION` (install.sh), `GITHUB_TOKEN`
(sent to api.github.com only), `NGITOOL_NO_UPDATE_CHECK=1`, `NO_COLOR`.

## Discovery (prompt 2)

`ngitool scan` runs five steps (`discover.Scanner.Steps`): host processes,
Docker containers, compose projects, port owners, parsing configs. It only
reads. The report is cached in `scan.json`.

### Instance model (`discover.Instance`)

| Field | Meaning |
|---|---|
| `id` | stable across runs: `host:<unit>` or `host:<exe>[:<conf>]`, `ctr:<container>`, `compose:<project>/<service>`, `edge:<working dir>`; a clash gets `#2` |
| `kind` | `host`, `container` (plain `docker run`), `compose`, `edge` |
| `state` | `running`, `stopped`, `defined` (in a compose file, never created) |
| `variant`, `version` | from `-V`/`-v` (`nginx/1.30.5`, `openresty/…`, `Angie/…`, `Tengine/…`) |
| `owner` | host: the master's user; compose: owner of the working dir / compose file |
| `frontDoor` | owns :443, else :80 (via `ss`, docker-proxy → published port, or the pid) |
| `managedBy` | `edge` (the edge stack), `other:<tool>` (nginx-proxy-manager, nginx-proxy, swag), `none`; `ngitool` arrives with prompt 3 |
| `conf`, `source` | the main file as nginx sees it; `dump` (`nginx -T`) or `files` |
| `summary` | `nginxconf.Summary`: servers, locations, targets, upstreams, resolver, stream, hook |
| `reach` | per target file:line: ok / warn / err / unknown with a reason |
| `capabilities` | read, test, reload, write: `{ok, reason when false, note when true}` |
| `methods` | the exact dump / test / reload commands |

Edge detection: a compose project whose working dir has `compose.yaml`,
`conf/nginx.conf` and NgiTool's marker `.ngitool-edge` (written by `edge
init` and the migration) or the legacy edge CLI (`edge` or `cli/src`).

A container is a candidate when its image is nginx-like (any path part
`nginx`, `nginx-*`, `openresty`, `angie`, `tengine`, `swag`), when `docker top`
shows an nginx master (DISC-10), or, stopped, when its entrypoint/cmd names
nginx. `docker top` also gives the master's `-c`: the edge stack runs
`nginx -c /etc/nginx/edge/nginx.conf`, so a bare `nginx -T` would dump the
image's default config instead.

Host masters are `/proc/*/cmdline` titles `nginx|openresty|angie|tengine:
master process`; a master in another mount namespace (or a container
cgroup) is a container's, not the host's. The proc root is `Env.Proc` so
tests use a fake tree.

### Driver contract (`internal/driver`)

`Dump(ctx)` returns `nginx -T` output (parsed in memory, never logged or
cached whole, CONF-11). `Test(ctx)` returns `Result{OK, Output, File, Line}`;
its error only means the test could not run. `Reload(ctx)`'s error names the
method and its output. `Describe()` gives the three commands.

| Kind | Dump | Test | Reload |
|---|---|---|---|
| host | `<exe> [-p] [-c] [-g] -T` | same with `-t` | `systemctl reload <unit>` when the master is in one, else `<exe> … -s reload` |
| container, running | `docker exec <name> <bin> [-c] -T` | `… -t` | `docker exec <name> <bin> [-c] -s reload` |
| container, stopped | none (files are read through the mounts) | `docker run --rm --network none --pull never -v src:dst:ro … --entrypoint <bin> <image> -t` | none: nginx reads its config on start |

Every test output goes through `driver.Clean` (drops "signal process started"
and the worker_connections warning). In prompt 2 only Dump and Test run for
real; Reload is unit-tested with `execx.Fake`.

### Capability rules

- **read**: config files were obtained (dump or files). A stopped container
  whose config is not on a mount: false, "inside the image" (CONF-06).
- **test**: false for `defined` services and when Docker is unusable.
  Stopped containers test in a throwaway container.
- **reload**: false for other managers, stopped/defined instances, an
  invalid config (CONF-03), and a host instance without root.
- **write**: true only when the hook directory (or the file needing the
  include line) is on the host: a host path (root needed), or a container
  bind mount / named volume covering it. A `:ro` mount does not block it —
  the note says `:ro` only stops the container from writing. Single-file
  mounts are edited in place (CONF-07); a mounted `/etc/nginx/templates`
  means edits go to templates (CONF-08). False for other managers (DISC-11),
  defined services (DISC-09), invalid configs (CONF-03), non-UTF-8 files
  (CONF-10), baked-in configs (CONF-06), no `http {}`.
- **hook point** (`Summary.Hook`): an include of `conf.d/*.conf` (preferred)
  or `sites-enabled/*` directly inside `http {}`; otherwise "needs one include
  line in <file>:<line of http {>". Recorded only; prompt 3 writes it.
- When only `conf.d` is mounted into an official nginx image and the
  container is stopped or never created, the main file is taken to be the
  image's stock `nginx.conf` (`stockMain`) and the instance says so.

### Parser limits (`internal/nginxconf`)

- Follows `ngx_conf_read_token`: `#` is a comment only at a token start, `}`
  does not end a bare word, `{` right after `$` stays in the word (`${var}`),
  escapes `\" \' \\ \t \r \n` in quotes. Quoted directive names are kept.
- `*_by_lua_block` bodies are raw text (Lua strings, long brackets and
  comments are skipped when matching braces). Other non-nginx bodies are not
  special-cased.
- Comments are dropped; `Format` round-trips the tree, not the bytes.
- Includes: relative to the main file's directory; globs sorted byte-wise
  like glob(3); a missing non-glob include is an error, an empty glob is
  not; nesting capped at 32. Symlinks are resolved inside the container /
  chroot (`FileSource.Resolve`), so Debian `sites-enabled` links work from
  outside; `FileInfo.Link` keeps the target.
- Variables in `proxy_pass` are resolved only through a literal `set` in
  the location or server, or listed as the values of a simple `map`
  (CONF-05). Everything else is "decided per request".
- `if` blocks are parsed but their `proxy_pass` is not treated as a
  location target.

### Findings

error: CONF-03 (invalid config), RP-05 (target container missing or not on
a shared network). warn: RP-03, RP-04, RP-07, DISC-15 (two instances on one
host port), DOCK-11 (0.0.0.0 publishes, except the front door's 80/443),
DISC-13, DISC-14. info: DOCK-15 (double proxy, who terminates TLS), DISC-01,
DISC-11, DISC-15 (nobody on :80/:443, or not an nginx), DISC-16, DISC-17,
RP-14.

## Routes, pools and apply (prompt 3)

### Model (`internal/model`, state.json schema 2)

- **Route** `{id (host or host/path), instance, host, path ("" = whole
  host), www, pool, enabled, http (redirect|serve), tls {name, cert, key,
  names} or none, options, extra}`. Options: strip prefix, websocket (on),
  buffering (on), body size, connect/read/send timeouts, extra headers,
  upstream TLS {verify, serverName, ca}, Host header ("" = `$host`,
  `$proxy_host`, or a name).
- **Pool** `{name, instance, method, hashKey, consistent, sticky (preset
  label), scheme (http|https|grpc|grpcs, one per pool), members, keepalive,
  nextUpstream {conditions, tries, timeout}, errorPage, previous (blue/green),
  extra}`. Upstream name `ngt_<pool>` (LB-13). Pool names are global.
- **Member** `{kind: container | compose-service | host-port | address |
  unix, ref, host (a DNS name or alias, never an IP — DOCK-16), port, weight,
  backup, down, maxFails, failTimeout, maxConns, app}`. As a flag:
  `container:NAME:PORT`, `service:PROJECT/SERVICE:PORT`,
  `app:APP/SERVICE:PORT` (prompt 4: stored as compose-service with `app`,
  `ref` `<project>/<service>`, `host` the alias once attached), `port:PORT`,
  `HOST:PORT` / `addr:` / `https://HOST`, `unix:/path`, then
  `,weight=N,backup,down,max_fails=N,fail_timeout=T,max_conns=N`.
- A single-target route is a pool of one member named after the host
  (`api.example.com/v1` → `api_example_com_v1`); users meet pools only when
  they add a second member.
- **Adopted** `{id, kind, layout, root (nginx path), hostRoot, include
  {file, line, text, backup} or none, adoptedAt}`.
- Validation (`CheckRoute`, `CheckPool`) runs on the next state before
  anything renders; every Problem carries its edge-case ID, a level (error,
  warn, note) and a fix. The CLI prints the ID muted: `… (LB-02)`.

### Render layouts (`internal/render`)

Every file starts with the marker line and `# ngitool: <id> sha256:<body>`
(ids: `instance`, `snippet`, `pool:<name>`, `route:<host>`,
`route:<host/path>`); a body whose hash no longer matches is drift.

| Layout | When | Files |
|---|---|---|
| host | host nginx | `/etc/nginx/ngitool/{upstreams,servers,locations/<host>}/`, `proxy.conf`, entry `<hook dir>/ngitool.conf` (or `/etc/nginx/ngitool/ngitool.conf` behind an adopted include line) |
| edge | the edge stack | `sites/<host>.conf`, `locations/<host>/<slug>.conf`, `ngitool/upstreams/`, entry `conf.d/ngitool.conf`, `snippets/ngitool-proxy.conf`; edge-CLI files are left alone; reuses `conf.d/websocket.conf`'s map; includes `security-headers.conf` and `acme-challenge.conf` when present |
| mount | container with a bind-mounted conf dir | `<mount>/ngitool/{upstreams,servers,locations}/`, `proxy.conf`, entry `<mount>/ngitool.conf` |

- nginx ≥ 1.27.3 (Angie always): `upstream ngt_x { zone ngt_x <64k+>; <method>;
  resolver …; server name:port resolve …; keepalive N; }` — names resolve at
  runtime, so nginx starts while a member is down (RP-07, verified by hand).
  Older: one member → `set $ngt_upstream …; proxy_pass http://$ngt_upstream`
  with a server-level resolver; several → static server lines and the warning
  "nginx will refuse to start if a name does not resolve." (LB-03).
- Resolver: `127.0.0.11` for containers, the host's `/etc/resolv.conf`
  nameservers for host nginx and host-network containers. Host ports from a
  container go through the network's gateway (RP-06), from the host to
  127.0.0.1.
- `$connection_upgrade`: an existing map that gives `""` without Upgrade is
  reused; one that gives `close` makes NgiTool render its own
  `$ngt_connection_upgrade`; none → NgiTool renders the map (LB-06).
- TLS servers get `http2 on` (≥ 1.25.1, else `listen … ssl http2`), HSTS on
  redirect, `max-age=0` on serve, and a `ssl_reject_handshake` default server
  in the entry when the instance has no default server on 443 (RP-18).
- Path routes: `location = /p { return 301 /p/…; }` + `location /p/`;
  strip is `rewrite ^/p/(.*)$ /$1 break;` (RP-23). A path route on a host an
  existing edge site serves only drops its location file into that site's
  `locations/<host>/` (RP-20 then warns about regex locations there).
- Adopted hand edits (APPLY-05) are stored as `extra`: route lines go into
  the location (`server:`-prefixed ones into the server), pool lines into the
  upstream (`location:`-prefixed ones into the locations).
- `render.Mutate` exists only so the integration test can break a file.

### Transaction (`internal/apply.Run`, via `cli.env.change`)

1. lock (`withLock`); state is re-loaded under the lock and the command's
   change re-applied to a fresh copy;
2. render the instance's full file set; a hand-written file at a target path
   is a hard error (CONF-09);
3. drift: managed files whose hash no longer matches are diffed (what
   NgiTool wrote → disk) and the user picks overwrite, adopt or abort
   (`--on-drift` off a terminal);
4. coloured unified diff grouped by file, summary "N files changed, N added,
   N removed", notes; `--dry-run` stops here (APPLY-08);
5. confirm unless `--yes`;
6. snapshot every managed file, every target path, the include file and
   state.json (APPLY-06);
7. stage every file next to its target (fsync), then rename them all; a
   failure while staging changes nothing, a failure while swapping restores
   the snapshot; the error names disk full / read-only (APPLY-07, APPLY-09);
8. `nginx -t` through the driver; a failure restores the snapshot and prints
   nginx's error with the offending file:line highlighted (APPLY-01);
9. reload; for containers `docker logs --since` is read for `[emerg]` lines
   (a bind failure after a passing test, APPLY-10); a failure restores the
   snapshot, reloads again and reports both outputs (APPLY-03). A stopped
   instance is tested with `docker run --rm` and not reloaded (APPLY-02);
10. probe each touched route through the instance's own listener with its
    Host header (Go HTTP, TLS unverified); anything but 502/503/504 counts as
    reached; a failure prints the likely causes per member kind; no published
    port → skipped with a hint;
11. save state.json, rotate snapshots.

`rollback` is the same transaction with the snapshot's files as the target
set; only that instance's routes, pools and adoption come back from the
snapshot's state.json, other instances keep today's.

### Adopt rules

- Required once per instance before any write; `instance release` undoes it.
- Plan first: layout, directory (nginx and host path), and whether the
  include already exists (`conf.d/*.conf` or `sites-enabled/*` inside http).
- Otherwise one line `include <root>/ngitool.conf;  # NgiTool …` goes right
  after the `{` of http in the hand-written file: its own Explain + confirm,
  a backup in `backups/<instance>/adopt-include/`, written in place for a
  single-file mount (CONF-07), CRLF kept (CONF-10). Release removes exactly
  that line. It is the only edit NgiTool ever makes to a file it did not
  write.
- `write: false` instances are refused with the reason and what they would
  need (CONF-06: for a compose service `ngitool instance externalize <id>`
  copies `/etc/nginx` to `/var/lib/ngitool/externalized/<app>/` and mounts
  it through the app's override, linking the project first if needed);
  other managers are refused (DISC-11).

### Member picker

`memberSources` in picker.go is a list of `{Title, Rows(state, report,
instance)}`: linked apps (prompt 4, `<app>/<service>` with state, ports and
a network chip), compose services (from running containers' labels; a
linked app's services are not listed twice), plain containers, host ports
(from `ss`, with the process or `docker-proxy → container`). Shared-network
rows sort first; host-network instances get container rows disabled with
the RP-05 reason; the last row is "✎ Enter an address manually…".

## Compose apps (prompt 4)

### Finder (`compose.Scan`, `compose.Merge`)

- Roots: config.json `scanRoots` (default `/home/*`, `/root`, `/opt`,
  `/srv`, globs allowed), depth 4 below each root. `docker/`, `deploy/`,
  `.docker/` and `infra/` are looked into even one level past the limit
  (DOCK-01). Skipped: node_modules, vendor, dist, build, .git, .next,
  .cache and every other hidden directory.
- Patterns, case-sensitive like Docker: `compose.y(a)ml`,
  `docker-compose.y(a)ml` (base); `compose.*.y(a)ml`,
  `docker-compose.*.y(a)ml`, `*.compose.y(a)ml` — `override` anywhere in
  the middle word is an override, any other word an env variant (dev, prod,
  staging, local, …). Files a container label names that match nothing
  (the legacy `edge.override.yaml`) are `extra`.
- Merge order shown and pre-selected: base (compose.* before
  docker-compose.*), overrides, variants by word, label-only files.
  Default `-f` set: the running label (authoritative, DOCK-02), else the
  first base plus its overrides, else the only file.
- Symlinked directories are never followed (no loops, a project is found
  once where it lives). A symlinked compose file that resolves into another
  directory is a pointer: that directory is read instead (the legacy
  `apps/<app>/compose.yaml` links). A symlink to a missing file and an
  unreadable directory are candidates disabled with the reason (DOCK-10).
- Candidates are merged with `docker ps -a` labels: project (`-p`, DOCK-03),
  working dir, config_files. A project the walk did not reach is still
  listed from its labels; label files that are gone are "missing — last
  known path" (DOCK-10).
- Name: the label, else `name:` in the first file, else the folder
  (normalised like compose). Owner: the file's uid. State: running N of M,
  stopped, never started (raw YAML gives services, networks and ports).
  nginx badge: an nginx-like image, or an instance from the last scan
  (DISC-10). The edge stack's directory is listed disabled: it is run by its
  own commands (prompt 5), never as an app.

### App model (state.json `apps`, schema 3)

`{name, projectName, workingDir, files[] in merge order, profiles[],
envFiles[] (only when chosen), overridePath, attachedServices {service:
{network, alias, keys, manual}}, mounts {service: [{source, target,
readOnly}]}, owner, linkedAt, lastAction}`. The name defaults to the
project name, deduplicated `-2`, `-3` (cli/src/flows.mjs:829-833). It
replaces the legacy `apps/` symlink folder; prompt 5 imports that folder.

### Override rules

- `/var/lib/ngitool/overrides/<app>.yaml` (0600), rendered from the app
  (`compose.RenderOverride`); the project's own files are never edited.
  It exists while a service is attached (not manually) or a config is
  externalized, and is deleted when nothing needs it or the app is unlinked.
- Line 4 is `# ngitool: {"app":…,"services":…,"mounts":…}` (`ParseMeta`
  reads it back). Each attached service lists its own network keys as
  `key: {}` (so it never falls off `default`) and joins `ngt_<network>`
  (external, `name: <network>`) with the alias `<app>-<service>`. Routes use
  that alias, never an IP (DOCK-16).
- "I'll edit the compose file" prints `ManualSnippet` and records the
  attachment as `manual` (left out of the override; drift checks only the
  network).
- `network_mode: host | none | service:x | container:x` cannot join: the
  attach is refused with DOCK-06 and the host-port alternative.
- A route (or `pool member add`) to a service off the instance's network
  attaches it here instead of `docker network connect`: on a terminal the
  choice override/snippet, then "Recreate now?" with the explain panel and
  `up -d --no-deps <service>`; off a terminal only with `--connect` (then
  override and recreate). `app attach` does the same on its own.

### Every compose run

`<bin> -p <project> --project-directory <dir> -f <file>… [-f <override>]
[--env-file …] [--profile …] <step>`, cwd `<dir>`, `COMPOSE_PROJECT_NAME`
and `COMPOSE_FILE` scrubbed (DOCK-08). `<bin>` is `docker compose`, or
`docker-compose` with a one-time warning when only v1 exists (DOCK-05).
`config --format json` is decoded without `environment`, labels or secrets
(DOCK-04); on failure compose's last 3 lines are shown and the files are
read as raw YAML, marked unresolved.

### Actions (`compose.Steps`, golden-tested)

| Action | Steps after the global args | Options (flag) |
|---|---|---|
| up | `up -d [--build] [--pull always] [--no-deps] [--remove-orphans] [svc…]` | --build, --pull, --no-deps, --remove-orphans |
| restart | `restart [svc…]` | – |
| recreate | `up -d --force-recreate [--no-deps] [svc…]` | --no-deps |
| rebuild | `build [--no-cache] [--pull] svc…` then `up -d [--no-deps] svc…` (services with `build:` only) | --no-cache, --pull, --no-deps |
| pull | `pull svc…` then `up -d svc…` (image-only services; the others are skipped with a note) | – |
| stop | `stop [svc…]` | – |
| down | `down [--volumes] [--remove-orphans]` (whole app) | --volumes, --remove-orphans |
| logs | `logs [--follow] --tail N [--since T] [--timestamps] [svc…]` | --no-follow, --timestamps, --tail, --since |
| ps | `ps -a --format json` → the summary table | – |
| fix | `up -d --force-recreate --no-deps <detached services>` | – |

Before running: heading, Explain (What it does / What it affects / How to
undo — the text in `compose.Actions`), service checklist (all by default),
options checklist (one line each, ticked from flags), "Will run:" with
every command exactly, then the confirm: `--yes` off a terminal;
`--volumes` lists the volumes and needs the typed project name (`--yes
--force`); `--remove-orphans` needs the typed name on a terminal and
`--yes` off one (DOCK-14). Output streams with a coloured service prefix
(`ui.Series`); afterwards `lastAction` is recorded and a table shows
container, state, health, on the proxy network, routes and a probe of each
route through its instance. `logs` holds Ctrl-C for the child
(`holdInterrupt`), so it returns to the menu.

### Drift (`compose.Check`, DOCK-07)

Per linked app, from `docker ps -a` labels: `missing` (files or dir gone,
DOCK-10), `not attached` (nothing attached), `stopped` (nothing running),
`detached` (a running attached service whose config_files lack the
override, or that is not on its network: "started without NgiTool's
override (plain docker compose up?) — its routes return 502"), else `ok`.
Shown by `app ls` (red chip + `ngitool app fix <app>`), `route ls` (for apps
with routes), doctor's "Linked apps" check (fails), and after every action.
There is no `status` command yet; doctor carries it.

## Edge stack, certificates, migration (prompt 5)

### Edge stack (`internal/edge`, `cli/edge.go`)

- Assets are embedded (`internal/edge/assets`, `go:embed all:assets`):
  compose.yaml, conf/ (nginx.conf, start.sh with the 6 h reload loop,
  conf.d/, snippets/, sites/00-default.conf), templates/ (the legacy
  templates, used by the migration's legacy renderer and for placeholder
  pages), env.example, secrets/cloudflare.ini.example. `edge init` writes
  compose.yaml and conf/ (not templates/: nothing reads them from disk any
  more), the directories, `.ngitool-edge` (the marker discovery recognises
  next to the legacy `edge`/`cli/src`), .env (0600) and
  secrets/cloudflare.ini (0600 in a 0700 dir). Existing files that differ
  are kept and reported; `edge upgrade-assets` diffs and replaces them.
- Machine state is never replaced by upgrade-assets: conf/sites,
  conf/locations, conf/snippets/ssl, conf/certs, conf.d/cloudflare-realip.conf,
  NgiTool's own files, data/, secrets/, www/, .env.
- `state.json` `edges` records `{dir, project, instance}`; the stack's nginx
  is instance `edge:<dir>`, adopted automatically after `edge up` (layout
  edge), so routes, pools and the transaction work on it unchanged.
- Every stack command: `docker compose -p <project> --project-directory <dir>
  -f <dir>/compose.yaml …`, COMPOSE_PROJECT_NAME/COMPOSE_FILE scrubbed. The
  project comes from `--project-name`, else $COMPOSE_PROJECT_NAME at init,
  else `edge`; a non-default one is also written to .env so a manual
  `docker compose` in the directory finds it.
- `edge up` checks EDGE-03 (another project runs from the dir), EDGE-01
  (ports held by someone else, from the scan's listeners), then the legacy
  preconditions (network — EDGE-02, created on request; cloudflare.ini a
  file; data/acme), then `up -d --quiet-pull --remove-orphans`, then adopts.
- cf-sync and upgrade-assets write the stack's own nginx files through
  `apply.Change.Raw` (snapshotted, tested, reloaded, restored on failure);
  only `edge init`, before anything runs, writes them directly.

### Certificates (`internal/certs`, `cli/certissue.go`, `cli/certcmd.go`)

- state.json `certs`: `{name, instance, kind, challenge, names, aop, cert,
  key (nginx paths), hostCert, hostKey, certbot (stack|host),
  authenticator, webroot}`. A route's `tls` copies name, paths, names, aop.
- Where files go: edge stack — data/letsencrypt/live/<name>/ (certbot) and
  data/certs/<name>/ (others), seen by nginx as /etc/letsencrypt/live and
  /etc/edge-certs; host and mount layouts — `<adopted root>/certs/<name>/`.
  Keys 0600, dirs 0700.
- Let's Encrypt on the stack: `run --rm --no-deps --entrypoint certbot
  certbot certonly …` (webroot /var/acme or DNS-01 with
  /secrets/cloudflare.ini). HTTP-01 is preflighted first (needs the stack
  running and data/acme mounted). On host nginx: host certbot, nginx plugin
  or `--webroot` (CERT-05); without certbot the options are disabled with
  the install command.
- Self-signed (ECDSA P-256, 90 days) and the Origin CA key + CSR (RSA 2048,
  origin-rsa, 15 years) are made in Go: no openssl needed.
- The route wizard's TLS step lists NgiTool's certificates, then the ones
  found on the instance, then "Issue a new certificate…" (`--issue KIND`);
  a new certificate is issued after its own confirm and deleted again when
  the route's transaction fails or is a dry run.
- AOP: `cert aop <name> on|off` sets it on the cert and every route using
  it; the edge layout renders `include <root>/snippets/cloudflare-aop.conf`
  in the server.

### Cloudflare (`internal/cloudflare`)

- Endpoints are package variables (tests point them at httptest). The token
  is read from the instance's stack (secrets/cloudflare.ini), another
  stack's, or $CLOUDFLARE_API_TOKEN, sent only as the Authorization header
  to the API base, never printed or stored in state.
- The route wizard's DNS step appears only when the instance's stack has a
  token, only for whole-host routes: proxied / dns-only / skip (`--dns`),
  stored as `route.dns`. For HTTP-01 the record is written before certbot
  runs, otherwise after the transaction. UpsertA refuses to replace a CNAME.

### Static routes (EDGE-04)

- Member kind `static`, Ref a folder under www/ (edge) or an absolute dir
  (host); a static pool has exactly one member and renders no upstream.
  Rendered as the legacy body-static/path-static templates (root + SPA
  fallback; alias for paths). New folders get templates/index.html.tpl.

### Removal (`model/remove.go`, `cli/remove.go`)

- `PlanRemoval(state, target, wwwExists)` is pure: routes (a host takes its
  paths; a domain its hosts but not those of a more specific domain; an app
  its routes, or only its members in shared pools; a certificate its routes
  unless `--cert-to` moves them; a folder the routes serving it), pools left
  without routes, and the extras left unused (certs, folders, DNS records,
  app containers). `ApplyOn(next, instance)` is the nginx part of one
  instance (one transaction each), `ApplyState` the rest (after nginx
  accepted). Extras: `--purge` all, `--purge-dns` DNS, a terminal asks
  (certs pre-ticked); kept ones are named afterwards.

### Migration (`internal/migrate`, `cli/migrate.go`)

- Read: .env, edge.json (certs, domains, sites, paths incl. enabled:false,
  http:"serve", strip, aop, challenge), apps/<name>/ (symlink, override and
  its `# edge: {...}` line), whether a token exists (never its value), and
  the route files split into `# Managed by edge` and hand-written.
- Map: sites/paths → routes + single-member pools (app → compose-service
  of the linked app with Host = the legacy upstream, container, port →
  host-port, static); certs registered in place; domains keep zone ids; apps
  linked with the same files, project from the running label, else `name:`,
  else the folder; overrides move to /var/lib/ngitool/overrides/<app>.yaml.
- Dry run: copy conf/ to a scratch dir, drop the legacy files there, render
  NgiTool's files (the legacy files count as NgiTool's own for RP-03 and the
  attach logic), nginx -t with the stack's image and mounts (`docker run
  --rm --network none --pull never`), then `Compare(Effective(old),
  Effective(new))`: for every port and exact host name, the server nginx
  would pick, its cert/key, ssl_verify_client, return, and each location's
  normalised behaviour (proxy targets through `set` and upstream blocks).
  Differences, drift (MIG-02), nginx -t failures and mapping errors are
  blockers; RP-03/RP-04 conflicts that exist today are warnings.
- Real run: the same checks, a full backup, then one transaction with
  `Change.Legacy` (the legacy files may be replaced or removed), the
  mapping re-applied to the locked state, and every route probed. Then the
  overrides, the marker, and edge.json → edge.json.migrated.

### Legacy tests → Go tests

Every test of the deleted `cli/test/run.mjs`, and where its behaviour is
proven now:

| Legacy test | Go test |
|---|---|
| parses commands, values and booleans | cobra/pflag; cli TestMissingValueOffTTYNamesTheFlag, TestUnknownFlagIsUsageError |
| --no-<flag> sets a boolean false; unset stays undefined | reason: NgiTool has explicit `--no-*` flags (`--no-strip`, `--no-www`, `--no-websocket`, …) parsed by pflag; covered where used (cli TestRouteLifecycle) |
| rejects unknown options and missing values | cli TestUnknownFlagIsUsageError, TestMissingValueOffTTYNamesTheFlag |
| parseTarget normalises scheme, case and slashes | model TestLegacyTargets, TestHostnamesAndPaths |
| wildcards cover exactly one level | certs TestWildcardsCoverExactlyOneLevel |
| domainOf picks the longest configured suffix | certs TestDomainOfAndDepth |
| sourceFromFlags builds each source type | model TestLegacySourcesAreMembers (app, container, port, static); edge TestStaticFolders (www/ normalisation, no escapes). "Choose one source" is replaced: two `--to` make a pool |
| validatePath refuses the root and odd characters | model TestLegacyTargets (the root is the whole host in NgiTool, not an error) |
| parseEnv reads values, strips quotes, skips comments | edge TestParseEnv |
| setEnvText replaces in place, uncomments, or appends | edge TestSetEnvText |
| render drops lines that hold only an empty token | edge TestFillDropsLinesOfEmptyTokens |
| buildFiles writes one file per cert, enabled site and path | migrate TestLegacyBuildFiles (the legacy renderer); render goldens for NgiTool's own files |
| apex gets www when its cert covers it; upstreams and certs are wired | migrate TestLegacyBuildFiles, TestMappingOfEverySourceType (WWW, TLS); render golden apex-www, edge-static-and-aop |
| path routes: strip adds a rewrite, keep does not | migrate TestLegacyBuildFiles; render golden path-strip-and-keep, edge-static-and-aop (alias) |
| buildFiles refuses a site whose cert is unknown | migrate TestLegacyBuildFiles; MIG-01 blocker in plan.go |
| writeFiles syncs managed files and never touches hand-written ones | apply TestTestFailureRestoresTheSnapshot, TestDriftIsShownAndAsked and the CONF-09 check in apply.Run; migrate TestDryRunVerdict (hand-written files kept) |
| summarizeServices reads ports, networks and aliases only | compose TestSummarizeServices |
| override keeps existing networks and adds the edge one | compose TestOverrideRenderAndRoundTrip |
| finds compose files and derives app names | compose TestScanFixtureTree, TestClassify |
| removing a host takes its paths; unused cert, folder and DNS become extras | model TestRemovingAHostTakesItsPaths |
| removing a domain keeps hosts of a more specific domain | model TestRemovingADomainKeepsHostsOfAMoreSpecificDomain |
| removing an app drops every route that points at it | model TestRemovingAnAppDropsEveryRouteToIt |
| removing a cert: move its hosts to a covering cert, or drop them | model TestRemovingACertMovesOrDropsItsHosts |
| removing a static folder drops the routes serving it; reset drops everything | model TestRemovingAStaticFolderAndReset; cli TestRemovalNeedsForceOffATerminal |
| writeFiles removes empty location dirs of hosts that are gone | apply removeEmptyDirs, exercised by cli TestRouteLifecycle (route rm) |
| letsencrypt entries without a challenge are DNS-01; labels say which | certs TestChallengeAndLabel; migrate TestMappingOfEverySourceType |
| plain HTTP: serve adds a :80 server and turns HSTS off for that host only | migrate TestLegacyBuildFiles; render golden tls-serve-no-hsts |
| shared snippets: no HSTS in security-headers, scheme-aware X-Forwarded-Proto | edge TestSharedSnippets |
| generated sites answer ACME challenges | migrate TestLegacyBuildFiles; render golden layout-edge, edge-static-and-aop (also on HTTPS) |
| decode turns escape sequences and control keys into names | reason: the hand-written key decoder is replaced by bubbletea/huh; NgiTool's prompt behaviour is checked in a private tmux (docs/ux.md, SYS-09) |

## UX rules

See [docs/ux.md](docs/ux.md). In short: colours only through the roles in
theme.go; plain output has the same content; every prompt collapses to
`✔ question › answer`; Esc goes back, Ctrl-C exits 130; every prompt has a
flag and fails naming it off a terminal; `--yes` skips confirms but a typed
danger confirm needs `--yes --force`; every list/inspect command has `--json`;
typing is the last resort — discovered lists end with "✎ Enter it manually…".

## Non-negotiables

- Static binary, `CGO_ENABLED=0`, under the size budget (14.5 MB for
  linux/amd64 since prompt 5, `make size`); the dependency rule above.
- A project's own compose files are never edited; every compose run passes
  `-p`, `--project-directory`, every `-f` and the override, with
  `COMPOSE_PROJECT_NAME`/`COMPOSE_FILE` scrubbed.
- One transaction path for every nginx write (`apply.Run`); only files with
  the NgiTool marker are rewritten or deleted; the adopted include line is
  the only edit to a hand-written file; state is saved only after nginx -t
  and reload succeed; no NGINX Plus directive is ever rendered.
- Docker and nginx only through their CLIs via `internal/execx`.
- Every interactive step has a flag equivalent and fails clearly off a TTY.
  Every list or inspect command has `--json`.
- No raw colour codes outside `internal/ui/theme.go`. NO_COLOR is honoured everywhere.
- Never print or store resolved compose environment or secrets.
- Never print or store the Cloudflare token or private keys; secrets files
  are 0600 in 0700 directories.
- During development never touch the live stack (`/root/srv/nginx`), running
  containers of other projects, host nginx, `/usr/local/bin`, or the real
  `/etc/ngitool`, `/var/lib/ngitool`, `/var/cache/ngitool`. Tests use
  `NGITOOL_ROOT` and `NGITOOL_PREFIX` in a scratch directory.
- Keep server IPs, real domains, usernames and project names out of tracked
  files, including fixtures and doc examples. Use `example.com` and made-up
  project names.
- Never `--no-verify`. Never commit to `main`.

## Verification gate

Both must pass before any commit:

```bash
make check              # test -z "$(gofmt -l .)" && go vet ./... && go test ./... && make build && make size
NGITOOL_IT=1 go test ./...   # real Docker: discover (ngitool-it-scan, :18079); cli (ngitool-it-front on
                             # 127.0.0.1:18080, ngitool-it-b1..b3, compose projects ngitool-it-app1/app2,
                             # network ngitool-it; the edge stack ngitool-it-edge on :18080/:18443 with
                             # network ngitool-it-edge and backend ngitool-it-edge-b1); migrate (nginx -t
                             # of a migrated fixture with nginx:stable-alpine)
```

## Testing safely

```bash
S=$(mktemp -d)                                     # or the session scratchpad
export NGITOOL_ROOT=$S/root NGITOOL_PREFIX=$S/prefix NGITOOL_NO_UPDATE_CHECK=1
make build && ./dist/ngitool doctor
```

The release flow end to end, without GitHub:

```bash
for v in v0.1.0 v0.2.0; do make release-local VERSION=$v; done   # dist/release/<tag>/
# releases.json: [{"tag_name":"v0.2.0","body":"…"},{"tag_name":"v0.1.0"}] next to the tag dirs
python3 -m http.server 18765 --bind 127.0.0.1 --directory dist/release &
export NGITOOL_DOWNLOAD_BASE=http://127.0.0.1:18765 NGITOOL_RELEASES_URL=http://127.0.0.1:18765/releases.json
NGITOOL_VERSION=v0.1.0 sh install.sh                # into $NGITOOL_PREFIX/bin
$NGITOOL_PREFIX/bin/ngitool update --check          # exit 10
$NGITOOL_PREFIX/bin/ngt update --yes                # → v0.2.0, v0.1.0 kept as ngitool.prev
$NGITOOL_PREFIX/bin/ngitool update --rollback --yes
$NGITOOL_PREFIX/bin/ngitool uninstall --purge --yes --force
```

Interactive screens are checked in a private tmux server
(`tmux -L <name> new-session -d -x 100 -y 30`, `send-keys`, `capture-pane -p -e`).

The edge stack in a sandbox (never ports 80/443, never the networks `edge`
or `proxy`):

```bash
ngitool edge init $S/edge --network ngitool-demo --email admin@example.com --no-token \
  --http-port 18080 --https-port 18443 --project-name ngitool-demo --start --yes
# … then: docker compose -p ngitool-demo --project-directory $S/edge down; docker network rm ngitool-demo
```

A migration is tried only on a copy: `cp -a <dir> $S/copy && ngitool migrate
edge $S/copy --dry-run`, then delete the copy (it holds private keys and the
token).

## Toolchain

Go **1.27.1**, installed 2026-09-30 from the official
`go1.27.1.linux-amd64.tar.gz` (go.dev/dl), SHA-256
`63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445` verified,
extracted to `/usr/local/go`. Use `export PATH=$PATH:/usr/local/go/bin` (the
Makefile adds it). `go.mod` says `go 1.27.1`; CI reads it via
`setup-go go-version-file`. GoReleaser 2.18.2 was used once from a scratch
directory to validate `.goreleaser.yaml` (`goreleaser check` and a
`--snapshot` build); it is not installed on the server.

## Decisions

Assumptions from the plan (all held):

- The GitHub repo will be renamed to `amirhosseinbanaei/NgiTool` by the owner
  (GitHub redirects the old name). Code, installer and update URLs already
  use the new name. As of 2026-09-30 the API answers 404 for both names, so
  `doctor` shows the update source as a warning until the repo is public
  with releases.
- The command is `ngitool`; install.sh creates the `ngt` symlink.
- Size: `dist/ngitool` (linux/amd64, stripped, trimpath) is **11,002,016
  bytes** (~11.0 MB), under the 12,000,000-byte budget. Release archives are ~4.3 MB
  (amd64), ~3.9 MB (arm64), ~4.1 MB (armv7).
- Nothing is pushed, tagged or released from this server (its SSH key is
  rejected by GitHub).

Open decisions, and the choice made:

- **Scan cache (prompt 2): 60 s for commands, always fresh for `scan`.**
  `instances`, `inspect` and doctor's front-door/configs checks reuse
  `scan.json` when it is under 60 s old (`--fresh` skips it); `scan` always
  scans and refreshes it (`--fresh` is accepted and changes nothing). The
  cache is 0600 and never holds a dump.
- **Managed-by-other instances in `instances`: shown**, with WRITE
  "read-only (<tool>)", because hiding them would hide real port conflicts.
- **.deb packages via GoReleaser nfpms: not now.** Tarballs + install.sh are
  enough and keep the pipeline small. Revisit if someone needs apt-managed
  upgrades.
- **Update signatures: SHA-256 only for now.** `update` and install.sh verify
  every archive against `checksums.txt`. **Follow-up:** sign `checksums.txt`
  (cosign keyless via GitHub OIDC, or minisign with a pinned public key in
  the binary and install.sh) so a compromised release page cannot swap both
  archive and checksum.

Decisions made while building prompt 1:

- `update --version vX.Y.Z` installs exactly that release (downgrades get an
  Explain panel) and records `pinned` in config.json, which silences the
  background notice; a plain `ngitool update` clears the pin.
- The new binary is started (`version --json`) before it replaces the old
  one; a binary that does not run or reports another version is not installed.
- Rollback swaps `<binary>` and `<binary>.prev`, so a second rollback undoes
  the first.
- The background update check runs in a detached child
  (`ngitool __update-check`, hidden) with a 2 s timeout and writes the cache;
  the notice is printed from the cache on stderr after a later command. This
  is the only way to be "never blocks" for fast commands.
- Mirror layout for `NGITOOL_DOWNLOAD_BASE` is GitHub's: `<base>/<tag>/<asset>`
  and `<base>/<tag>/checksums.txt`. `NGITOOL_RELEASES_URL` may be a list (the
  API's `/releases`) or a single release object; install.sh takes the first
  `tag_name` in it.
- Esc or "No" at a confirmation exits 1 with "cancelled — nothing changed".
- The root menu is two levels: groups, then the group's actions; each action
  runs the real command in-process (`runArgs`), so menu and CLI never drift.
- MultiSelect owns its selection (huh only renders and navigates): group
  headings toggle their group, disabled rows cannot be ticked, `a`/`n` touch
  only selectable rows. huh's ctrl+a is disabled for that reason.
- `uninstall` removes `$NGITOOL_PREFIX/bin/ngitool` when NGITOOL_PREFIX is
  set, otherwise the running binary; the `ngt` alias only when it points at
  that binary; `--purge` refuses any directory that is not one of the three
  fixed ngitool dirs or under NGITOOL_ROOT.
- `update` takes the state lock when it can; a non-root user updating their
  own copy (lock dir not writable) proceeds without it.

Decisions made while building prompt 2:

- The default compose scan roots are `/home/*`, `/root`, `/opt`, `/srv`
  (config.json `scanRoots`, globs allowed), depth 4 below each root; hidden
  directories are skipped as well as node_modules, vendor, dist, build, .git.
  Only files with an nginx-like `image:` line are passed to
  `docker compose config`, which is decoded into a struct without
  `environment`, so secrets are never even held.
- `docker inspect` runs on every container (one call), not only nginx ones:
  reachability needs every container's networks and DNS names. Env is never
  decoded.
- Running containers are read through `/proc/<pid>/root` when `-T` fails
  (root only), stopped ones through their mounts.
- DOCK-11 lists every running container with a 0.0.0.0/:: publish, not
  only nginx ones: an exposed database bypasses the front door too.
- Size: `dist/ngitool` is **11,821,216 bytes** after prompt 2 (yaml.v3 and
  the new packages added ~0.8 MB). Only ~179 KB of the 12 MB budget is left:
  prompt 3/4 will need to trim or raise the budget deliberately.
- On this server (2026-10-01) the scan found a second nginx the plan did not
  list: one compose app's own image runs nginx 1.29.8 (found by `docker top`,
  DISC-10), and the edge front door proxies to it (DOCK-15).

Decisions made while building prompt 3:

- **Default method for a new multi-member pool: `least_conn`** (open
  decision, the recommended choice). It degrades better than round robin
  when request times are uneven; the wizard lists it first and says so.
- **Snapshot retention: 20 per instance**, configurable with `snapshots` in
  config.json (open decision, the recommended choice).
- **Size budget raised from 12 MB to 13 MB.** prompt 3 grew `dist/ngitool`
  from 11,821,216 to **12,599,456 bytes**: cli +~200 KB, model + render +
  apply ~170 KB, text/template ~135 KB (the prompt asks for embedded
  text/templates), the rest is metadata. Nothing big enough to trim was
  left without dropping a required feature; ~400 KB remain for prompts 4–5.
- Pools are global by name and belong to one instance; routes of one host
  share its certificate, http mode and www (the first route of the host
  decides).
- A path error page (`--error-page /x`) must be served by another route on
  that host, else nginx shows its own 502 (a note says so); URLs redirect.
- `pool switch` keeps the old members as `backup`, or `down` when the method
  cannot have backups (LB-02), until `--confirm` / `--revert`; interactively
  it asks right after the apply.
- `pool check` uses `docker run --rm --network <net> curlimages/curl` (curl's
  own timing is reported), falling back to `docker exec <instance> wget` when
  the image cannot be pulled; host-network instances use Go requests.
- RP-04 (same host on two instances) is refused with the chain explanation
  rather than chained automatically; chaining is an address member on the
  front door.
- A route whose instance has no published port for 80/443 is not probed
  ("skipped with a hint", never faked).
- Pre-existing, not fixed here: under NO_COLOR on a terminal the theme still
  sends the OSC 11 / DSR background query (seen in prompt 2's binary too).

Decisions made while building prompt 4:

- **`app scan --all` and other users' projects: only with
  `--include-others`** (open decision, the recommended choice). Without
  it `--all` links unlinked projects owned by the current user (root) and
  skips the rest. Interactively, picking another user's project asks once
  per owner, naming the owner (DOCK-09); ownership is never changed.
- **Lifecycle output: condensed per-service by default, `--verbose` for
  raw** (open decision, the recommended choice). Condensed keeps service
  events (`Container x  Started`), build steps (`#7 [web 2/4] RUN …`),
  log lines and anything mentioning error/failed/warn, and drops
  BuildKit's bookkeeping (DONE, sha256, transferring …). Every line is
  prefixed with its service in a rotating role colour.
- **Size budget raised from 13 MB to 13.5 MB.** prompt 4 grew
  `dist/ngitool` from 12,599,456 to **13,058,208 bytes** (+458 KB: the
  compose package ~90 KB, the app commands with their help, explain and
  option text ~250 KB, the rest type metadata). Nothing of that size could
  go without dropping a required feature; ~440 KB remain for prompt 5.
- The app commands live in `apps.go`, `apps_link.go`, `apps_run.go` (the
  prompt said `app.go`, which is the root command since prompt 1).
- `app link <path>` takes a directory or one compose file; `--file` sets
  the `-f` list off a terminal, `--project-name` the `-p`.
- An app member is `app:APP/SERVICE:PORT`; a `service:PROJECT/SERVICE`
  member whose project is linked is recorded with its app too.
- Attaching through a route is permanent (the override); plain containers
  keep prompt 3's `docker network connect`, now hinting at `app link`.
- `app fix` recreates only the detached services, with `--force-recreate
  --no-deps`, so a service someone stopped on purpose stays stopped.
- `externalize` mounts the copy writable: read-only was tried first and
  broke a service that writes its config at start (the official image does
  the same with `templates/`). It refuses a second externalized service in
  the same app (one directory per app, as asked).
- `runArgs` (menu → command in-process) reset repeatable flags with
  `Set("[]")`, which appended a literal `[]`; slice flags are now cleared
  with `Replace(nil)`. Found when "Serve it now?" passed `--to` through it.
- `down` acts on the whole app (no service list); `ps` prints the summary
  table instead of compose's raw table.
- Integration tests share the `ngitool-it` network across packages. The
  discover test usually creates it and finishes first, unable to remove it
  while the cli tests are attached, so the compose-apps test (the last
  one) always tries `docker network rm` at the end; it fails harmlessly
  while anything still uses the network.

Decisions made while building prompt 5:

- **Default edge stack directory: `/opt/ngitool/edge`** (open decision, the
  recommended choice): /root is private, and /opt is the conventional place
  for a self-contained stack. `edge init [dir]` takes any other.
- **Migration keeps the directory in place** (open decision, the
  recommended choice): the running containers bind-mount it, and moving it
  would mean downtime. The instance's root stays `<dir>/conf`.
- **Size budget raised from 13.5 MB to 14.5 MB.** prompt 5 grew
  `dist/ngitool` from 13,058,208 to **14,196,896 bytes** (+1.14 MB): the
  new CLI commands ~195 KB, migrate ~84 KB, model ~57 KB, edge (with the
  embedded assets) ~33 KB, certs ~20 KB, certificate generation and
  x509 creation (RSA/ECDSA key generation, CreateCertificate/Request)
  ~150 KB, the rest type metadata and strings. Nothing of that size could go
  without dropping a required feature; ~300 KB remain.
- The legacy templates are embedded but not written into a new stack:
  NgiTool renders its own files, and the migration needs the templates only
  to recognise hand edits.
- `rm --app` plans with `model.PlanRemoval`, which follows the same rules as
  `app unlink` (only the app's members leave shared pools), and also offers
  `docker compose down` of the app as an extra.
- `route add` with `--issue` issues before the transaction (nginx -t needs
  the files) and deletes the new certificate again when the transaction
  fails or is a dry run.
- In a migration, a host that another instance (or a hand-written server)
  already serves is a warning, not a blocker: it is true before the
  migration too. Dry runs of a *copy* of a running stack show exactly that
  for every host.
- `ngitool --version` prints `ngitool vX.Y.Z` (cobra's version flag), next
  to `ngitool version [--json]`.
- Local builds (`make build`, `make release-local`) default to
  `VERSION=v1.0.0`, the next tag; a plain `go build` still reports `dev`.

## Next steps

After v1.0.0, in rough order: TCP/UDP `stream` proxying (reported today,
RP-14/CONF-12); rate limiting (`limit_req`/`limit_conn` per route);
response caching (`proxy_cache` per route); a WAF hook (ModSecurity or
Coraza) — none started. Releases: sign `checksums.txt` (cosign keyless or
minisign, see Decisions) and publish `.deb` packages through GoReleaser
nfpms. Smaller: `real_ip_header` for instances other than the edge stack
(RP-12), a `troubleshoot` view for Cloudflare errors (RP-21).

## Prompts

| # | Branch | What | Status |
|---|---|---|---|
| 1 | `prompt-1-ngitool-foundation` | Go module, UI kit, help + root menu, paths/state/execx, version/update/uninstall/doctor/completion, install.sh, GoReleaser + CI, docs/edge-cases.md, docs/ux.md | done 2026-09-30 (unmerged, unpushed) |
| 2 | `prompt-2-nginx-discovery` | nginxconf parser + dump splitter + includes + summary; discover (host, docker, compose-defined, edge, ports, capabilities, reachability, findings); drivers; `scan`, `instances`, `inspect`, doctor checks, Instances menu | done 2026-10-01 (unmerged, unpushed) |
| 3 | `prompt-3-proxy-balancer` | model, render (3 layouts, resolve vs fallback), apply transaction (snapshot, drift, test, reload, probe, rollback), instance adopt/release, route/pool commands + wizard, Routes/Load balancing/Apply menu groups | done 2026-10-01 (unmerged, unpushed) |
| 4 | `prompt-4-compose-apps` | compose finder (every pattern, labels, owners), app model (state schema 3), override writer + attach, lifecycle actions with explain/Will run/streamed output/summary, drift + `app fix`, `instance externalize`, Linked apps member source, Apps menu group, doctor apps check | done 2026-10-01 (unmerged, unpushed) |
| 5 | `prompt-5-edge-migration` | edge stack (embedded assets, `edge` commands), certificates (Let's Encrypt via certbot with HTTP-01 preflight or DNS-01, Origin CA, custom, self-signed, AOP), Cloudflare (DNS step, cf-sync), static routes, removal cascades (`rm`, `reset`, `www`, `domain`), `migrate edge` with a proving dry run, legacy CLI deleted, README/CHANGELOG/migration runbook (EDGE, CERT, MIG) | done 2026-10-01 (unmerged, unpushed) |
