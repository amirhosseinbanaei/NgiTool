// edge.json → nginx files, and the test-then-reload transaction around every change.
//
// Files the CLI writes start with MARKER. Only those are ever rewritten or
// deleted; hand-written files next to them (00-default.conf, custom sites)
// are left alone.

import fs from 'node:fs';
import path from 'node:path';
import { P, IN, saveState } from './config.mjs';
import { compose, docker, nginxImage, nginxRunning } from './exec.mjs';
import { upstreamOf, describeSource, slugPath, certCovers, CERT_TYPES } from './targets.mjs';
import { task, log, c } from './ui.mjs';

export const MARKER = '# Managed by edge';

export class NginxError extends Error {
  constructor(output) {
    super(`nginx rejected the configuration — nothing was changed\n${output}`);
    this.output = output;
  }
}

// ── templates ───────────────────────────────────────────────────────────────

const tplCache = new Map();
const tpl = (name) => {
  if (!tplCache.has(name)) tplCache.set(name, fs.readFileSync(path.join(P.templates, name), 'utf8'));
  return tplCache.get(name);
};

/** Fill {{KEY}}s. A line holding nothing but an empty {{KEY}} is dropped entirely. */
export function render(text, vars) {
  let out = text;
  for (const [key, raw] of Object.entries(vars)) {
    const val = raw == null ? '' : String(raw);
    if (!val) out = out.replace(new RegExp(`^[ \\t]*\\{\\{${key}\\}\\}[ \\t]*\\n`, 'gm'), '');
    out = out.split(`{{${key}}}`).join(val);
  }
  return out;
}

// ── certificate file locations ──────────────────────────────────────────────

/** Where a certificate's PEM files are, on the host and inside the nginx container. */
export function certPaths(name, cert) {
  if (cert.type === 'letsencrypt') {
    return {
      crt: path.join(P.letsencrypt, 'live', name, 'fullchain.pem'),
      key: path.join(P.letsencrypt, 'live', name, 'privkey.pem'),
      inCrt: `${IN.letsencrypt}/live/${name}/fullchain.pem`,
      inKey: `${IN.letsencrypt}/live/${name}/privkey.pem`,
    };
  }
  return {
    crt: path.join(P.certs, name, 'fullchain.pem'),
    key: path.join(P.certs, name, 'privkey.pem'),
    inCrt: `${IN.certs}/${name}/fullchain.pem`,
    inKey: `${IN.certs}/${name}/privkey.pem`,
  };
}

// ── state → files ───────────────────────────────────────────────────────────

/** Every managed file the state describes: Map<path relative to ROOT, content>. */
export function buildFiles(state) {
  const files = new Map();

  for (const [name, cert] of Object.entries(state.certs)) {
    const p = certPaths(name, cert);
    files.set(
      `conf/snippets/ssl/${name}.conf`,
      render(tpl('ssl.conf.tpl'), {
        CERT: name,
        TYPE: CERT_TYPES[cert.type] || cert.type,
        NAMES: (cert.names || []).join(', '),
        CRT: p.inCrt,
        KEY: p.inKey,
        AOP: cert.aop
          ? '# Authenticated Origin Pulls: only Cloudflare may connect (edge cert aop ' + name + ' off)\ninclude /etc/nginx/edge/snippets/cloudflare-aop.conf;'
          : '',
      }),
    );
  }

  for (const [host, site] of Object.entries(state.sites)) {
    if (site.enabled === false) continue;
    const cert = state.certs[site.cert];
    if (!cert) throw new Error(`${host} uses certificate "${site.cert}", which is not in edge.json`);
    const apexWithWww = state.domains[host] && certCovers(cert.names || [], `www.${host}`);
    const body =
      site.source.type === 'static'
        ? render(tpl('body-static.tpl'), { DIR: site.source.dir })
        : render(tpl('body-proxy.tpl'), { UPSTREAM: upstreamOf(site.source) });
    files.set(
      `conf/sites/${host}.conf`,
      render(tpl('site.conf.tpl'), {
        TARGET: host,
        DESC: describeSource(site.source),
        HOST: host,
        SERVER_NAMES: apexWithWww ? `${host} www.${host}` : host,
        CERT: site.cert,
        BODY: body.replace(/\n$/, ''),
      }),
    );
  }

  for (const [key, route] of Object.entries(state.paths)) {
    if (route.enabled === false) continue;
    const src = route.source;
    const file = `conf/locations/${route.host}/${slugPath(route.path)}.conf`;
    if (src.type === 'static') {
      files.set(
        file,
        render(tpl('path-static.conf.tpl'), { TARGET: key, DESC: describeSource(src), PATH: route.path, DIR: src.dir }),
      );
    } else {
      files.set(
        file,
        render(tpl('path-proxy.conf.tpl'), {
          TARGET: key,
          DESC: describeSource(src) + (route.strip ? ' (prefix stripped)' : ''),
          PATH: route.path,
          UPSTREAM: upstreamOf(src),
          STRIP: route.strip ? `rewrite ^${route.path}/(.*)$ /$1 break;` : '',
        }),
      );
    }
  }

  return files;
}

