// The guided flows. Each one asks only for what the flags didn't give it, so
// every flow also works non-interactively (`--yes` plus flags).

import fs from 'node:fs';
import path from 'node:path';
import { P, readEnv, writeEnv, loadState, saveState, clone, readToken, writeToken, emptyState } from './config.mjs';
import { run, networkInfo, listNetworks, createNetwork, listContainers, exposedPorts, connectNetwork, compose } from './exec.mjs';
import { commit, ensureStaticDir } from './nginx.mjs';
import * as certs from './certs.mjs';
import * as cf from './cloudflare.mjs';
import * as apps from './apps.mjs';
import {
  UsageError, parseTarget, validateHost, validatePath, normalizePath, isPort, isName,
  domainOf, depthBelow, guessDomain, describeSource, sourceFromFlags, CERT_TYPES,
} from './targets.mjs';
import { select, input, confirm, paste, multiselect, canPrompt } from './prompt.mjs';
import { c, log, sym, task, heading, table, daysLeft, shorten, strip } from './ui.mjs';

const today = () => new Date().toISOString().slice(0, 10);

/** Use the flag if given; otherwise prompt; off a terminal, fail with the flag to pass. */
async function ask(value, flag, prompt) {
  if (value !== undefined && value !== null && value !== '') return value;
  if (!canPrompt()) throw new UsageError(`missing ${flag}`);
  return prompt();
}

async function sure(opts, message, hint) {
  if (opts.yes || !canPrompt()) return true;
  return confirm({ message, initial: true, hint });
}

/** A short "here's what will happen" block before anything is changed. */
function plan(title, lines) {
  heading(title);
  for (const [k, v] of lines) console.log(`    ${c.gray(k.padEnd(12))} ${v}`);
  console.log('');
}

async function edgeNet() {
  const env = readEnv();
  const info = await networkInfo(env.EDGE_NETWORK);
  if (!info) throw new UsageError(`the Docker network "${env.EDGE_NETWORK}" does not exist — run \`edge init\``);
  return info;
}

// ═════════════════════════════════════════════════════════════ sources ═════

/** Ask where a route sends its traffic. Returns a source object for edge.json. */
export async function pickSource(opts, target) {
  const net = await edgeNet();
  const flagged = sourceFromFlags(opts, { gateway: net.gateway });
  if (flagged?.type === 'app') return appSource(opts, net, flagged);
  if (flagged?.type === 'container') return containerSource(opts, net, flagged);
  if (flagged?.type === 'static') {
    ensureStaticDir(flagged.dir, target);
    return flagged;
  }
  if (flagged?.type === 'port') return portSource(net, flagged.port);
  if (!canPrompt()) {
    throw new UsageError('say where traffic goes: --app NAME/SERVICE[:PORT], --container NAME:PORT, --port PORT or --static DIR');
  }

  const linked = apps.listApps().filter((a) => !a.broken).length;
  const type = await select({
    message: `Where should ${c.cyan(target)} send traffic?`,
    choices: [
      { value: 'app', label: 'Linked app', hint: linked ? `a service of a project in apps/ (${linked} linked)` : 'link a project’s docker compose into apps/ first' },
      { value: 'container', label: 'Running container', hint: `any container — it joins the ${net.name} network` },
      { value: 'port', label: 'Host process', hint: `a port on this server (systemd, PM2 …), reached at ${net.gateway}` },
      { value: 'static', label: 'Static files', hint: 'a folder in www/' },
    ],
  });
  if (type === 'app') return appSource(opts, net, {});
  if (type === 'container') return containerSource(opts, net, {});
  if (type === 'port') return portSource(net);
  return staticSource(target);
}

async function appSource(opts, net, preset) {
  let app;
  if (preset.app) {
    app = apps.getApp(preset.app);
    if (!app) throw new UsageError(`no app named ${preset.app} in apps/ — link it with \`edge app link\``);
  } else {
    const list = apps.listApps();
    const choice = await select({
      message: 'Which app?',
      choices: [
        ...list.map((a) => ({
          value: a.name,
          label: a.name,
          hint: shorten(a.target || ''),
          disabled: a.broken ? `broken: ${a.broken}` : false,
        })),
        { value: '+link', label: '＋ Link a project…', hint: 'symlink its docker compose into apps/' },
      ],
    });
    app = choice === '+link' ? await flowAppLink(opts, undefined, { offerRoute: false }) : apps.getApp(choice);
  }
  if (app.broken) throw new Error(`apps/${app.name} is broken: ${app.broken}`);

  const services = await task(`Reading ${app.name}'s compose file`, () => apps.appServices(app, net.name));
  let svc;
  if (preset.service) {
    svc = services.find((s) => s.name === preset.service);
    if (!svc) throw new UsageError(`${app.name} has no service ${preset.service} (has: ${services.map((s) => s.name).join(', ')})`);
  } else {
    const name = await select({
      message: 'Which service?',
      choices: services.map((s) => ({
        value: s.name,
        label: s.name,
        hint: [
          s.ports.length ? `port ${s.ports.join(', ')}` : 'no ports declared',
          s.onEdge ? `on ${net.name} ✔` : `not on ${net.name}`,
          s.oneOff ? 'one-off job' : '',
        ].filter(Boolean).join(' · '),
      })),
    });
    svc = services.find((s) => s.name === name);
  }

  const port = preset.port || (await pickPort(svc.ports, `${svc.name}`));
  let upstream = apps.upstreamFor(svc);
  if (!svc.onEdge) upstream = await attachService(opts, app, svc, net);
  return { type: 'app', app: app.name, service: svc.name, port, upstream };
}

