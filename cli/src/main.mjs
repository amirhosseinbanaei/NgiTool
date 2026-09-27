import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';

import { parseArgs, helpText } from './args.mjs';
import { P, readEnv, loadState, readToken, writeToken } from './config.mjs';
import { compose, stackStatus, networkInfo, listContainers, nginxServesThis } from './exec.mjs';
import { commit, testConfig, reloadNginx } from './nginx.mjs';
import * as certs from './certs.mjs';
import * as cf from './cloudflare.mjs';
import * as apps from './apps.mjs';
import * as flows from './flows.mjs';
import { UsageError, describeSource, CERT_TYPES } from './targets.mjs';
import { select, confirm, canPrompt, Cancelled } from './prompt.mjs';
import { c, log, sym, setColor, task, heading, table, daysLeft, shorten } from './ui.mjs';

const PKG = JSON.parse(fs.readFileSync(path.join(P.root, 'package.json'), 'utf8'));

export class ExitError extends Error {
  constructor(code) {
    super(`exit ${code}`);
    this.code = code;
  }
}

export async function main(argv) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (err) {
    log.err(err.message);
    console.error(c.gray('  Run `edge --help`.'));
    return 2;
  }
  if (opts['no-color']) setColor(false);
  if (opts.version) {
    console.log(PKG.version);
    return 0;
  }
  if (opts.help) {
    console.log(helpText());
    return 0;
  }

  try {
    if (!opts._.length) {
      if (!canPrompt()) {
        console.log(helpText());
        return 0;
      }
      return await menu(opts);
    }
    return (await dispatch(opts._, opts)) ?? 0;
  } catch (err) {
    if (err instanceof Cancelled) {
      log.plain(c.gray('Cancelled.'));
      return 130;
    }
    if (err instanceof UsageError) {
      log.err(err.message);
      return 2;
    }
    throw err;
  }
}

// ═══════════════════════════════════════════════════════════ dispatch ═════

async function dispatch([cmd, ...rest], opts) {
  const [sub, ...args] = rest;
  switch (cmd) {
    case 'init':
      return flows.flowInit(opts);
    case 'status':
    case 'st':
      return status();
    case 'ls':
    case 'list':
    case 'routes':
      return printRoutes(loadState(), await liveness());

    case 'domain':
    case 'domains':
      if (!sub || sub === 'ls') return printDomains();
      if (sub === 'add') return flows.flowDomain(opts, args[0]);
      if (sub === 'rm' || sub === 'remove') return removeDomain(opts, args[0]);
      break;
    case 'site':
    case 'sub':
    case 'subdomain':
      if (sub === 'add') return flows.flowSite(opts, args[0]);
      if (!sub || sub === 'ls') return printRoutes(loadState(), await liveness());
      break;
    case 'path':
      if (sub === 'add') return flows.flowPath(opts, args[0]);
      if (!sub || sub === 'ls') return printRoutes(loadState(), await liveness());
      break;
    case 'rm':
    case 'remove':
      if (!sub) throw new UsageError('usage: edge rm <host>[/<path>]');
      return flows.removeRoute(opts, sub);
    case 'enable':
    case 'disable':
      if (!sub) throw new UsageError(`usage: edge ${cmd} <host>[/<path>]`);
      return flows.toggleRoute(sub, cmd === 'enable');

    case 'app':
    case 'apps':
      return appCommand(sub, args, opts);

    case 'cert':
    case 'certs':
      if (!sub || sub === 'ls') return printCerts(loadState());
      if (sub === 'add') return flows.flowCertAdd(opts, args[0]);
      if (sub === 'rm' || sub === 'remove') return flows.flowCertRemove(opts, args[0]);
      if (sub === 'renew') return renew();
      if (sub === 'aop') {
        if (!args[0] || !['on', 'off'].includes(args[1])) throw new UsageError('usage: edge cert aop <name> on|off');
        return flows.flowCertAop(args[0], args[1] === 'on');
      }
      break;

    case 'up':
      return stackUp();
    case 'down':
      return stackRun(['down'], 'Stopping nginx-edge');
    case 'restart':
      return stackRun(['restart'], 'Restarting nginx-edge');
    case 'reload':
      return reload();
    case 'test': {
      const t = await task('Checking nginx configuration', async () => {
        const r = await testConfig();
        if (!r.ok) throw new Error(r.output);
        return r;
      }).catch((err) => {
        console.error(c.red(err.message));
        return null;
      });
      return t ? 0 : 1;
    }
    case 'logs':
      return logs(sub);
    case 'sync':
      await commit(loadState());
      log.ok('nginx files regenerated from edge.json');
      return 0;
    case 'cf-sync':
      return cfSync();
    case 'install':
      return install();
    case 'help':
      console.log(helpText());
      return 0;
    default:
      break;
  }
  throw new UsageError(`unknown command: ${[cmd, sub].filter(Boolean).join(' ')} — run \`edge --help\``);
}

