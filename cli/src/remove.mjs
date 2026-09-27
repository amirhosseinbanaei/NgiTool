// Removing things — and everything that goes with them.
//
// planRemoval() is pure: from edge.json and a target it works out
//   drop     what must go too: paths under a removed host, hosts under a removed
//            domain, routes that point at a removed app, folder or certificate
//   extras   what would be left unused: certificates, static folders, DNS
//            records, app containers — deleted only when asked for (--purge)
// applyRemoval() then makes it so with one nginx commit, and only after the
// commit succeeds deletes files, DNS records and app links.
//
// Project folders are never deleted: an app is only unlinked from apps/ (and
// its containers stopped, if asked).

import fs from 'node:fs';
import path from 'node:path';
import { P, loadState, clone, readToken } from './config.mjs';
import { commit } from './nginx.mjs';
import * as certs from './certs.mjs';
import * as cf from './cloudflare.mjs';
import * as apps from './apps.mjs';
import { zoneFor } from './flows.mjs';
import { UsageError, parseTarget, domainOf, certCovers, describeSource } from './targets.mjs';
import { select, input, confirm, multiselect, canPrompt } from './prompt.mjs';
import { c, log, sym, task, heading, shorten } from './ui.mjs';

export const KINDS = {
  domain: 'domain',
  site: 'host',
  path: 'path',
  app: 'app',
  cert: 'certificate',
  www: 'static folder',
  all: 'everything',
};

const emptyDrop = () => ({ domains: new Set(), sites: new Set(), paths: new Set(), certs: new Set(), apps: new Set(), www: new Set() });

function dropSite(state, drop, host) {
  drop.sites.add(host);
  for (const [key, p] of Object.entries(state.paths)) if (p.host === host) drop.paths.add(key);
}

/** Every route (site or path) as { kind, key, source }. */
const routes = (state) => [
  ...Object.entries(state.sites).map(([key, s]) => ({ kind: 'site', key, source: s.source })),
  ...Object.entries(state.paths).map(([key, p]) => ({ kind: 'path', key, source: p.source })),
];

function dropRoutesWhere(state, drop, test) {
  for (const r of routes(state)) {
    if (!test(r.source)) continue;
    if (r.kind === 'site') dropSite(state, drop, r.key);
    else drop.paths.add(r.key);
  }
}

/** Static folders routes use: Set<dir>. */
const wwwUsed = (state) => new Set(routes(state).filter((r) => r.source.type === 'static').map((r) => r.source.dir));

/** Certificates sites and domains use: Set<name>. */
const certsUsed = (state) =>
  new Set([...Object.values(state.sites).map((s) => s.cert), ...Object.values(state.domains).map((d) => d.cert)].filter(Boolean));

/**
 * What removing `target` means. target = { kind, key, replaceCert? }
 *   kind: domain | site | path | app | cert | www | all
 *   replaceCert: for kind 'cert' — move its users to this certificate instead of removing them
 * `linked` is the list of app names in apps/ (needed for 'all').
 */