async function pickPort(ports, what) {
  if (ports.length === 1) {
    log.hint(`${what} listens on ${ports[0]}`);
    return ports[0];
  }
  if (ports.length > 1) {
    return select({
      message: `Which port of ${what}?`,
      choices: [...ports.map((p) => ({ value: p, label: String(p) })), { value: 'other', label: 'Another port…' }],
    }).then((v) => (v === 'other' ? askPort(what) : v));
  }
  return askPort(what);
}

const askPort = async (what) =>
  Number(await input({ message: `Port ${what} listens on`, placeholder: '3000', validate: (v) => (isPort(v) ? null : 'a number from 1 to 65535') }));

/** The service is not on the nginx network: add an override, or show the lines to add. */
async function attachService(opts, app, svc, net) {
  const alias = `${app.name}-${svc.name}`;
  let how = opts.attach;
  if (!how) {
    how = canPrompt()
      ? await select({
          message: `${svc.name} is not on the ${net.name} network. How should it join?`,
          choices: [
            { value: 'override', label: 'edge override', hint: `apps/${app.name}/${apps.OVERRIDE} — the project's own compose file stays untouched` },
            { value: 'manual', label: 'I’ll edit the compose file', hint: 'shows the lines to add to the project' },
          ],
        })
      : 'override';
  }
  if (how === 'manual') {
    heading(`Add this to ${shorten(app.target)}`, 'then restart the app');
    console.log(c.cyan(apps.manualSnippet(svc, alias, net.name).replace(/^/gm, '    ')));
    console.log('');
    return alias;
  }
  apps.writeOverride(app, svc, alias, net.name);
  log.ok(`wrote ${c.bold(`apps/${app.name}/${apps.OVERRIDE}`)} ${c.gray(`(${svc.name} joins ${net.name} as ${alias})`)}`);
  const now =
    opts.yes ||
    (canPrompt() &&
      (await confirm({ message: `Recreate ${svc.name} now so it joins ${net.name}?`, initial: true, hint: `runs edge app up ${app.name} — the service restarts briefly` })));
  if (now) {
    await task(`Starting ${app.name} with the override`, async () => {
      const res = await apps.appCompose(app, ['up', '-d', '--remove-orphans', svc.name]);
      if (res.code !== 0) throw new Error(res.stderr.trim().split('\n').slice(-6).join('\n'));
    });
  } else {
    log.hint(`start it later with ${c.bold(`edge app up ${app.name}`)} — until then the route answers 502`);
  }
  return alias;
}

async function containerSource(opts, net, preset) {
  let name = preset.name;
  let port = preset.port;
  const all = await listContainers();
  if (!name) {
    const list = all.filter((k) => k.project !== 'edge' && !k.name.startsWith('edge-'));
    if (!list.length) throw new UsageError('no running containers');
    name = await select({
      message: 'Which container?',
      choices: list.map((k) => ({
        value: k.name,
        label: k.name,
        hint: `${k.image} · ${k.networks.includes(net.name) ? `on ${net.name} ✔` : k.networks.join(', ')}`,
      })),
    });
  }
  const found = all.find((k) => k.name === name);
  if (!port) port = await pickPort(await exposedPorts(name), name);
  if (found && !found.networks.includes(net.name)) {
    const join =
      opts.yes ||
      (canPrompt() &&
        (await confirm({
          message: `Connect ${name} to the ${net.name} network now?`,
          initial: true,
          hint: 'lasts until the container is recreated — link its compose as an app to make it permanent',
        })));
    if (join) await task(`Connecting ${name} to ${net.name}`, () => connectNetwork(net.name, name));
    else log.warn(`${name} is not on ${net.name} — nginx can't reach it until it is`);
  } else if (!found) {
    log.warn(`${name} is not running right now — the route answers 502 until it is`);
  }
  return { type: 'container', name, port };
}

async function portSource(net, given) {
  const port = given || (await askPort('the process'));
  const res = await run('ss', ['-ltnH']);
  const listening = res.stdout
    .split('\n')
    .map((l) => l.trim().split(/\s+/)[3] || '')
    .filter((a) => a.endsWith(`:${port}`));
  const reachable = listening.some((a) => a.startsWith('0.0.0.0:') || a.startsWith('*:') || a.startsWith('[::]:') || a.startsWith(`${net.gateway}:`));
  if (!listening.length) log.warn(`nothing listens on port ${port} yet`);
  else if (!reachable) log.warn(`port ${port} listens on ${listening.join(', ')} — nginx needs ${net.gateway} or 0.0.0.0`);
  log.hint(`firewall: ${c.bold(`ufw allow in on ${net.bridge} to any port ${port} proto tcp`)}`);
  return { type: 'port', port, ip: net.gateway };
}

