// apps/ — one folder per project, holding a symlink to that project's compose file:
//
//   apps/my-site/compose.yaml → /home/you/my-site/compose.yaml
//   apps/my-site/edge.override.yaml   (optional, written by edge)
//
// Compose is always run against the REAL file with the project directory as cwd,
// so relative paths (build context, ./data, .env) and the project name are exactly
// what they are when you run `docker compose` inside the project.

import fs from 'node:fs';
import path from 'node:path';
import { P } from './config.mjs';
import { run } from './exec.mjs';

export const COMPOSE_NAMES = ['compose.yaml', 'compose.yml', 'docker-compose.yaml', 'docker-compose.yml'];
export const OVERRIDE = 'edge.override.yaml';
const OVERRIDE_NET = 'nginx_edge';

/** A compose file path from a project dir or a file path. */
export function findComposeFile(input) {
  const abs = path.resolve(input);
  if (!fs.existsSync(abs)) throw new Error(`${input} does not exist`);
  if (fs.statSync(abs).isFile()) return abs;
  for (const n of COMPOSE_NAMES) {
    const f = path.join(abs, n);
    if (fs.existsSync(f)) return f;
  }
  throw new Error(`no compose file in ${input} (looked for ${COMPOSE_NAMES.join(', ')})`);
}

/** Folder name → app name: lower-case, [a-z0-9._-]. */
export const appNameFor = (file) =>
  path
    .basename(path.dirname(file))
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, '-')
    .replace(/^-+|-+$/g, '') || 'app';

function readApp(name) {
  const dir = path.join(P.apps, name);
  const link = COMPOSE_NAMES.map((n) => path.join(dir, n)).find((f) => {
    try {
      return fs.lstatSync(f).isSymbolicLink() || fs.lstatSync(f).isFile();
    } catch {
      return false;
    }
  });
  const app = { name, dir, link: link || null, target: null, projectDir: null, override: null, broken: null };
  if (!link) {
    app.broken = 'no compose file or symlink in the folder';
    return app;
  }
  try {
    app.target = fs.realpathSync(link);
    app.projectDir = path.dirname(app.target);
  } catch {
    app.broken = `symlink points to a missing file (${fs.readlinkSync(link)})`;
  }
  const ov = path.join(dir, OVERRIDE);
  if (fs.existsSync(ov)) app.override = ov;
  return app;
}

export function listApps() {
  if (!fs.existsSync(P.apps)) return [];
  return fs
    .readdirSync(P.apps, { withFileTypes: true })
    .filter((d) => d.isDirectory() && !d.name.startsWith('.'))
    .map((d) => readApp(d.name))
    .sort((a, b) => a.name.localeCompare(b.name));
}

export function getApp(name) {
  const dir = path.join(P.apps, name);
  return fs.existsSync(dir) && fs.statSync(dir).isDirectory() ? readApp(name) : null;
}

/** Create apps/<name>/<compose file name> → the project's compose file. */
export function linkApp(composeFile, name) {
  const target = fs.realpathSync(composeFile);
  const dupe = listApps().find((a) => a.target === target);
  if (dupe) {
    if (dupe.name === name) return { app: dupe, created: false };
    throw new Error(`that compose file is already linked as apps/${dupe.name}`);
  }
  const dir = path.join(P.apps, name);
  if (fs.existsSync(dir) && fs.readdirSync(dir).length) {
    throw new Error(`apps/${name} already exists — choose another name`);
  }
  fs.mkdirSync(dir, { recursive: true });
  fs.symlinkSync(target, path.join(dir, path.basename(target)));
  return { app: readApp(name), created: true };
}

/** Remove the symlink and edge's override; the folder goes too if nothing else is in it. */
export function unlinkApp(name) {
  const app = getApp(name);
  if (!app) throw new Error(`no app named ${name}`);
  if (app.link) fs.rmSync(app.link);
  if (app.override) fs.rmSync(app.override);
  if (!fs.readdirSync(app.dir).length) fs.rmdirSync(app.dir);
}

