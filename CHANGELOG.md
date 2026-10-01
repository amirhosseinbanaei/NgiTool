# Changelog

All notable changes to NgiTool. Versions follow [semantic versioning](https://semver.org);
`ngitool update` installs them from GitHub Releases.

## v1.0.0

NgiTool replaces the Node `edge` CLI of nginx-edge with one static Go binary,
`ngitool` (alias `ngt`), that manages **every nginx on the server** for reverse
proxy and load balancing — host-installed nginx, nginx in containers and Compose
projects, and its own edge stack.

### Install and update

- One-line install (`curl … install.sh | sh`) for Linux amd64, arm64 and armv7;
  pin a version with `NGITOOL_VERSION`, install elsewhere with `NGITOOL_PREFIX`.
- `ngitool update` downloads, verifies the SHA-256 against the release's
  checksums and swaps the binary atomically; `--check`, `--version vX.Y.Z` to pin
  or downgrade, `--rollback` to the previous binary. A background check (at most
  once a day, never blocking) mentions new releases; turn it off in
  `/etc/ngitool/config.json` or with `NGITOOL_NO_UPDATE_CHECK=1`.
- `ngitool uninstall` (and `--purge` for its data), `ngitool doctor`, shell
  completion, `--json` on every list.

### Find every nginx

- `ngitool scan`, `instances`, `inspect`: host masters (systemd or not, several at
  once, source builds, openresty, Angie, Tengine), plain containers, Compose
  services (running, stopped, or only defined), with their config parsed
  (includes, globs, symlinks, maps), the front door on :80/:443, and what NgiTool
  may do to each one (read, test, reload, write) — with the reason when not.

### Routes and load balancing

- `ngitool route add` — a wizard (or flags) for a host or `host/path`: targets
  from a checklist of linked apps, Compose services, containers, host ports and
  static folders; two or more targets make a pool.
- Pools with round robin, least connections, IP hash, consistent hash or random
  two; weights, backups, `max_fails`, drain and undrain, blue/green `pool switch`,
  and `pool check` for member health.
- WebSocket, streaming, body size, timeouts, extra headers, HTTPS and gRPC
  upstreams, strip-prefix paths, apex + www, plain HTTP alongside HTTPS.
- Container names resolve at runtime (`resolve` on nginx ≥ 1.27.3), so one app
  being down never stops nginx.

### Safe changes

- Every change goes through one transaction: diff, snapshot, write, `nginx -t`,
  reload, a probe of each route — and the snapshot comes back on any failure.
- Only files with NgiTool's marker are ever rewritten; hand edits are detected and
  can be adopted or overwritten; `ngitool rollback` restores any snapshot.

### Compose apps

- `ngitool app link` / `scan` finds every Compose project on the server (any file
  name, any layout, any owner); `app up`, `restart`, `rebuild`, `pull`, `down`,
  `logs` show the exact command and stream its output.
- An app joins nginx's network through NgiTool's own override file — the
  project's files are never edited — and `app fix` repairs apps started without it.

### The edge stack

- `ngitool edge init` writes NgiTool's own nginx + certbot stack (default
  `/opt/ngitool/edge`) from files inside the binary, and asks for the Docker
  network, the Cloudflare token (masked, verified), the ACME email and the public
  IP. `edge up`, `down`, `restart`, `status`, `logs [host]`, `cf-sync`, and
  `upgrade-assets` for later releases' stack files.
- Static sites from the stack's `www/` (`--to static:NAME`), `ngitool www ls|rm`.

### Certificates and Cloudflare

- `ngitool cert add`: Let's Encrypt with HTTP-01 (checked from the internet before
  Let's Encrypt is asked) or DNS-01 through Cloudflare (wildcards), Cloudflare
  Origin CA, your own certificate, or self-signed. `cert ls` with days left,
  `cert renew`, `cert rm`, `cert aop` for Authenticated Origin Pulls, and
  `ngitool certbot …`. The route wizard can issue a certificate on the spot.
- Cloudflare DNS records per host (proxied or DNS-only) from the route wizard,
  and the real-IP list kept current.

### Removal

- `ngitool rm` removes a domain, host, path, app, certificate or static folder
  with everything that depends on it, shown first; what becomes unused
  (certificates, folders, DNS records, app containers) is offered for deletion.
  `ngitool reset` removes everything NgiTool manages.

### Migrating from nginx-edge

- `ngitool migrate edge <dir> --dry-run` maps an nginx-edge directory (routes,
  certificates, domains with their Cloudflare zones, linked apps and their
  overrides) onto NgiTool, renders the result into a scratch copy, runs `nginx -t`
  and proves the effective config is unchanged; the real run takes the directory
  over in place, in one transaction. See [docs/migration.md](docs/migration.md).
- The Node `edge` CLI is gone; its `edge.json`-era behaviour lives on in NgiTool.