async function staticSource(target) {
  const dirs = fs.existsSync(P.www)
    ? fs.readdirSync(P.www, { withFileTypes: true }).filter((d) => d.isDirectory() && !d.name.startsWith('.')).map((d) => d.name)
    : [];
  let dir = await select({
    message: 'Which folder in www/?',
    choices: [
      ...dirs.map((d) => ({ value: d, label: d, hint: `${fs.readdirSync(path.join(P.www, d)).length} entries` })),
      { value: '+new', label: '＋ New folder…', hint: 'creates www/<name> with a placeholder page' },
    ],
  });
  if (dir === '+new') {
    const suggestion = target.replace(/\//g, '-');
    dir = await input({
      message: 'Folder name',
      defaultValue: suggestion,
      validate: (v) => (isName(v) ? (dirs.includes(v) ? 'exists — pick it from the list' : null) : 'letters, digits, . _ - only'),
    });
  }
  ensureStaticDir(dir, target);
  return { type: 'static', dir };
}

// ═══════════════════════════════════════════════════════ DNS & certs ═════

async function zoneFor(state, host, token) {
  const domain = domainOf(host, Object.keys(state.domains));
  if (domain && state.domains[domain].zone) return state.domains[domain].zone;
  if (!token) return null;
  return task(`Finding the Cloudflare zone for ${host}`, () => cf.findZone(token, host)).catch((err) => {
    log.warn(`Cloudflare: ${err.message}`);
    return null;
  });
}

async function pickDns(opts, { host, zone, token }) {
  const modes = ['proxied', 'dns-only', 'skip'];
  if (opts.dns) {
    if (!modes.includes(opts.dns)) throw new UsageError(`--dns must be one of ${modes.join(', ')}`);
    if (opts.dns !== 'skip' && (!token || !zone)) throw new UsageError(`--dns ${opts.dns} needs a Cloudflare token and zone for ${host}`);
    return opts.dns;
  }
  if (!canPrompt()) return token && zone ? 'proxied' : 'skip';
  const why = !token ? 'no Cloudflare token — run edge init' : !zone ? `${guessDomain(host)} is not a zone in this Cloudflare account` : false;
  return select({
    message: `Cloudflare DNS for ${c.cyan(host)}`,
    choices: [
      { value: 'proxied', label: 'Proxied', hint: 'orange cloud — visitors go through Cloudflare (recommended)', disabled: why },
      { value: 'dns-only', label: 'DNS only', hint: 'grey cloud — visitors connect straight to this server', disabled: why },
      { value: 'skip', label: 'Leave DNS alone', hint: 'I manage this record myself' },
    ],
  });
}

async function applyDns(host, mode, zone, token) {
  if (mode === 'skip' || !zone || !token) return;
  const ip = readEnv().SERVER_IP;
  if (!ip) {
    log.warn(`SERVER_IP is not set in .env — add the DNS record for ${host} yourself (or run edge init)`);
    return;
  }
  await task(`DNS ${host} → ${ip} (${mode})`, async (t) => {
    const r = await cf.upsertA(token, zone.id, host, ip, mode === 'proxied');
    t.update(`DNS ${host} → ${ip} (${mode}) ${c.gray(r)}`);
  }).catch((err) => log.warn(`DNS not changed: ${err.message}`));
}

/**
 * Which certificate a host uses. Returns { use: name } for an existing one, or
 * { create: type, name, names } for a new one.
 */
async function pickCert(opts, state, { host, dns, zone, token, domainWide = false }) {
  const covering = certs.certsFor(state, host);
  const wildcardNames = [host, `*.${host}`];
  const kinds = ['letsencrypt', 'origin', 'custom', 'self-signed'];

  if (opts.cert) {
    if (state.certs[opts.cert]) return { use: opts.cert };
    if (opts.cert === 'auto') {
      if (covering[0]) return { use: covering[0].name };
      throw new UsageError(`no certificate covers ${host} yet`);
    }
    if (!kinds.includes(opts.cert)) throw new UsageError(`--cert must be an existing certificate or one of: auto, ${kinds.join(', ')}`);
    const names = domainWide || opts.wildcard ? wildcardNames : [host];
    return { create: opts.cert, name: certs.freeCertName(state, host), names };
  }
  if (!canPrompt()) {
    if (covering[0]) return { use: covering[0].name };
    throw new UsageError(`no certificate covers ${host} — pass --cert letsencrypt|origin|custom|self-signed`);
  }

  const noToken = !token && 'needs a Cloudflare token (edge init)';
  const noZone = !zone && `${guessDomain(host)} must be a zone in your Cloudflare account`;
  const choices = covering.map((k) => ({
    value: `use:${k.name}`,
    label: `Use ${k.name}`,
    answer: k.name,
    hint: `${CERT_TYPES[k.type] || k.type} · ${k.names.join(', ')} · ${strip(daysLeft(k.info?.validTo))}`,
  }));
  if (choices.length) choices.push({ separator: `or create a new certificate for ${host}` });
  choices.push(
    domainWide
      ? { value: 'letsencrypt', label: "Let's Encrypt wildcard", hint: `${host} + *.${host} via certbot — free, renews itself (recommended)`, disabled: noToken || noZone }
      : { value: 'letsencrypt', label: "Let's Encrypt", hint: `${host} via certbot + Cloudflare DNS — free, renews itself`, disabled: noToken || noZone },
    {
      value: 'origin',
      label: 'Cloudflare Origin CA',
      hint: `15-year ${domainWide ? 'wildcard ' : ''}certificate, trusted only by Cloudflare`,
      disabled: dns === 'dns-only' ? 'browsers don’t trust it — it needs the orange cloud' : noToken,
    },
    { value: 'custom', label: 'Custom certificate', hint: 'import a fullchain + private key you already have' },
    { value: 'self-signed', label: 'Self-signed', hint: 'testing only — Cloudflare Full (strict) rejects it' },
  );
  const pick = await select({ message: `HTTPS certificate for ${c.cyan(host)}`, choices });
  if (pick.startsWith('use:')) return { use: pick.slice(4) };
  const names = domainWide ? wildcardNames : [host];
  return { create: pick, name: certs.freeCertName(state, domainWide ? host : host), names };
}

/** Produce the certificate files for a { create } choice. Returns the edge.json entry. */
async function obtainCert(opts, choice, token) {
  const { create: type, name, names } = choice;
  const label = names.join(', ');
  if (type === 'letsencrypt') {
    return task(`Requesting Let's Encrypt certificate for ${label} (DNS challenge, ~1 min)`, () => certs.issueLetsEncrypt(name, names));
  }
  if (type === 'origin') {
    return task(`Creating Cloudflare Origin CA certificate for ${label}`, () => certs.originCert(name, names, token));
  }
  if (type === 'self-signed') {
    return task(`Creating self-signed certificate for ${label}`, () => certs.selfSigned(name, names));
  }
  // custom: from files or pasted
  let certPem;
  let keyPem;
  if (opts['cert-file'] && opts['key-file']) {
    certPem = fs.readFileSync(opts['cert-file'], 'utf8');
    keyPem = fs.readFileSync(opts['key-file'], 'utf8');
  } else {
    if (!canPrompt()) throw new UsageError('a custom certificate needs --cert-file and --key-file');
    const how = await select({
      message: 'Where is the certificate?',
      choices: [
        { value: 'files', label: 'Files on this server', hint: 'paths to the fullchain and private key PEM files' },
        { value: 'paste', label: 'Paste them here', hint: 'PEM text, e.g. copied from the Cloudflare dashboard' },
      ],
    });
    const exists = (v) => (fs.existsSync(v) ? null : 'no such file');
    if (how === 'files') {
      certPem = fs.readFileSync(await input({ message: 'Certificate (fullchain) file', placeholder: '/path/fullchain.pem', validate: exists }), 'utf8');
      keyPem = fs.readFileSync(await input({ message: 'Private key file', placeholder: '/path/privkey.pem', validate: exists }), 'utf8');
    } else {
      certPem = await paste({ message: 'Paste the certificate (fullchain PEM)' });
      keyPem = await paste({ message: 'Paste the private key (PEM)' });
    }
  }
  const entry = await task('Checking and storing the certificate', () => certs.importCustom(name, certPem, keyPem));
  const missing = names.filter((n) => !entry.names.some((have) => have === n || (have.startsWith('*.') && n.endsWith(have.slice(1)) && !n.slice(0, -have.length + 1).includes('.'))));
  if (missing.length) log.warn(`the certificate is for ${entry.names.join(', ')} — it does not cover ${missing.join(', ')}`);
  return entry;
}

// ═════════════════════════════════════════════════════════════ domain ═════

export async function flowDomain(opts, arg) {
  const state = loadState();
  const token = readToken();
  const domain = (
    await ask(arg, 'the domain (edge domain add example.com)', () =>
      input({
        message: 'Domain',
        placeholder: 'example.com',
        hint: 'the zone itself — subdomains are added afterwards with “Add a subdomain”',
        validate: (v) => validateHost(v.toLowerCase()) || (state.domains[v.toLowerCase()] ? 'already set up' : null),
      }),
    )
  ).toLowerCase();
  if (validateHost(domain)) throw new UsageError(`invalid domain: ${domain}`);
  if (state.domains[domain]) throw new UsageError(`${domain} is already set up — see \`edge status\``);

  const zone = await zoneFor(state, domain, token);
  if (token && !zone) log.warn(`${domain} is not in this Cloudflare account — certbot and DNS records won't work for it`);

  const certChoice = await pickCert(opts, state, { host: domain, dns: 'proxied', zone, token, domainWide: true });

  let apex = opts.apex;
  if (!apex && (opts.app || opts.container || opts.port || opts.static)) apex = 'source';
  if (!apex) {
    apex = canPrompt()
      ? await select({
          message: `What should ${c.cyan(domain)} itself show?`,
          choices: [
            { value: 'placeholder', label: 'Placeholder page', hint: `www/${domain}/ — replace it with your site later` },
            { value: 'source', label: 'A project', hint: 'linked app, container, host port or static folder' },
            { value: 'none', label: 'Nothing yet', hint: 'only subdomains for now' },
          ],
        })
      : 'placeholder';
  }
  const source = apex === 'source' ? await pickSource(opts, domain) : apex === 'placeholder' ? { type: 'static', dir: domain } : null;
  const dns = source ? await pickDns(opts, { host: domain, zone, token }) : 'skip';

  plan(`Set up ${domain}`, [
    ['certificate', certChoice.use ? `use ${certChoice.use}` : `${CERT_TYPES[certChoice.create]} for ${certChoice.names.join(', ')}`],
    ['serves', source ? describeSource(source) : c.gray('nothing yet')],
    ['dns', source && dns !== 'skip' ? `${domain}, www.${domain} → ${readEnv().SERVER_IP || '?'} (${dns})` : c.gray('unchanged')],
  ]);
  if (!(await sure(opts, 'Go ahead?'))) return;

  const next = clone(state);
  const certName = certChoice.use || certChoice.name;
  if (certChoice.create) next.certs[certName] = await obtainCert(opts, certChoice, token);
  next.domains[domain] = { cert: certName, zone: zone || null, added: today() };
  if (source) {
    if (source.type === 'static') ensureStaticDir(source.dir, domain);
    next.sites[domain] = { cert: certName, source, dns, enabled: true, added: today() };
  }
  await commitOrCleanup(next, certChoice);
  if (source) {
    await applyDns(domain, dns, zone, token);
    await applyDns(`www.${domain}`, dns, zone, token);
  }
  done(source ? `https://${domain}` : domain, source ? describeSource(source) : 'ready for subdomains');
}

async function commitOrCleanup(next, certChoice) {
  try {
    await commit(next);
  } catch (err) {
    if (certChoice?.create) await certs.removeCertFiles(certChoice.name, next.certs[certChoice.name]).catch(() => {});
    throw err;
  }
}

function done(what, detail) {
  console.log('');
  console.log(`  ${c.green(sym.ok)} ${c.bold(what)} ${c.gray('→')} ${detail}`);
  console.log('');
}

// ═══════════════════════════════════════════════════════════════ site ═════

export async function flowSite(opts, arg) {
  const state = loadState();
  const token = readToken();
  let host = await ask(arg, 'the hostname (edge site add api.example.com)', () =>
    input({
      message: 'Subdomain',
      placeholder: 'api.example.com',
      hint: 'the full hostname',
      validate: (v) => validateHost(v.toLowerCase()),
    }),
  );
  const t = parseTarget(host);
  if (t.path) throw new UsageError(`${host} has a path — use \`edge path add\``);
  host = t.host;

  if (state.sites[host] && !opts.force) {
    const current = describeSource(state.sites[host].source);
    if (!canPrompt()) throw new UsageError(`${host} already serves ${current} — add --force to replace it`);
    if (!(await confirm({ message: `${host} already serves ${current}. Replace it?`, initial: false }))) return;
  }

  const domain = domainOf(host, Object.keys(state.domains));
  if (domain && depthBelow(host, domain) > 1) {
    log.warn(`${host} is two levels below ${domain}: wildcard certificates and Cloudflare's free edge certificate don't cover it`);
  }
  const zone = await zoneFor(state, host, token);
  const dns = await pickDns(opts, { host, zone, token });
  const certChoice = await pickCert(opts, state, { host, dns, zone, token });
  const source = await pickSource(opts, host);

  plan(`Serve ${host}`, [
    ['traffic', describeSource(source)],
    ['certificate', certChoice.use ? `use ${certChoice.use}` : `new ${CERT_TYPES[certChoice.create]} for ${certChoice.names.join(', ')}`],
    ['dns', dns === 'skip' ? c.gray('unchanged') : `${host} → ${readEnv().SERVER_IP || '?'} (${dns})`],
  ]);
  if (!(await sure(opts, 'Go ahead?'))) return;

  const next = clone(state);
  const certName = certChoice.use || certChoice.name;
  if (certChoice.create) next.certs[certName] = await obtainCert(opts, certChoice, token);
  const keepPaths = state.sites[host]?.added;
  next.sites[host] = { cert: certName, source, dns, enabled: true, added: keepPaths || today() };
  await commitOrCleanup(next, certChoice);
  await applyDns(host, dns, zone, token);
  done(`https://${host}`, describeSource(source));
}

// ═══════════════════════════════════════════════════════════════ path ═════

export async function flowPath(opts, arg) {
  const state = loadState();
  const hosts = Object.keys(state.sites).filter((h) => state.sites[h].enabled !== false);
  if (!hosts.length) throw new UsageError('there are no hosts yet — add a domain or subdomain first');

  let host;
  let routePath;
  if (arg) {
    const t = parseTarget(arg);
    host = t.host;
    routePath = t.path || null;
  }
  host = await ask(host, 'the target (edge path add example.com/admin)', () =>
    select({
      message: 'On which host?',
      choices: hosts.map((h) => ({ value: h, label: h, hint: describeSource(state.sites[h].source) })),
    }),
  );
  if (!state.sites[host]) throw new UsageError(`${host} is not served yet — add it first (edge site add ${host})`);

  routePath = normalizePath(
    await ask(routePath, 'a path (example.com/admin)', () =>
      input({
        message: 'Path',
        placeholder: '/admin',
        validate: (v) => validatePath(v) || (state.paths[host + normalizePath(v)] && !opts.force ? 'already used — manage it from the routes list' : null),
      }),
    ),
  );
  const pathError = validatePath(routePath);
  if (pathError) throw new UsageError(pathError);
  const key = host + routePath;
  if (state.paths[key] && !opts.force) throw new UsageError(`${key} already exists — add --force to replace it`);

  const source = await pickSource(opts, key);
  let strip = false;
  if (source.type !== 'static') {
    strip =
      opts.strip ??
      (canPrompt()
        ? await select({
            message: `What path does the app see for ${routePath}/users?`,
            choices: [
              { value: false, label: `${routePath}/users`, answer: 'keep the prefix', hint: `the app is built with base path ${routePath} (recommended)` },
              { value: true, label: '/users', answer: 'strip the prefix', hint: 'prefix removed — only for APIs whose responses have no absolute links' },
            ],
          })
        : false);
    if (!strip) log.hint(`the app must use base path ${routePath}: Vite base, Next.js basePath, React Router basename, FastAPI root_path …`);
  }

  plan(`Serve ${key}`, [
    ['traffic', describeSource(source) + (strip ? ' (prefix stripped)' : '')],
    ['certificate', `${state.sites[host].cert} (from ${host})`],
  ]);
  if (!(await sure(opts, 'Go ahead?'))) return;

  const next = clone(state);
  next.paths[key] = { host, path: routePath, source, strip, enabled: true, added: today() };
  await commit(next);
  done(`https://${key}/`, describeSource(source));
}

// ══════════════════════════════════════════════════════════════ routes ═════

export function routeList(state) {
  const out = [];
  for (const [host, s] of Object.entries(state.sites)) {
    out.push({ key: host, kind: 'site', ...s });
    for (const [key, p] of Object.entries(state.paths)) if (p.host === host) out.push({ key, kind: 'path', ...p });
  }
  for (const [key, p] of Object.entries(state.paths)) if (!state.sites[p.host]) out.push({ key, kind: 'path', orphan: true, ...p });
  return out;
}

export async function removeRoute(opts, target) {
  const state = loadState();
  const t = parseTarget(target);
  const next = clone(state);
  if (t.path) {
    if (!next.paths[t.key]) throw new UsageError(`nothing is served at ${t.key}`);
    delete next.paths[t.key];
  } else {
    const site = next.sites[t.host];
    if (!site) throw new UsageError(`${t.host} is not served`);
    const children = Object.keys(next.paths).filter((k) => next.paths[k].host === t.host);
    if (children.length) {
      const ok =
        opts.force ||
        (canPrompt() && (await confirm({ message: `${t.host} has ${children.length} path route(s): ${children.join(', ')}. Remove them too?`, initial: false })));
      if (!ok) throw new UsageError(`${t.host} still has path routes — remove them first or add --force`);
      for (const k of children) delete next.paths[k];
    }
    delete next.sites[t.host];
    await commit(next);
    const token = readToken();
    const zone = site.dns && site.dns !== 'skip' ? await zoneFor(state, t.host, token) : null;
    if (zone && token) {
      const drop =
        opts['purge-dns'] ||
        (canPrompt() && !opts.yes && (await confirm({ message: `Also delete the DNS record for ${t.host}?`, initial: false })));
      if (drop) await task(`Deleting DNS record ${t.host}`, () => cf.deleteRecords(token, zone.id, t.host)).catch((e) => log.warn(e.message));
    }
    log.ok(`${t.host} removed`);
    return;
  }
  await commit(next);
  log.ok(`${t.key} removed`);
}

export async function toggleRoute(target, enabled) {
  const state = loadState();
  const t = parseTarget(target);
  const next = clone(state);
  const entry = t.path ? next.paths[t.key] : next.sites[t.host];
  if (!entry) throw new UsageError(`nothing is served at ${t.key}`);
  entry.enabled = enabled;
  await commit(next);
  log.ok(`${t.key} ${enabled ? 'enabled' : 'disabled'}`);
}

// ════════════════════════════════════════════════════════════════ apps ═════

export async function flowAppLink(opts, arg, { offerRoute = true } = {}) {
  const input1 = await ask(arg, 'a project folder or compose file (edge app link ~/my-project)', () =>
    input({
      message: 'Project folder or compose file',
      placeholder: '/home/you/my-project',
      validate: (v) => {
        try {
          apps.findComposeFile(v.replace(/^~/, process.env.HOME || ''));
          return null;
        } catch (err) {
          return err.message;
        }
      },
    }),
  );
  const file = apps.findComposeFile(String(input1).replace(/^~/, process.env.HOME || ''));
  const existing = apps.listApps().find((a) => a.target === fs.realpathSync(file));
  if (existing) {
    log.info(`already linked as ${c.bold(`apps/${existing.name}`)}`);
    return existing;
  }
  const taken = new Set(apps.listApps().map((a) => a.name));
  const name = await ask(opts.name, '--name', () =>
    input({
      message: 'Name in apps/',
      defaultValue: apps.appNameFor(file),
      validate: (v) => (!isName(v) ? 'letters, digits, . _ - only' : taken.has(v) ? 'taken' : null),
    }),
  );
  const { app } = apps.linkApp(file, name);
  log.ok(`linked ${c.bold(`apps/${app.name}/${path.basename(app.target)}`)} ${c.gray(`→ ${shorten(app.target)}`)}`);

  if (offerRoute && canPrompt() && !opts.yes) {
    const next = await select({
      message: `Serve ${app.name} now?`,
      choices: [
        { value: 'site', label: 'On a subdomain', hint: 'app.example.com' },
        { value: 'path', label: 'On a path', hint: 'example.com/app' },
        { value: 'no', label: 'Not now' },
      ],
    });
    if (next === 'site') await flowSite({ ...opts, app: `${app.name}/` + (await pickServiceName(app)) }, undefined);
    if (next === 'path') await flowPath({ ...opts, app: `${app.name}/` + (await pickServiceName(app)) }, undefined);
  }
  return app;
}

async function pickServiceName(app) {
  const net = await edgeNet();
  const services = await task(`Reading ${app.name}'s compose file`, () => apps.appServices(app, net.name));
  if (services.filter((s) => !s.oneOff).length === 1) return services.find((s) => !s.oneOff).name;
  return select({
    message: 'Which service?',
    choices: services.map((s) => ({ value: s.name, label: s.name, hint: s.ports.length ? `port ${s.ports.join(', ')}` : 'no ports' })),
  });
}

export async function flowAppScan(opts, roots) {
  const dirs = roots.length ? roots : fs.readdirSync('/home').map((u) => path.join('/home', u));
  const found = await task(`Looking for compose files in ${dirs.map((d) => shorten(d)).join(', ')}`, async () => apps.scanForProjects(dirs));
  if (!found.length) {
    log.info('no compose files found');
    return;
  }
  const linked = new Map(apps.listApps().map((a) => [a.target, a.name]));
  const choices = found.map((f) => {
    const real = fs.realpathSync(f);
    return {
      value: f,
      label: apps.appNameFor(f),
      hint: shorten(f),
      badge: linked.has(real) ? `linked` : '',
      disabled: linked.has(real) ? `already apps/${linked.get(real)}` : false,
    };
  });
  const pick = opts.all
    ? choices.filter((ch) => !ch.disabled).map((ch) => ch.value)
    : await ask(null, '--all (or run in a terminal to pick)', () =>
        multiselect({ message: 'Which projects should be linked into apps/?', choices }),
      );
  if (!pick.length) return;
  const taken = new Set(apps.listApps().map((a) => a.name));
  for (const f of pick) {
    let name = apps.appNameFor(f);
    for (let i = 2; taken.has(name); i++) name = `${apps.appNameFor(f)}-${i}`;
    taken.add(name);
    try {
      const { app } = apps.linkApp(f, name);
      log.ok(`apps/${c.bold(app.name)} ${c.gray(`→ ${shorten(app.target)}`)}`);
    } catch (err) {
      log.err(`${shorten(f)}: ${err.message}`);
    }
  }
}

export async function flowAppUnlink(opts, name) {
  const state = loadState();
  name = await ask(name, 'the app name', () =>
    select({ message: 'Unlink which app?', choices: apps.listApps().map((a) => ({ value: a.name, label: a.name, hint: shorten(a.target || a.broken) })) }),
  );
  const users = routeList(state).filter((r) => r.source.type === 'app' && r.source.app === name);
  if (users.length && !opts.force) {
    throw new UsageError(`${name} is still served at ${users.map((r) => r.key).join(', ')} — remove those routes first or add --force`);
  }
  if (!(await sure(opts, `Unlink apps/${name}?`, 'removes the symlink only — the project and its containers are untouched'))) return;
  apps.unlinkApp(name);
  log.ok(`apps/${name} unlinked`);
}

// ═══════════════════════════════════════════════════════════════ certs ═════

export async function flowCertAdd(opts, arg) {
  const state = loadState();
  const token = readToken();
  const namesRaw = await ask(arg, 'the hostnames (edge cert add example.com,*.example.com)', () =>
    input({
      message: 'Hostnames',
      placeholder: 'example.com, *.example.com',
      validate: (v) =>
        v.split(/[\s,]+/).filter(Boolean).every((h) => !validateHost(h.replace(/^\*\./, ''))) ? null : 'comma-separated hostnames; *.domain for a wildcard',
    }),
  );
  const names = String(namesRaw).split(/[\s,]+/).filter(Boolean).map((h) => h.toLowerCase());
  const main = names[0].replace(/^\*\./, '');
  const zone = await zoneFor(state, main, token);
  const type = await ask(opts.cert, '--cert letsencrypt|origin|custom|self-signed', () =>
    select({
      message: 'Kind of certificate',
      choices: [
        { value: 'letsencrypt', label: "Let's Encrypt", hint: 'certbot + Cloudflare DNS, renews itself', disabled: (!token && 'needs a Cloudflare token') || (!zone && 'needs the domain in your Cloudflare account') },
        { value: 'origin', label: 'Cloudflare Origin CA', hint: '15 years, proxied hosts only', disabled: !token && 'needs a Cloudflare token' },
        { value: 'custom', label: 'Custom certificate', hint: 'import PEM files' },
        { value: 'self-signed', label: 'Self-signed', hint: 'testing only' },
      ],
    }),
  );
  const name = opts.name || certs.freeCertName(state, main);
  const next = clone(state);
  next.certs[name] = await obtainCert(opts, { create: type, name, names }, token);
  await commit(next, { quiet: true });
  log.ok(`certificate ${c.bold(name)} ready ${c.gray(`(${next.certs[name].names.join(', ')})`)}`);
}

export async function flowCertRemove(opts, name) {
  const state = loadState();
  name = await ask(name, 'the certificate name', () =>
    select({ message: 'Remove which certificate?', choices: Object.keys(state.certs).map((n) => ({ value: n, label: n, hint: state.certs[n].names.join(', ') })) }),
  );
  const cert = state.certs[name];
  if (!cert) throw new UsageError(`no certificate named ${name}`);
  const users = [
    ...Object.entries(state.sites).filter(([, s]) => s.cert === name).map(([h]) => h),
    ...Object.entries(state.domains).filter(([, d]) => d.cert === name).map(([d]) => `domain ${d}`),
  ];
  if (users.length) throw new UsageError(`${name} is used by ${users.join(', ')}`);
  if (!(await sure(opts, `Delete certificate ${name}?`, 'its files are deleted too'))) return;
  const next = clone(state);
  delete next.certs[name];
  await commit(next, { quiet: true });
  await certs.removeCertFiles(name, cert);
  log.ok(`certificate ${name} removed`);
}

export async function flowCertAop(name, on) {
  const state = loadState();
  if (!state.certs[name]) throw new UsageError(`no certificate named ${name}`);
  if (on && !fs.existsSync(P.aopCa)) await task('Downloading the Cloudflare origin-pull CA', () => cf.syncCloudflareFiles());
  if (on) log.warn('Authenticated Origin Pulls must be ON in Cloudflare (SSL/TLS → Origin Server) first, or every request fails');
  const next = clone(state);
  next.certs[name].aop = on;
  await commit(next);
  log.ok(`Authenticated Origin Pulls ${on ? 'on' : 'off'} for hosts using ${name}`);
}

// ════════════════════════════════════════════════════════════════ init ═════

export async function flowInit(opts) {
  heading('nginx-edge setup', shorten(P.root));
  console.log('');
  const version = await run('docker', ['compose', 'version', '--short']);
  if (version.code !== 0) throw new Error('docker compose is not available — install Docker first');

  const env = readEnv();
  if (!fs.existsSync(P.env)) writeEnv({});

  const email = await ask(opts.email, '--email', () =>
    input({
      message: "Email for Let's Encrypt",
      defaultValue: env.ACME_EMAIL,
      placeholder: env.ACME_EMAIL || 'you@example.com',
      hint: 'expiry warnings go here',
      validate: (v) => (/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(v) ? null : 'not an email address'),
    }),
  );

  // the network nginx shares with the apps
  let network = opts.network;
  if (!network) {
    const nets = await listNetworks();
    network = canPrompt()
      ? await select({
          message: 'Docker network nginx shares with your apps',
          initial: env.EDGE_NETWORK,
          choices: [
            ...nets.map((n) => ({
              value: n.name,
              label: n.name,
              hint: `${n.subnet} · ${n.containers.length} container${n.containers.length === 1 ? '' : 's'}${n.compose ? ` · created by compose project ${n.compose}` : ''}`,
            })),
            { value: '+new', label: '＋ Create “edge”', hint: '172.30.0.0/24, interface br-edge', disabled: nets.some((n) => n.name === 'edge') && 'already exists' },
          ],
        })
      : env.EDGE_NETWORK;
    if (network === '+new') network = 'edge';
  }
  if (!(await networkInfo(network))) {
    await task(`Creating Docker network ${network}`, () => createNetwork(network, opts.subnet || (network === 'edge' ? '172.30.0.0/24' : undefined)));
  }
  const net = await networkInfo(network);

  // Cloudflare token
  let token = opts.token || readToken();
  if (token && !opts.token && canPrompt()) {
    const keep = await confirm({ message: 'Keep the Cloudflare token already in secrets/cloudflare.ini?', initial: true });
    if (!keep) token = null;
  }
  while (!token && canPrompt() && !opts['no-token']) {
    const entered = await input({
      message: 'Cloudflare API token',
      mask: true,
      optional: true,
      hint: 'Zone → DNS → Edit (+ SSL and Certificates → Edit for Origin CA). Enter to skip.',
    });
    if (!entered) break;
    try {
      await task('Verifying the token', () => cf.verifyToken(entered));
      token = entered;
    } catch (err) {
      log.err(`Cloudflare says: ${err.message}`);
    }
  }
  if (opts.token) await task('Verifying the token', () => cf.verifyToken(opts.token));
  writeToken(token);

  // public IP for DNS records
  let ip = opts['server-ip'];
  if (!ip) {
    const detected = env.SERVER_IP || (await task('Detecting this server’s public IP', async () => (await cf.publicIp()) || ''));
    ip = canPrompt()
      ? await input({
          message: 'Public IP for DNS records',
          defaultValue: detected,
          optional: true,
          validate: (v) => (/^\d{1,3}(\.\d{1,3}){3}$/.test(v) ? null : 'an IPv4 address'),
        })
      : detected;
  }

  writeEnv({ ACME_EMAIL: email, EDGE_NETWORK: network, SERVER_IP: ip || '' });
  for (const d of [P.letsencrypt, P.certs, P.apps, P.www, P.sslSnippets, P.locations]) fs.mkdirSync(d, { recursive: true });
  fs.chmodSync(P.secrets, 0o700);

  await task('Fetching Cloudflare IP ranges', async (t) => {
    const r = await cf.syncCloudflareFiles();
    t.update(`Fetched Cloudflare IP ranges ${c.gray(`(${r.ranges} ranges)`)}`);
  }).catch((err) => log.warn(`could not fetch Cloudflare IPs: ${err.message}`));

  if (!fs.existsSync(P.state)) saveState(emptyState());
  await commit(loadState(), { quiet: true });

  heading('Ready');
  console.log(
    table([
      [c.gray('network'), `${net.name}  ${c.gray(`${net.subnet} · host apps listen on ${net.gateway} · interface ${net.bridge}`)}`],
      [c.gray('cloudflare'), token ? c.green('token verified') : c.yellow('no token — certbot and DNS records are off')],
      [c.gray('public ip'), ip || c.yellow('unknown — DNS records are off')],
    ]).join('\n'),
  );
  console.log('');
  const start = opts.yes || (canPrompt() && (await confirm({ message: 'Start nginx-edge now?', initial: true })));
  if (start) {
    const up = await task('Starting nginx and certbot', () => compose(['up', '-d', '--quiet-pull']));
    if (up.code !== 0) log.err(up.stderr.trim().split('\n').slice(-4).join('\n'));
  }
  console.log('');
  log.hint(`next: ${c.bold('edge domain add example.com')}, then ${c.bold('edge site add api.example.com')}`);
}