// ═════════════════════════════════════════════════════════════ status ═════

/** What is alive right now: running container names and whether the network exists. */
async function liveness() {
  const containers = await listContainers();
  return { running: new Set(containers.filter((k) => k.state === 'running').map((k) => k.name)), containers };
}

function routeAlive(src, live) {
  switch (src.type) {
    case 'app':
    case 'container': {
      const name = src.type === 'app' ? src.upstream : src.name;
      if (live.running.has(name)) return true;
      // aliases resolve to containers of that compose service
      return live.containers.some((k) => k.state === 'running' && src.type === 'app' && k.service === src.service && k.workingDir === apps.getApp(src.app)?.projectDir);
    }
    case 'static':
      return fs.existsSync(path.join(P.www, src.dir));
    default:
      return null;
  }
}

function printRoutes(state, live) {
  const routes = flows.routeList(state);
  heading('Routes', routes.length ? `${routes.length}` : '');
  if (!routes.length) {
    log.hint('nothing is served yet — edge site add / edge path add');
    return 0;
  }
  const rows = routes.map((r) => {
    const alive = routeAlive(r.source, live);
    const dot =
      r.enabled === false ? c.gray(sym.ring) : alive === false ? c.red(sym.dot) : alive ? c.green(sym.dot) : c.cyan(sym.dot);
    const name = r.kind === 'path' ? `  ${c.gray('└')} ${r.key}` : c.bold(r.key);
    const extra = [];
    if (r.kind === 'site') extra.push(r.dns === 'proxied' ? c.yellow('proxied') : r.dns === 'dns-only' ? 'dns-only' : c.gray('dns manual'));
    if (r.kind === 'site') extra.push(c.gray(`cert ${r.cert}`));
    if (r.strip) extra.push(c.gray('strip'));
    if (r.enabled === false) extra.push(c.gray('disabled'));
    if (alive === false && r.enabled !== false) extra.push(c.red('upstream down'));
    return [dot, name, `${c.gray('→')} ${describeSource(r.source)}`, extra.join(c.gray(' · '))];
  });
  console.log(table(rows, { indent: 2 }).join('\n'));
  console.log('');
  return 0;
}

function printCerts(state) {
  const list = certs.listCerts(state);
  heading('Certificates', list.length ? `${list.length}` : '');
  if (!list.length) {
    log.hint('none yet — edge domain add example.com');
    return 0;
  }
  const rows = list.map((k) => {
    const days = k.info ? (k.info.validTo - Date.now()) / 86_400_000 : -1;
    const mark = k.problem ? c.red(sym.err) : days < 14 ? c.red(sym.warn) : days < 30 ? c.yellow(sym.warn) : c.green(sym.ok);
    return [
      mark,
      c.bold(k.name),
      CERT_TYPES[k.type] || k.type,
      k.problem ? c.red(k.problem) : daysLeft(k.info.validTo),
      c.gray(k.names.join(', ') + (k.aop ? ' · AOP on' : '')),
    ];
  });
  console.log(table(rows, { indent: 2 }).join('\n'));
  console.log('');
  return 0;
}

function printDomains() {
  const state = loadState();
  heading('Domains');
  const rows = Object.entries(state.domains).map(([d, v]) => [
    c.bold(d),
    c.gray(`cert ${v.cert}`),
    v.zone ? c.gray(`zone ${v.zone.id.slice(0, 8)}…`) : c.yellow('not in Cloudflare'),
    state.sites[d] ? describeSource(state.sites[d].source) : c.gray('apex not served'),
  ]);
  console.log(rows.length ? table(rows).join('\n') : c.gray('    none yet — edge domain add example.com'));
  console.log('');
  return 0;
}