export function planRemoval(state, target, { linked = [] } = {}) {
  const { kind, key } = target;
  const drop = emptyDrop();
  const moved = { sites: {}, domains: {} };

  switch (kind) {
    case 'path':
      if (!state.paths[key]) throw new UsageError(`nothing is served at ${key}`);
      drop.paths.add(key);
      break;
    case 'site':
      if (!state.sites[key]) throw new UsageError(`${key} is not served`);
      dropSite(state, drop, key);
      break;
    case 'domain': {
      if (!state.domains[key]) throw new UsageError(`${key} is not set up as a domain`);
      drop.domains.add(key);
      const domains = Object.keys(state.domains);
      // hosts of a more specific domain (shop.example.com inside example.com) stay with it
      for (const host of Object.keys(state.sites)) if (domainOf(host, domains) === key) dropSite(state, drop, host);
      for (const [k, p] of Object.entries(state.paths)) if (domainOf(p.host, domains) === key) drop.paths.add(k);
      break;
    }
    case 'cert': {
      if (!state.certs[key]) throw new UsageError(`no certificate named ${key}`);
      drop.certs.add(key);
      const to = target.replaceCert;
      if (to) {
        if (to === key) throw new UsageError('pick a different certificate to move to');
        if (!state.certs[to]) throw new UsageError(`no certificate named ${to}`);
      }
      for (const [host, s] of Object.entries(state.sites)) {
        if (s.cert !== key) continue;
        if (!to) dropSite(state, drop, host);
        else if (!certCovers(state.certs[to].names || [], host)) throw new UsageError(`${to} does not cover ${host}`);
        else moved.sites[host] = to;
      }
      // a domain's cert is only its default — point it elsewhere, never drop the domain
      for (const [d, v] of Object.entries(state.domains)) if (v.cert === key) moved.domains[d] = to || null;
      break;
    }
    case 'app':
      if (!linked.includes(key) && !routes(state).some((r) => r.source.type === 'app' && r.source.app === key)) {
        throw new UsageError(`no app named ${key} — see \`edge app ls\``);
      }
      if (linked.includes(key)) drop.apps.add(key);
      dropRoutesWhere(state, drop, (src) => src.type === 'app' && src.app === key);
      break;
    case 'www':
      if (!/^[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)*$/.test(key) || key.split('/').includes('..')) throw new UsageError(`invalid folder: ${key}`);
      if (!fs.existsSync(path.join(P.www, key)) && !wwwUsed(state).has(key)) throw new UsageError(`www/${key} does not exist`);
      drop.www.add(key);
      dropRoutesWhere(state, drop, (src) => src.type === 'static' && src.dir === key);
      break;
    case 'all':
      for (const k of Object.keys(state.domains)) drop.domains.add(k);
      for (const k of Object.keys(state.sites)) drop.sites.add(k);
      for (const k of Object.keys(state.paths)) drop.paths.add(k);
      for (const k of Object.keys(state.certs)) drop.certs.add(k);
      for (const k of linked) drop.apps.add(k);
      break;
    default:
      throw new UsageError(`can't remove a ${kind}`);
  }

  // edge.json afterwards
  const next = clone(state);
  for (const k of drop.domains) delete next.domains[k];
  for (const k of drop.sites) delete next.sites[k];
  for (const k of drop.paths) delete next.paths[k];
  for (const k of drop.certs) delete next.certs[k];
  for (const [h, to] of Object.entries(moved.sites)) next.sites[h].cert = to;
  for (const [d, to] of Object.entries(moved.domains)) next.domains[d].cert = to;

  // left unused by this removal (never things that were unused already)
  const usedAfter = certsUsed(next);
  const usedBefore = [
    ...[...drop.sites].map((h) => state.sites[h].cert),
    ...[...drop.domains].map((d) => state.domains[d].cert),
  ];
  const extraCerts = [...new Set(usedBefore)].filter((n) => n && next.certs[n] && !usedAfter.has(n));

  const wwwAfter = wwwUsed(next);
  const wwwBefore = [...drop.sites].map((h) => state.sites[h].source).concat([...drop.paths].map((k) => state.paths[k].source));
  const extraWww = [...new Set(wwwBefore.filter((s) => s.type === 'static').map((s) => s.dir))].filter((d) => !drop.www.has(d) && !wwwAfter.has(d));
  if (kind === 'all') for (const d of wwwUsed(state)) if (!extraWww.includes(d)) extraWww.push(d);

  const dns = [];
  for (const h of drop.sites) {
    const s = state.sites[h];
    if (s.dns !== 'proxied' && s.dns !== 'dns-only') continue;
    dns.push(h);
    if (state.domains[h]) dns.push(`www.${h}`); // `edge domain add` points www too
  }

  return {
    target,
    drop,
    moved,
    next,
    extras: { certs: extraCerts.sort(), www: extraWww.sort(), dns, down: [...drop.apps].sort() },
  };
}

/** Anything besides the target itself? */
export function cascades(plan) {
  const { kind, key } = plan.target;
  const d = plan.drop;
  const own = { domain: d.domains, site: d.sites, path: d.paths, cert: d.certs, app: d.apps, www: d.www }[kind];
  const total = Object.values(d).reduce((n, set) => n + set.size, 0);
  return kind !== 'all' && total > (own?.has(key) ? 1 : 0);
}

// ═══════════════════════════════════════════════════════════════ show ═════

const list = (items) => [...items].join(', ');