/** Find compose files up to `depth` levels below each root (skips node_modules, dot-dirs). */
export function scanForProjects(roots, depth = 2) {
  const found = [];
  const walk = (dir, level) => {
    let entries;
    try {
      entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch {
      return;
    }
    const compose = COMPOSE_NAMES.find((n) => entries.some((e) => e.name === n && (e.isFile() || e.isSymbolicLink())));
    if (compose) {
      found.push(path.join(dir, compose));
      return; // a project's subfolders are its own business
    }
    if (level >= depth) return;
    for (const e of entries) {
      if (!e.isDirectory() || e.name.startsWith('.') || ['node_modules', 'vendor', 'dist', 'build'].includes(e.name)) continue;
      walk(path.join(dir, e.name), level + 1);
    }
  };
  for (const r of roots) walk(path.resolve(r), 0);
  return [...new Set(found)];
}

// ── docker compose against an app ───────────────────────────────────────────

export function composeArgs(app, { override = true } = {}) {
  const args = ['compose', '--project-directory', app.projectDir, '-f', app.target];
  if (override && app.override) args.push('-f', app.override);
  return args;
}

export const appCompose = (app, args, opts = {}) =>
  run('docker', [...composeArgs(app), ...args], { cwd: app.projectDir, ...opts });

/**
 * The services of an app as edge needs them. Reads `docker compose config`,
 * keeps only names, ports and networks — the resolved config also holds the
 * project's secrets, and none of that is ever printed or stored.
 */
export async function appServices(app, network) {
  const res = await run('docker', [...composeArgs(app), 'config', '--format', 'json'], { cwd: app.projectDir });
  if (res.code !== 0) {
    const why = res.stderr.trim().split('\n').slice(-3).join(' ');
    throw new Error(`docker compose could not read ${app.target}: ${why}`);
  }
  return summarizeServices(JSON.parse(res.stdout), network);
}

export function summarizeServices(config, network) {
  const project = config.name;
  const nets = config.networks || {};
  const netName = (key) => nets[key]?.name || `${project}_${key}`;
  return Object.entries(config.services || {})
    .map(([name, s]) => {
      const keys = Object.keys(s.networks || { default: null });
      const edgeKey = keys.find((k) => netName(k) === network);
      const ports = new Set();
      for (const e of s.expose || []) ports.add(Number(String(e).split('/')[0].split('-')[0]));
      for (const p of s.ports || []) if (p.target) ports.add(Number(p.target));
      return {
        name,
        project,
        containerName: s.container_name || null,
        defaultName: `${project}-${name}-1`,
        ports: [...ports].filter(Boolean).sort((a, b) => a - b),
        networkKeys: keys,
        networks: keys.map(netName),
        onEdge: Boolean(edgeKey),
        aliases: edgeKey ? s.networks?.[edgeKey]?.aliases || [] : [],
        oneOff: s.restart === 'no',
      };
    })
    .sort((a, b) => Number(a.oneOff) - Number(b.oneOff) || a.name.localeCompare(b.name));
}

/** The name nginx should use for a service that is on the network. */
export const upstreamFor = (svc) => svc.aliases[0] || svc.containerName || svc.defaultName;

// ── edge.override.yaml ──────────────────────────────────────────────────────

function readOverrideMeta(app) {
  if (!app.override) return {};
  const m = fs.readFileSync(app.override, 'utf8').match(/^# edge: (\{.*\})$/m);
  try {
    return m ? JSON.parse(m[1]).services || {} : {};
  } catch {
    return {};
  }
}

export function overrideYaml(network, services) {
  const meta = JSON.stringify({ network, services });
  const lines = [
    `# Managed by edge — attaches services of this app to the nginx network "${network}".`,
    '# Applied by `edge app up|restart`; the project\'s own compose file is never touched.',
    `# edge: ${meta}`,
    'services:',
  ];
  for (const [svc, { alias, keys }] of Object.entries(services)) {
    lines.push(`  ${svc}:`, '    networks:');
    for (const k of keys) if (k !== OVERRIDE_NET) lines.push(`      ${k}: {}`);
    lines.push(`      ${OVERRIDE_NET}:`, `        aliases: [${alias}]`);
  }
  lines.push('networks:', `  ${OVERRIDE_NET}:`, `    name: ${network}`, '    external: true', '');
  return lines.join('\n');
}

/** Add (or update) one service in the app's override. */
export function writeOverride(app, svc, alias, network) {
  const services = readOverrideMeta(app);
  services[svc.name] = { alias, keys: svc.networkKeys };
  const file = path.join(app.dir, OVERRIDE);
  fs.writeFileSync(file, overrideYaml(network, services));
  app.override = file;
  return file;
}

/** The lines to paste into a project's compose file instead of using the override. */
export const manualSnippet = (svc, alias, network) =>
  [
    'services:',
    `  ${svc.name}:`,
    '    networks:',
    ...svc.networkKeys.map((k) => `      ${k}:`),
    '      nginx:',
    `        aliases: [${alias}]`,
    'networks:',
    '  nginx:',
    `    name: ${network}`,
    '    external: true',
  ].join('\n');

/** Running/total containers of an app, matched by compose's working_dir label. */
export function appState(app, containers) {
  const mine = containers.filter((c) => c.workingDir === app.projectDir);
  return { running: mine.filter((c) => c.state === 'running').length, total: mine.length, containers: mine };
}
