# AGENTS.md — NgiTool

Project memory for anyone (human or agent) working in this repository. Read it
first; update it when you change something it describes.

## What NgiTool is

NgiTool is a single static Go binary, `ngitool` (installed with a short alias
`ngt`), that will manage **every nginx on a server** for **reverse proxy and
load balancing**:

- host-installed nginx (systemd or not, several masters),
- nginx in plain Docker containers,
- nginx inside Docker Compose projects,
- its own bundled "edge" nginx stack (this repository's `compose.yaml`).

It is the successor of the Node `edge` CLI in `cli/`, which keeps working
(the live stack still uses it) until prompt 5 migrates and removes it.

**What it is not (yet):** after prompt 2 it finds every nginx and reads its
config (`scan`, `instances`, `inspect`), but it writes nothing: no routes or
pools, no compose linking, no certificates. Those arrive in prompts 3–5. Only commands that work are registered — never a
"coming soon" placeholder. TCP/UDP `stream` proxying, Kubernetes ingress and
NGINX Plus features are out of scope (see docs/edge-cases.md).

## Layout

```
cmd/ngitool/main.go     wiring only: os.Exit(cli.Main(os.Args[1:]))
internal/cli            one file per command group; app.go (root, exit codes,
                        the ordered `commands` list), menu.go (groups + menu
                        items registry), help.go (help template), doctor.go
                        (checks registry), instances.go (scan, instances,
                        inspect, the scan cache), update.go, updatecheck.go,
                        uninstall.go, completion.go, version.go, lock.go
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
internal/compose        find.go: compose files under the scan roots (prompt 4 extends)
internal/ui             theme.go (the ONLY colours), text.go (width, truncate,
                        cursor), components.go, task.go (spinner, steps),
                        diff.go, prompt.go (huh prompts), ask.go (flag rules)
internal/paths          every path, NGITOOL_ROOT / NGITOOL_PREFIX
internal/state          atomic writes, flock, JSON stores with schema + migrations
internal/execx          process runner (docker, docker compose, nginx)
internal/version        build info (-ldflags), semver compare
internal/update         GitHub releases, download, verify, install, rollback
install.sh              curl | sh installer (POSIX sh)
.goreleaser.yaml        release build; .github/workflows/{ci,release}.yml
docs/ux.md              terminal UX rulebook
docs/edge-cases.md      master exception list, referenced by ID
cli/, conf/, templates/, compose.yaml, package.json   legacy nginx-edge (do not touch until prompt 5)
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
| `/etc/ngitool/config.json` | settings: `updateCheck`, `channel` (stable/prerelease), `scanRoots`, `frontDoor`, `pinned` |
| `/var/lib/ngitool/state.json` | what NgiTool manages (schema only so far) |
| `/var/lib/ngitool/.lock` | exclusive flock held by every mutating command |
| `/var/lib/ngitool/backups/` | snapshots before every apply (prompt 3) |
| `/var/lib/ngitool/overrides/` | compose overrides (prompt 4) |
| `/var/cache/ngitool/` | `update-check.json`, `scan.json` (the last scan: summaries only, never a dump) |

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
`conf/nginx.conf` and the edge CLI (`edge` or `cli/src`).

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

## UX rules

See [docs/ux.md](docs/ux.md). In short: colours only through the roles in
theme.go; plain output has the same content; every prompt collapses to
`✔ question › answer`; Esc goes back, Ctrl-C exits 130; every prompt has a
flag and fails naming it off a terminal; `--yes` skips confirms but a typed
danger confirm needs `--yes --force`; every list/inspect command has `--json`;
typing is the last resort — discovered lists end with "✎ Enter it manually…".

## Non-negotiables

- Static binary, `CGO_ENABLED=0`, under the size budget (12 MB for
  linux/amd64, `make size`); the dependency rule above.
- Docker and nginx only through their CLIs via `internal/execx`.
- Every interactive step has a flag equivalent and fails clearly off a TTY.
  Every list or inspect command has `--json`.
- No raw colour codes outside `internal/ui/theme.go`. NO_COLOR is honoured everywhere.
- Never print or store resolved compose environment or secrets.
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
node cli/test/run.mjs   # the legacy CLI is untouched and must stay green
NGITOOL_IT=1 go test ./internal/discover/...   # optional: real Docker, own container ngitool-it-scan on network ngitool-it
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
The Node CLI's tests: `node cli/test/run.mjs` (its own EDGE_ROOT sandbox).

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

## Prompts

| # | Branch | What | Status |
|---|---|---|---|
| 1 | `prompt-1-ngitool-foundation` | Go module, UI kit, help + root menu, paths/state/execx, version/update/uninstall/doctor/completion, install.sh, GoReleaser + CI, docs/edge-cases.md, docs/ux.md | done 2026-09-30 (unmerged, unpushed) |
| 2 | `prompt-2-nginx-discovery` | nginxconf parser + dump splitter + includes + summary; discover (host, docker, compose-defined, edge, ports, capabilities, reachability, findings); drivers; `scan`, `instances`, `inspect`, doctor checks, Instances menu | done 2026-10-01 (unmerged, unpushed) |
| 3 | `prompt-3-…` | routes, pools, render + apply with rollback (RP, LB, APPLY) | planned |
| 4 | `prompt-4-…` | compose scanning, app lifecycle (DOCK) | planned |
| 5 | `prompt-5-edge-migration` | edge stack, certificates, migrate edge.json, delete `cli/` (EDGE, CERT, MIG) | planned |