export function printPlan(state, plan) {
  const { kind, key } = plan.target;
  const d = plan.drop;
  heading(kind === 'all' ? 'Remove everything edge manages' : `Remove ${KINDS[kind]} ${key}`);
  const row = (label, value, color = (s) => s) => value && console.log(`    ${c.gray(label.padEnd(12))} ${color(value)}`);
  row('domains', list(d.domains));
  for (const h of d.sites) row(h === [...d.sites][0] ? 'hosts' : '', `${h} ${c.gray(`→ ${describeSource(state.sites[h].source)}`)}`);
  row('paths', list(d.paths));
  row('certificates', [...d.certs].map((n) => `${n} ${c.gray(`(${certs.certLabel(state.certs[n])})`)}`).join(', '));
  row('apps', [...d.apps].map((a) => `apps/${a}`).join(', '), (s) => `${s} ${c.gray('(unlinked — the project stays)')}`);
  row('folder', [...d.www].map((w) => `www/${w}`).join(', '));
  for (const [h, to] of Object.entries(plan.moved.sites)) row('moves', `${h} ${c.gray('→ certificate')} ${to}`);
  for (const [dm, to] of Object.entries(plan.moved.domains)) row('domain cert', `${dm} ${c.gray('→')} ${to || c.gray('none')}`);
  console.log('');
}

// ══════════════════════════════════════════════════════════════ apply ═════

function extraChoices(plan) {
  const e = plan.extras;
  return [
    ...e.certs.map((n) => ({ value: `cert:${n}`, label: `certificate ${n}`, hint: 'nothing else uses it — its files are deleted' })),
    ...e.www.filter((w) => fs.existsSync(path.join(P.www, w))).map((w) => ({ value: `www:${w}`, label: `www/${w}`, hint: 'static files — deleted from disk' })),
    ...e.dns.map((h) => ({ value: `dns:${h}`, label: `DNS ${h}`, hint: 'A/AAAA records in Cloudflare' })),
    ...e.down.map((a) => ({ value: `down:${a}`, label: `stop ${a}`, hint: 'docker compose down — data volumes are kept' })),
  ];
}

/** Which extras to delete: --purge = all, --purge-dns = DNS, a terminal = ask (certs pre-ticked), else none. */
async function pickExtras(opts, plan) {
  const choices = extraChoices(plan);
  if (!choices.length) return new Set();
  if (opts.purge) return new Set(choices.map((ch) => ch.value));
  if (canPrompt() && !opts.yes) {
    return new Set(
      await multiselect({
        message: 'Also delete what nothing else uses?',
        choices,
        initial: choices.filter((ch) => ch.value.startsWith('cert:')).map((ch) => ch.value),
        hint: 'space toggles',
      }),
    );
  }
  return new Set(choices.filter((ch) => opts['purge-dns'] && ch.value.startsWith('dns:')).map((ch) => ch.value));
}

const safeWww = (dir) => {
  const abs = path.resolve(P.www, dir);
  if (!abs.startsWith(P.www + path.sep) || abs === P.www) throw new Error(`refusing to delete ${abs}`);
  return abs;
};

/** Make it so: stop apps, one nginx commit, then files, app links and DNS. */
export async function applyRemoval(state, plan, picked = new Set()) {
  const has = (prefix) => [...picked].filter((v) => v.startsWith(prefix)).map((v) => v.slice(prefix.length));
  const certsGone = [...plan.drop.certs, ...has('cert:')];
  const next = clone(plan.next);
  for (const n of has('cert:')) delete next.certs[n];

  // containers first: `down` needs apps/<name>/edge.override.yaml, which unlinking deletes
  for (const name of has('down:')) {
    const app = apps.getApp(name);
    if (!app || app.broken) continue;
    await task(`Stopping ${name}`, async () => {
      const res = await apps.appCompose(app, ['down']);
      if (res.code !== 0) throw new Error(res.stderr.trim().split('\n').slice(-4).join('\n'));
    }).catch((err) => log.warn(err.message));
  }

  await commit(next);

  for (const n of certsGone) {
    await task(`Deleting certificate ${n}`, () => certs.removeCertFiles(n, state.certs[n])).catch((err) => log.warn(err.message));
  }
  for (const w of new Set([...plan.drop.www, ...has('www:')])) {
    const abs = safeWww(w);
    if (fs.existsSync(abs)) {
      fs.rmSync(abs, { recursive: true, force: true });
      log.ok(`deleted www/${w}`);
    }
  }
  for (const name of plan.drop.apps) {
    if (!apps.getApp(name)) continue;
    apps.unlinkApp(name);
    log.ok(`apps/${name} unlinked ${c.gray('— the project itself is untouched')}`);
  }
  const dnsHosts = has('dns:');
  if (dnsHosts.length) {
    const token = readToken();
    if (!token) log.warn(`no Cloudflare token — delete the DNS records for ${dnsHosts.join(', ')} yourself`);
    for (const h of token ? dnsHosts : []) {
      const zone = await zoneFor(state, h.replace(/^www\./, ''), token);
      if (!zone) {
        log.warn(`${h}: no Cloudflare zone found — DNS record left alone`);
        continue;
      }
      await task(`Deleting DNS record ${h}`, async (t) => {
        const n = await cf.deleteRecords(token, zone.id, h);
        t.update(`Deleting DNS record ${h} ${c.gray(n ? `(${n} removed)` : '(none found)')}`);
      }).catch((err) => log.warn(err.message));
    }
  }
}