function printApps(live, state) {
  const list = apps.listApps();
  heading('Apps', `${shorten(P.apps)}/`);
  if (!list.length) {
    log.hint('none linked — edge app link <project> or edge app scan');
    return;
  }
  const used = new Map();
  for (const r of flows.routeList(state)) if (r.source.type === 'app') used.set(r.source.app, [...(used.get(r.source.app) || []), r.key]);
  const rows = list.map((a) => {
    if (a.broken) return [c.red(sym.err), c.bold(a.name), c.red(a.broken), ''];
    const s = apps.appState(a, live.containers);
    const dot = !s.total ? c.gray(sym.ring) : s.running === s.total ? c.green(sym.dot) : s.running ? c.yellow(sym.dot) : c.red(sym.dot);
    const st = s.total ? `${s.running}/${s.total} running` : 'not started';
    const routes = used.get(a.name);
    return [dot, c.bold(a.name), st, c.gray(`${shorten(a.target)}${a.override ? ' + override' : ''}${routes ? ` · ${routes.join(', ')}` : ''}`)];
  });
  console.log(table(rows, { indent: 2 }).join('\n'));
}

async function status() {
  const env = readEnv();
  const state = loadState();
  const [stack, net, live] = await Promise.all([stackStatus(), networkInfo(env.EDGE_NETWORK), liveness()]);

  heading('nginx-edge', shorten(P.root));
  const svc = (name) => {
    const s = stack[name];
    if (!s) return `${c.gray(sym.ring)} ${name} ${c.gray('not created')}`;
    const ok = s.state === 'running' && (!s.health || s.health === 'healthy');
    return `${ok ? c.green(sym.dot) : s.state === 'running' ? c.yellow(sym.dot) : c.red(sym.dot)} ${name} ${c.gray(s.health || s.state)}`;
  };
  console.log(`    ${svc('nginx')}   ${svc('certbot')}`);
  console.log(
    `    ${net ? `${c.green(sym.dot)} network ${c.bold(net.name)} ${c.gray(`${net.subnet} · gateway ${net.gateway}`)}` : `${c.red(sym.dot)} network ${env.EDGE_NETWORK} ${c.red('missing — edge init')}`}`,
  );
  const token = readToken();
  const notes = [];
  if (!token) notes.push('no Cloudflare token (certbot, DNS records off)');
  if (!env.SERVER_IP) notes.push('SERVER_IP not set (DNS records off)');
  if (!env.ACME_EMAIL) notes.push('ACME_EMAIL not set');
  if (notes.length) console.log(`    ${c.yellow(sym.warn)} ${c.yellow(notes.join(' · '))}`);

  printCerts(state);
  printRoutes(state, live);
  printApps(live, state);
  console.log('');
  return 0;
}

// ═══════════════════════════════════════════════════════════════ apps ═════

async function appCommand(sub, args, opts) {
  switch (sub) {
    case undefined:
    case 'ls':
    case 'list':
      printApps(await liveness(), loadState());
      console.log('');
      return 0;
    case 'link':
    case 'add':
      await flows.flowAppLink(opts, args[0]);
      return 0;
    case 'scan':
      await flows.flowAppScan(opts, args);
      return 0;
    case 'unlink':
    case 'rm':
      await flows.flowAppUnlink(opts, args[0]);
      return 0;
    case 'up':
    case 'down':
    case 'restart':
    case 'ps':
    case 'logs':
    case 'pull':
    case 'build':
      return appRun(sub, args[0], args.slice(1));
    default:
      throw new UsageError(`unknown app command: ${sub} — run \`edge --help\``);
  }
}

async function appRun(action, name, services) {
  if (!name) {
    if (!canPrompt()) throw new UsageError(`usage: edge app ${action} <name>`);
    name = await select({ message: `${action} which app?`, choices: apps.listApps().filter((a) => !a.broken).map((a) => ({ value: a.name, label: a.name, hint: shorten(a.target) })) });
  }
  const app = apps.getApp(name);
  if (!app) throw new UsageError(`no app named ${name} — see \`edge app ls\``);
  if (app.broken) throw new UsageError(`apps/${name} is broken: ${app.broken}`);
  const args = {
    up: ['up', '-d', '--remove-orphans'],
    down: ['down'],
    restart: ['restart'],
    ps: ['ps'],
    logs: ['logs', '-f', '--tail', '100'],
    pull: ['pull'],
    build: ['build'],
  }[action];
  log.step(c.gray(`docker compose ${args[0]} in ${shorten(app.projectDir)}${app.override ? ' (+ edge override)' : ''}`));
  const res = await apps.appCompose(app, [...args, ...services], { inherit: true });
  return res.code;
}

