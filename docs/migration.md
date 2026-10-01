# Migrating from nginx-edge to NgiTool

This runbook moves a running nginx-edge stack (the directory the legacy Node
`edge` CLI managed) under NgiTool without downtime. NgiTool takes the
directory over **in place**: the containers keep running from it, the
certificates stay where they are, nothing is reissued.

Examples use `/srv/nginx-edge` for the directory. Use yours.

## What changes, what stays

| | Before | After |
|---|---|---|
| Routes | `edge.json` sites and paths | `/var/lib/ngitool/state.json` routes and pools |
| nginx files | `conf/sites/<host>.conf`, `conf/locations/<host>/<path>.conf`, `conf/snippets/ssl/<cert>.conf` with `# Managed by edge` | `conf/sites/<host>.conf`, `conf/locations/<host>/<path>.conf`, `conf/ngitool/upstreams/`, `conf/conf.d/ngitool.conf`, `conf/snippets/ngitool-proxy.conf` with `# Managed by NgiTool` |
| Upstreams | `set $upstream app:3000; proxy_pass http://$upstream;` | `upstream ngt_<pool> { server app:3000 resolve; }` (nginx ≥ 1.27.3), same targets |
| Certificates | `edge.json` certs | registered in state with the same files |
| Domains | `edge.json` domains with Cloudflare zone ids | state domains, same zone ids |
| Apps | `apps/<name>/compose.yaml` symlinks, `apps/<name>/edge.override.yaml` | linked apps with the same compose files; the override moves to `/var/lib/ngitool/overrides/<app>.yaml` with the same network and aliases |
| Hand-written files | `conf/sites/00-default.conf`, any file without `# Managed by edge` | left in place, untouched, still included |
| Cloudflare token | `secrets/cloudflare.ini` | used in place; never copied, never printed |
| Containers | compose project `edge` | the same project, the same containers |
| Commands | `edge …` | `ngitool …` (see the table at the end) |

## 1. Back up

NgiTool keeps its own backup during the real run, but take one you control
first:

```bash
cp -a /srv/nginx-edge /root/nginx-edge-backup-$(date +%F)
```

The copy contains private keys and the Cloudflare token: keep it root-only
(`chmod 700`) and delete it once you no longer need it.

## 2. Install NgiTool

```bash
curl -fsSL https://raw.githubusercontent.com/amirhosseinbanaei/NgiTool/main/install.sh | sh
ngitool doctor
```

## 3. Dry run

```bash
ngitool migrate edge /srv/nginx-edge --dry-run
```

Nothing in the directory changes. NgiTool:

1. prints the mapping: every certificate, domain, app, route and
   hand-written file, with what it becomes;
2. renders its own files into a scratch copy of `conf/`;
3. runs `nginx -t` on that copy with the stack's own image and mounts
   (read-only, no network);
4. compares the effective config before and after — for every port and
   hostname: which server answers, its certificate, client verification
   (Authenticated Origin Pulls), and what each location does, with proxy
   targets resolved through `set` and `upstream` blocks;
5. ends with **safe to migrate** or **N blockers**, each with its reason.

Blockers stop the real run:

| Blocker | Meaning | What to do |
|---|---|---|
| a difference (`MIG-01`) | a host would be answered differently | read its reason; fix edge.json or the hand-written file, run the dry run again |
| hand-edited file (`MIG-02`) | a `# Managed by edge` file is not what edge.json renders | move the edit into a hand-written file, or accept losing it with `--on-drift overwrite` |
| `nginx -t` failed (`APPLY-01`) | the new config is rejected | the output names the file and line |

Warnings do not stop it. "already served by another nginx (RP-04)" is true
today and the migration does not change it.

## 4. Run it

```bash
ngitool migrate edge /srv/nginx-edge
```

It runs the same checks, asks once, and then:

1. copies `conf/`, `apps/`, `edge.json` and `.env` to
   `/var/lib/ngitool/backups/migrate-<time>/`;
2. in **one transaction**: takes a snapshot, writes NgiTool's files over the
   legacy ones, removes the legacy `snippets/ssl/*.conf`, runs `nginx -t`,
   reloads the running nginx, and requests every route through it;
3. records the stack, routes, pools, certificates, domains and apps in
   `state.json`;