// ═══════════════════════════════════════════════════════════════ flow ═════

const label = (t) => (t.kind === 'all' ? 'everything' : `${KINDS[t.kind]} ${t.key}`);

/** For a certificate in use, ask (or read --cert / --force) what happens to its users. */
async function certUsers(opts, state, target) {
  const users = Object.entries(state.sites).filter(([, s]) => s.cert === target.key).map(([h]) => h);
  if (!users.length || opts.force) return target;
  if (opts.cert) return { ...target, replaceCert: opts.cert };
  const others = Object.keys(state.certs).filter((n) => n !== target.key && users.every((h) => certCovers(state.certs[n].names || [], h)));
  if (!canPrompt()) {
    throw new UsageError(`${target.key} is used by ${users.join(', ')} — add --cert <other> to move ${users.length === 1 ? 'it' : 'them'}${others.length ? ` (${others.join(', ')} covers them)` : ''}, or --force to remove them too`);
  }
  const pick = await select({
    message: `${target.key} is used by ${users.join(', ')}. What should happen to ${users.length === 1 ? 'it' : 'them'}?`,
    choices: [
      ...others.map((n) => ({ value: `move:${n}`, label: `Switch to ${n}`, hint: `${certs.certLabel(state.certs[n])} · ${state.certs[n].names.join(', ')}` })),
      { value: 'drop', label: users.length === 1 ? 'Remove that host too' : 'Remove those hosts too', hint: 'and their paths' },
      { value: 'cancel', label: 'Cancel' },
    ],
  });
  if (pick === 'cancel') return null;
  return pick === 'drop' ? target : { ...target, replaceCert: pick.slice(5) };
}

/**
 * Remove one thing. target = { kind, key }. Shows what goes with it, asks,
 * and applies. Off a terminal: a cascade needs --force, extras need --purge.
 */
export async function flowRemove(opts, target) {
  const state = loadState();
  const linked = apps.listApps().map((a) => a.name);
  if (target.kind === 'cert') {
    target = await certUsers(opts, state, target);
    if (!target) return 1;
  }
  const plan = planRemoval(state, target, { linked });
  printPlan(state, plan);

  if (target.kind === 'all') {
    if (canPrompt() && !opts.yes) {
      const typed = await input({ message: 'Type "reset" to remove everything above', validate: (v) => (v === 'reset' ? null : 'type reset, or Esc to cancel') });
      if (typed !== 'reset') return 1;
    } else if (!(opts.yes && opts.force)) {
      throw new UsageError('edge reset needs --yes --force off a terminal');
    }
  } else if (cascades(plan)) {
    if (!canPrompt() && !opts.force) throw new UsageError(`removing ${label(target)} also removes the routes listed above — add --force`);
    if (canPrompt() && !opts.yes && !(await confirm({ message: `Remove ${label(target)} and everything listed?`, initial: false }))) return 1;
  } else if (canPrompt() && !opts.yes && !(await confirm({ message: `Remove ${label(target)}?`, initial: false }))) {
    return 1;
  }

  const picked = await pickExtras(opts, plan);
  await applyRemoval(state, plan, picked);
  console.log('');
  console.log(`  ${c.green(sym.ok)} ${c.bold(`${label(target)} removed`)}`);
  console.log('');
  return 0;
}