// ══════════════════════════════════════════════════════════════ stack ═════

async function stackUp() {
  const env = readEnv();
  if (!(await networkInfo(env.EDGE_NETWORK))) throw new UsageError(`network ${env.EDGE_NETWORK} does not exist — run \`edge init\``);
  if (!fs.existsSync(P.cfIni)) writeToken(null); // certbot mounts it; must be a file, not a dir Docker invents
  await commit(loadState(), { quiet: true });
  return stackRun(['up', '-d', '--quiet-pull', '--remove-orphans'], 'Starting nginx and certbot');
}

async function stackRun(args, label) {
  const res = await task(label, () => compose(args));
  if (res.code !== 0) {
    console.error(c.red(res.stderr.trim().split('\n').slice(-8).join('\n')));
    return 1;
  }
  return 0;
}

async function reload() {
  const running = (await stackStatus()).nginx?.state === 'running';
  await task('Checking nginx configuration', async () => {
    const r = await testConfig();
    if (!r.ok) throw new Error(r.output);
  });
  if (!running) {
    log.hint('nginx is not running — edge up');
    return 0;
  }
  if (!(await nginxServesThis())) {
    log.hint('nginx is running from another copy of nginx-edge — edge up recreates it here');
    return 0;
  }
  await task('Reloading nginx', reloadNginx);
  return 0;
}

async function renew() {
  const res = await task("Renewing Let's Encrypt certificates that are due", () => certs.renewLetsEncrypt());
  const summary = res.stdout.split('\n').filter((l) => /renew|skipped|success|fail|not due/i.test(l));
  for (const l of summary.slice(-10)) log.hint(l.trim());
  if (res.code !== 0) {
    console.error(c.red(res.stderr.trim().split('\n').slice(-8).join('\n')));
    return 1;
  }
  return reload();
}

