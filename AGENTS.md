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

**What it is not (yet):** after prompt 1 it has no discovery, no config
parsing, no routes or pools, no compose handling and no certificates. Those
arrive in prompts 2–5. Only commands that work are registered — never a
"coming soon" placeholder. TCP/UDP `stream` proxying, Kubernetes ingress and
NGINX Plus features are out of scope (see docs/edge-cases.md).

## Layout

```
cmd/ngitool/main.go     wiring only: os.Exit(cli.Main(os.Args[1:]))
internal/cli            one file per command group; app.go (root, exit codes,
                        the ordered `commands` list), menu.go (groups + menu
                        items registry), help.go (help template), doctor.go
                        (checks registry), update.go, updatecheck.go,
                        uninstall.go, completion.go, version.go, lock.go
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
(not imported yet; `go mod tidy` drops it until prompt 2 or 4 parses YAML).
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
| `/var/cache/ngitool/` | `update-check.json`, scan caches |

`NGITOOL_ROOT=/dir` moves all of them to `/dir/etc`, `/dir/lib`, `/dir/cache`.
`NGITOOL_PREFIX=/dir` makes the installer and `uninstall` use `/dir/bin`.
Files are 0600, directories 0700. Every document has a top-level `schema`
integer; `state.Store` runs migrations on load (SYS-07).

Other environment variables: `NGITOOL_RELEASES_URL` (releases JSON instead of
GitHub's API), `NGITOOL_DOWNLOAD_BASE` (mirror laid out as
`<base>/<tag>/<asset>`), `NGITOOL_VERSION` (install.sh), `GITHUB_TOKEN`
(sent to api.github.com only), `NGITOOL_NO_UPDATE_CHECK=1`, `NO_COLOR`.

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

## Prompts

| # | Branch | What | Status |
|---|---|---|---|
| 1 | `prompt-1-ngitool-foundation` | Go module, UI kit, help + root menu, paths/state/execx, version/update/uninstall/doctor/completion, install.sh, GoReleaser + CI, docs/edge-cases.md, docs/ux.md | done 2026-09-30 (unmerged, unpushed) |
| 2 | `prompt-2-…` | discovery of every nginx + config parsing (DISC, CONF) | planned |
| 3 | `prompt-3-…` | routes, pools, render + apply with rollback (RP, LB, APPLY) | planned |
| 4 | `prompt-4-…` | compose scanning, app lifecycle (DOCK) | planned |
| 5 | `prompt-5-edge-migration` | edge stack, certificates, migrate edge.json, delete `cli/` (EDGE, CERT, MIG) | planned |