/** `edge rm <host>[/<path>]` */
export function routeTarget(arg) {
  const t = parseTarget(arg);
  const state = loadState();
  if (t.path) return { kind: 'path', key: t.key };
  if (!state.sites[t.host] && state.domains[t.host]) return { kind: 'domain', key: t.host };
  return { kind: 'site', key: t.host };
}

/** Folders in www/ with the routes that serve them. */
export function wwwDirs(state = loadState()) {
  if (!fs.existsSync(P.www)) return [];
  const users = new Map();
  for (const r of routes(state)) if (r.source.type === 'static') users.set(r.source.dir, [...(users.get(r.source.dir) || []), r.key]);
  return fs
    .readdirSync(P.www, { withFileTypes: true })
    .filter((d) => d.isDirectory() && !d.name.startsWith('.'))
    .map((d) => ({ dir: d.name, routes: users.get(d.name) || [] }))
    .sort((a, b) => a.dir.localeCompare(b.dir));
}

/** "What do you want to remove?" → which one → flowRemove. */
export async function pickAndRemove(opts, kind) {
  const state = loadState();
  const linked = apps.listApps();
  const www = wwwDirs(state);
  const count = {
    domain: Object.keys(state.domains).length,
    site: Object.keys(state.sites).length,
    path: Object.keys(state.paths).length,
    app: linked.length,
    cert: Object.keys(state.certs).length,
    www: www.length,
  };
  const none = (n) => !n && 'none';
  if (!kind) {
    if (!canPrompt()) throw new UsageError('usage: edge rm <host>[/<path>]  (or domain rm, app rm, cert rm, www rm, reset)');
    kind = await select({
      message: 'What do you want to remove?',
      choices: [
        { value: 'domain', label: 'Domain', hint: `${count.domain} · with its hosts, paths, certificate and DNS`, disabled: none(count.domain) },
        { value: 'site', label: 'Host (subdomain)', hint: `${count.site} · with its paths`, disabled: none(count.site) },
        { value: 'path', label: 'Path', hint: `${count.path}`, disabled: none(count.path) },
        { value: 'app', label: 'App', hint: `${count.app} · unlink from apps/, with the routes that use it`, disabled: none(count.app) },
        { value: 'cert', label: 'Certificate', hint: `${count.cert} · switch or remove the hosts using it`, disabled: none(count.cert) },
        { value: 'www', label: 'Static folder', hint: `${count.www} · a folder in www/`, disabled: none(count.www) },
        { separator: 'careful' },
        { value: 'all', label: 'Everything (reset)', hint: 'every route, domain, certificate and app link' },
      ],
    });
  }
  if (kind === 'all') return flowRemove(opts, { kind: 'all', key: '' });

  const choices = {
    domain: () => Object.keys(state.domains).map((d) => ({ value: d, label: d, hint: state.sites[d] ? describeSource(state.sites[d].source) : 'apex not served' })),
    site: () => Object.entries(state.sites).map(([h, s]) => ({ value: h, label: h, hint: describeSource(s.source) })),
    path: () => Object.entries(state.paths).map(([k, p]) => ({ value: k, label: k, hint: describeSource(p.source) })),
    app: () => linked.map((a) => ({ value: a.name, label: a.name, hint: a.broken ? `broken: ${a.broken}` : shorten(a.projectDir) })),
    cert: () => Object.entries(state.certs).map(([n, k]) => ({ value: n, label: n, hint: `${certs.certLabel(k)} · ${(k.names || []).join(', ')}` })),
    www: () => www.map((w) => ({ value: w.dir, label: `www/${w.dir}`, answer: w.dir, hint: w.routes.length ? `served at ${w.routes.join(', ')}` : 'not served' })),
  }[kind];
  if (!choices) throw new UsageError(`can't remove a ${kind}`);
  const items = choices();
  if (!items.length) {
    log.hint(`no ${KINDS[kind]}s to remove`);
    return 0;
  }
  const key = await select({ message: `Remove which ${KINDS[kind]}?`, choices: items });
  return flowRemove(opts, { kind, key });
}