4. writes each app's override to `/var/lib/ngitool/overrides/<app>.yaml`;
5. marks the directory (`.ngitool-edge`) and renames `edge.json` to
   `edge.json.migrated`, so the old CLI cannot regenerate files over
   NgiTool's.

If `nginx -t` or the reload fails, the snapshot comes back and nginx keeps
the old config.

## 5. Verify

```bash
ngitool edge status          # services, network, certificates, routes
ngitool route ls             # every route with its last probe
ngitool cert ls              # certificates with days left
ngitool app ls               # linked apps; see below for "detached"
curl -sI https://example.com # each of your hosts
```

### Apps started with the legacy override

An app the old CLI attached to the edge network was started with
`-f apps/<app>/edge.override.yaml`. Its containers' `config_files` label
still names that file, so NgiTool's drift check reports it **detached**
until it is recreated once through NgiTool:

```bash
ngitool app up <app>      # for every app the migration listed (MIG-04)
ngitool app ls            # all ok
```

The containers keep the same network and alias, so routes keep working in
between; only the label is stale.

## 6. Remove the old command

```bash
ls -l /usr/local/bin/edge        # the symlink `edge install` made
rm /usr/local/bin/edge
```

The directory's `cli/`, `edge`, `package.json`, `templates/` and
`examples/` are no longer used. Keep them until you are sure, then remove
them. `conf/`, `data/`, `www/`, `secrets/`, `compose.yaml` and `.env` are
the stack and stay.

## 7. Roll back

The migration's transaction took a snapshot first. Restoring it puts every
legacy file back (and removes NgiTool's), tests and reloads nginx, and
drops the imported routes, pools and the adoption from state:

```bash
ngitool rollback edge:/srv/nginx-edge     # pick the "migrate edge /srv/nginx-edge" snapshot
mv /srv/nginx-edge/edge.json.migrated /srv/nginx-edge/edge.json
rm /srv/nginx-edge/.ngitool-edge
```

The legacy `edge` CLI works again from here. NgiTool's state still lists
the imported certificates, domains and apps: harmless, but do **not**
remove them with `ngitool cert rm` (it deletes the certificate files the
stack still uses). If NgiTool manages nothing else on the server, delete
`/var/lib/ngitool/state.json` instead.

When the snapshot is gone, the full copy from step 4 has everything:

```bash
B=/var/lib/ngitool/backups/migrate-<time>
rsync -a --delete $B/conf/ /srv/nginx-edge/conf/
cp -a $B/edge.json /srv/nginx-edge/edge.json
docker compose -p edge --project-directory /srv/nginx-edge exec nginx nginx -c /etc/nginx/edge/nginx.conf -s reload
```

Apps recreated through `ngitool app up` keep their network and alias; run
`edge app up <app>` to give them the legacy override again.

## Commands, old and new

| nginx-edge | NgiTool |
|---|---|
| `edge init` | `ngitool edge init` |
| `edge up`, `down`, `restart`, `logs [host]` | `ngitool edge up`, `down`, `restart`, `logs [host]` |
| `edge status` | `ngitool edge status`, `ngitool route ls` |
| `edge site add api.example.com` | `ngitool route add api.example.com` |
| `edge path add example.com/admin` | `ngitool route add example.com/admin` |
| `edge domain add example.com` | `ngitool route add example.com --issue letsencrypt --dns proxied` |
| `--app shop/web:3000`, `--container x:3000`, `--port 3001`, `--static dir` | `--to app:shop/web:3000`, `--to container:x:3000`, `--to port:3001`, `--to static:dir` |
| `edge http <host> serve` | `ngitool route edit <host> --http serve` |
| `edge enable` / `disable` | `ngitool route enable` / `disable` |
| `edge rm …`, `domain rm`, `app rm`, `cert rm`, `www rm`, `reset` | `ngitool rm …`, `ngitool rm --domain`, `ngitool app unlink`, `ngitool cert rm`, `ngitool www rm`, `ngitool reset` |
| `edge cert add`, `renew`, `aop` | `ngitool cert add`, `renew`, `aop` |
| `edge certbot …` | `ngitool certbot …` |
| `edge cf-sync` | `ngitool edge cf-sync` |
| `edge app link`, `scan`, `up`, `down`, `logs` | `ngitool app link`, `scan`, `up`, `down`, `logs` |
| `edge sync` | `ngitool apply` |
| `edge test`, `reload` | `ngitool test`, `ngitool reload` |