// ── files on disk ───────────────────────────────────────────────────────────

function* confFiles() {
  const dirs = [P.sites, P.sslSnippets];
  if (fs.existsSync(P.locations)) {
    for (const d of fs.readdirSync(P.locations, { withFileTypes: true })) {
      if (d.isDirectory()) dirs.push(path.join(P.locations, d.name));
    }
  }
  for (const dir of dirs) {
    if (!fs.existsSync(dir)) continue;
    for (const f of fs.readdirSync(dir)) if (f.endsWith('.conf')) yield path.join(dir, f);
  }
}

const isManaged = (abs) => fs.readFileSync(abs, 'utf8').startsWith(MARKER);

/** Managed files currently on disk: Map<rel, content>. */
export function readManaged() {
  const out = new Map();
  for (const abs of confFiles()) {
    const text = fs.readFileSync(abs, 'utf8');
    if (text.startsWith(MARKER)) out.set(path.relative(P.root, abs), text);
  }
  return out;
}

/** Make the managed files on disk exactly `files`. Returns the number of files changed. */
export function writeFiles(files) {
  // Never overwrite something a human wrote.
  for (const rel of files.keys()) {
    const abs = path.join(P.root, rel);
    if (fs.existsSync(abs) && !isManaged(abs)) {
      throw new Error(`${rel} exists and was not written by edge — move it away first`);
    }
  }
  const current = readManaged();
  let changed = 0;
  for (const rel of current.keys()) {
    if (!files.has(rel)) {
      fs.rmSync(path.join(P.root, rel));
      changed++;
    }
  }
  for (const [rel, content] of files) {
    if (current.get(rel) === content) continue;
    const abs = path.join(P.root, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, content);
    changed++;
  }
  // every host with a site gets a locations dir, so its include glob always resolves
  for (const rel of files.keys()) {
    const m = rel.match(/^conf\/sites\/(.+)\.conf$/);
    if (m) fs.mkdirSync(path.join(P.locations, m[1]), { recursive: true });
  }
  return changed;
}

// ── nginx -t / reload ───────────────────────────────────────────────────────

const noise = (s) =>
  s
    .split('\n')
    .filter((l) => l.trim() && !/signal process started|worker_connections exceed/.test(l))
    .join('\n');

export async function testConfig() {
  let res;
  if (await nginxRunning()) {
    res = await compose(['exec', '-T', 'nginx', 'nginx', '-c', IN.conf, '-t', '-q']);
  } else {
    // Same image and mounts as the service, no network needed: upstreams resolve per request.
    res = await docker([
      'run', '--rm', '--network', 'none', '--ulimit', 'nofile=65535:65535',
      '-v', `${P.conf}:/etc/nginx/edge:ro`,
      '-v', `${P.www}:/var/www:ro`,
      '-v', `${P.letsencrypt}:${IN.letsencrypt}:ro`,
      '-v', `${P.certs}:${IN.certs}:ro`,
      '--entrypoint', 'nginx', await nginxImage(), '-c', IN.conf, '-t', '-q',
    ]);
  }
  return { ok: res.code === 0, output: noise(`${res.stderr}\n${res.stdout}`) };
}

export async function reloadNginx() {
  const res = await compose(['exec', '-T', 'nginx', 'nginx', '-c', IN.conf, '-s', 'reload']);
  if (res.code !== 0) throw new Error(`reload failed\n${noise(res.stderr + res.stdout)}`);
}

/**
 * The one way state changes reach nginx: write files for `next`, `nginx -t`,
 * then save edge.json and reload. If the test fails, the previous files are
 * put back and edge.json is left untouched.
 */
export async function commit(next, { quiet = false } = {}) {
  const before = readManaged();
  const files = buildFiles(next);
  writeFiles(files);
  try {
    await task('Checking nginx configuration', async () => {
      const t = await testConfig();
      if (!t.ok) throw new NginxError(t.output);
    });
  } catch (err) {
    writeFiles(before);
    throw err;
  }
  saveState(next);
  if (await nginxRunning()) {
    await task('Reloading nginx', reloadNginx);
  } else if (!quiet) {
    log.hint(`nginx is not running — start it with ${c.bold('edge up')}`);
  }
}

/** A placeholder page so a new static folder shows something. */
export function ensureStaticDir(dir, name) {
  const abs = path.join(P.www, dir);
  fs.mkdirSync(abs, { recursive: true });
  if (fs.readdirSync(abs).length === 0) {
    fs.writeFileSync(path.join(abs, 'index.html'), render(tpl('index.html.tpl'), { NAME: name, DIR: dir }));
  }
}