function logs(host) {
  return new Promise((resolve) => {
    const child = spawn('docker', ['compose', 'logs', '-f', '--tail', '200', '--no-log-prefix', 'nginx'], {
      cwd: P.root,
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    let buf = '';
    child.stdout.on('data', (d) => {
      buf += d;
      const lines = buf.split('\n');
      buf = lines.pop();
      for (const l of lines) if (!host || l.includes(` ${host} `)) console.log(colorLog(l));
    });
    child.on('close', (code) => resolve(code ?? 0));
  });
}

/** Colour the status code in an access-log line (format from conf/nginx.conf). */
function colorLog(line) {
  return line.replace(/" (\d{3}) /, (m, code) => {
    const col = code >= 500 ? c.red : code >= 400 ? c.yellow : code >= 300 ? c.cyan : c.green;
    return `" ${col(code)} `;
  });
}

async function cfSync() {
  await task('Fetching Cloudflare IP ranges and origin-pull CA', async (t) => {
    const r = await cf.syncCloudflareFiles();
    t.update(`Fetched ${r.ranges} Cloudflare IP ranges${r.aop ? ' and the origin-pull CA' : ''}`);
  });
  if ((await stackStatus()).nginx?.state === 'running') return reload();
  return 0;
}

async function install() {
  const target = '/usr/local/bin/edge';
  const shim = path.join(P.root, 'edge');
  try {
    const cur = fs.readlinkSync(target);
    if (cur === shim) {
      log.ok(`${target} already points here`);
      return 0;
    }
    if (!(await confirm({ message: `${target} points to ${cur}. Replace it?`, initial: false }))) return 1;
    fs.rmSync(target);
  } catch {
    if (fs.existsSync(target)) throw new UsageError(`${target} exists and is not a symlink — remove it first`);
  }
  fs.symlinkSync(shim, target);
  log.ok(`${target} → ${shim}`);
  log.hint('run `edge` from anywhere');
  return 0;
}

async function removeDomain(opts, domain) {
  const state = loadState();
  if (!domain) {
    if (!canPrompt()) throw new UsageError('usage: edge domain rm <domain>');
    domain = await select({ message: 'Remove which domain?', choices: Object.keys(state.domains).map((d) => ({ value: d, label: d })) });
  }
  if (!state.domains[domain]) throw new UsageError(`${domain} is not set up`);
  const hosts = Object.keys(state.sites).filter((h) => h === domain || h.endsWith(`.${domain}`));
  if (hosts.filter((h) => h !== domain).length) {
    throw new UsageError(`remove its hosts first: ${hosts.filter((h) => h !== domain).join(', ')}`);
  }
  if (!opts.yes && canPrompt() && !(await confirm({ message: `Remove ${domain}? (its certificate is kept — edge cert rm to delete it)`, initial: false }))) return 1;
  if (state.sites[domain]) await flows.removeRoute({ ...opts, force: true }, domain);
  const next = loadState();
  delete next.domains[domain];
  await commit(next, { quiet: true });
  log.ok(`${domain} removed`);
  return 0;
}

// ═══════════════════════════════════════════════════════════════ menu ═════

async function menu(opts) {
  for (;;) {
    const env = readEnv();
    const state = loadState();
    const stack = await stackStatus();
    const nginxUp = stack.nginx?.state === 'running';
    const counts = [
      `${Object.keys(state.domains).length} domains`,
      `${Object.keys(state.sites).length + Object.keys(state.paths).length} routes`,
      `${apps.listApps().length} apps`,
    ].join(' · ');
    console.log('');
    console.log(
      `  ${c.bold('nginx-edge')}  ${nginxUp ? c.green(`${sym.dot} running`) : c.red(`${sym.dot} stopped`)}  ${c.gray(counts)}  ${c.gray(`network ${env.EDGE_NETWORK}`)}`,
    );
    console.log('');
    let choice;
    try {
      choice = await select({
        message: 'What do you want to do?',
        choices: [
          { value: 'site', label: 'Add a subdomain', hint: 'serve a project on api.example.com' },
          { value: 'path', label: 'Add a path', hint: 'serve a project on example.com/admin' },
          { value: 'domain', label: 'Add a domain', hint: 'certificate + DNS for a new domain' },
          { value: 'link', label: 'Link an app', hint: 'symlink a project’s docker compose into apps/' },
          { separator: 'manage' },
          { value: 'status', label: 'Status', hint: 'stack, certificates, routes, apps' },
          { value: 'routes', label: 'Routes', hint: 'enable, disable, replace, remove' },
          { value: 'apps', label: 'Apps', hint: 'start, stop, logs, scan, unlink' },
          { value: 'certs', label: 'Certificates', hint: 'list, add, renew, remove' },
          { value: 'stack', label: 'Stack', hint: nginxUp ? 'reload, restart, stop, logs' : 'start nginx-edge' },
          { value: 'quit', label: 'Quit' },
        ],
      });
    } catch (err) {
      if (err instanceof Cancelled) return 0;
      throw err;
    }
    if (choice === 'quit') return 0;
    console.log('');
    try {
      if (choice === 'site') await flows.flowSite(opts);
      else if (choice === 'path') await flows.flowPath(opts);
      else if (choice === 'domain') await flows.flowDomain(opts);
      else if (choice === 'link') await flows.flowAppLink(opts);
      else if (choice === 'status') await status();
      else if (choice === 'routes') await routesMenu(opts);
      else if (choice === 'apps') await appsMenu(opts);
      else if (choice === 'certs') await certsMenu(opts);
      else if (choice === 'stack') await stackMenu(nginxUp);
    } catch (err) {
      if (err instanceof Cancelled) continue;
      log.err(err.message);
      if (process.env.DEBUG) console.error(c.gray(err.stack));
    }
  }
}

async function routesMenu(opts) {
  const state = loadState();
  const routes = flows.routeList(state);
  if (!routes.length) {
    log.hint('nothing is served yet');
    return;
  }
  const key = await select({
    message: 'Which route?',
    choices: routes.map((r) => ({
      value: r.key,
      label: r.kind === 'path' ? `  └ ${r.key}` : r.key,
      answer: r.key,
      hint: `${describeSource(r.source)}${r.enabled === false ? ' · disabled' : ''}`,
    })),
  });
  const r = routes.find((x) => x.key === key);
  const action = await select({
    message: key,
    choices: [
      r.enabled === false ? { value: 'enable', label: 'Enable' } : { value: 'disable', label: 'Disable', hint: 'keep it in edge.json, stop serving it' },
      { value: 'replace', label: 'Change where it points', hint: 'pick a new app, container, port or folder' },
      { value: 'remove', label: 'Remove' },
    ],
  });
  if (action === 'enable' || action === 'disable') return flows.toggleRoute(key, action === 'enable');
  if (action === 'remove') {
    if (!(await confirm({ message: `Remove ${key}?`, initial: false }))) return;
    return flows.removeRoute(opts, key);
  }
  const force = { ...opts, force: true };
  if (r.kind === 'site') return flows.flowSite({ ...force, cert: r.cert, dns: r.dns }, key);
  return flows.flowPath(force, key);
}

async function appsMenu(opts) {
  const list = apps.listApps();
  const live = await liveness();
  const name = await select({
    message: 'Which app?',
    choices: [
      ...list.map((a) => {
        const s = a.broken ? null : apps.appState(a, live.containers);
        return {
          value: a.name,
          label: a.name,
          hint: a.broken ? `broken: ${a.broken}` : `${s.total ? `${s.running}/${s.total} running` : 'not started'} · ${shorten(a.projectDir)}`,
        };
      }),
      { value: '+link', label: '＋ Link a project…', hint: 'one project folder or compose file' },
      { value: '+scan', label: '＋ Scan for projects…', hint: 'find compose files under /home and pick' },
    ],
  });
  if (name === '+link') return flows.flowAppLink(opts);
  if (name === '+scan') return flows.flowAppScan(opts, []);
  const action = await select({
    message: name,
    choices: [
      { value: 'up', label: 'Start / update', hint: 'docker compose up -d' },
      { value: 'restart', label: 'Restart' },
      { value: 'down', label: 'Stop', hint: 'docker compose down' },
      { value: 'ps', label: 'Containers', hint: 'docker compose ps' },
      { value: 'logs', label: 'Follow logs', hint: 'Ctrl-C to stop' },
      { value: 'unlink', label: 'Unlink from apps/', hint: 'the project itself is untouched' },
    ],
  });
  if (action === 'unlink') return flows.flowAppUnlink(opts, name);
  if (action === 'down' && !(await confirm({ message: `Stop every container of ${name}?`, initial: false }))) return;
  return appRun(action, name, []);
}

async function certsMenu(opts) {
  printCerts(loadState());
  const action = await select({
    message: 'Certificates',
    choices: [
      { value: 'add', label: 'Add a certificate', hint: "Let's Encrypt, Cloudflare Origin CA, custom or self-signed" },
      { value: 'renew', label: 'Renew now', hint: "Let's Encrypt certificates that are due (certbot also does this every 12h)" },
      { value: 'aop', label: 'Authenticated Origin Pulls', hint: 'accept HTTPS only from Cloudflare' },
      { value: 'rm', label: 'Remove a certificate' },
    ],
  });
  if (action === 'add') return flows.flowCertAdd(opts);
  if (action === 'renew') return renew();
  if (action === 'rm') return flows.flowCertRemove(opts);
  const state = loadState();
  const name = await select({
    message: 'For which certificate?',
    choices: Object.entries(state.certs).map(([n, k]) => ({ value: n, label: n, hint: `${k.aop ? 'on' : 'off'} · ${k.names.join(', ')}` })),
  });
  return flows.flowCertAop(name, !state.certs[name].aop);
}

async function stackMenu(nginxUp) {
  const action = await select({
    message: 'Stack',
    choices: nginxUp
      ? [
          { value: 'reload', label: 'Reload nginx', hint: 'test the config, then reload without dropping connections' },
          { value: 'logs', label: 'Follow logs', hint: 'Ctrl-C to stop' },
          { value: 'restart', label: 'Restart', hint: 'nginx + certbot containers' },
          { value: 'cf-sync', label: 'Refresh Cloudflare IP ranges' },
          { value: 'down', label: 'Stop nginx-edge', hint: 'every site goes offline' },
        ]
      : [
          { value: 'up', label: 'Start nginx-edge' },
          { value: 'test', label: 'Test the configuration' },
        ],
  });
  if (action === 'down' && !(await confirm({ message: 'Stop nginx? Every site goes offline.', initial: false }))) return;
  return dispatch([action], {});
}

