#!/usr/bin/env node
// Zero-dependency tests for the non-interactive half of the CLI.
// The prompts are exercised by hand; everything below is pure logic plus
// file generation against a throwaway copy of the templates.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const REAL_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const SANDBOX = fs.mkdtempSync(path.join(os.tmpdir(), 'edge-test-'));
fs.cpSync(path.join(REAL_ROOT, 'templates'), path.join(SANDBOX, 'templates'), { recursive: true });
fs.mkdirSync(path.join(SANDBOX, 'conf', 'sites'), { recursive: true });
process.env.EDGE_ROOT = SANDBOX;

const { parseArgs } = await import('../src/args.mjs');
const T = await import('../src/targets.mjs');
const { parseEnv, setEnvText } = await import('../src/config.mjs');
const { render, buildFiles, writeFiles, readManaged, MARKER } = await import('../src/nginx.mjs');
const { summarizeServices, upstreamFor, overrideYaml, scanForProjects, findComposeFile, appNameFor } = await import('../src/apps.mjs');
const { decode } = await import('../src/prompt.mjs');
const { planRemoval, cascades } = await import('../src/remove.mjs');
const { challengeOf, certLabel } = await import('../src/certs.mjs');

let passed = 0;
let failed = 0;

async function test(name, fn) {
  try {
    await fn();
    passed++;
    console.log(`  ok   ${name}`);
  } catch (err) {
    failed++;
    console.log(`  FAIL ${name}\n       ${err.message.split('\n').join('\n       ')}`);
  }
}

// ── args ──────────────────────────────────────────────────────────────────────

await test('parses commands, values and booleans', () => {
  const o = parseArgs(['site', 'add', 'api.example.com', '--app', 'shop/web:3000', '--dns=proxied', '-y']);
  assert.deepEqual(o._, ['site', 'add', 'api.example.com']);
  assert.equal(o.app, 'shop/web:3000');
  assert.equal(o.dns, 'proxied');
  assert.equal(o.yes, true);
});

await test('--no-<flag> sets a boolean false; unset stays undefined', () => {
  assert.equal(parseArgs(['--no-strip']).strip, false);
  assert.equal(parseArgs([]).strip, undefined);
  assert.equal(parseArgs(['--no-color'])['no-color'], true);
});

await test('rejects unknown options and missing values', () => {
  assert.throws(() => parseArgs(['--bogus']), /unknown option/);
  assert.throws(() => parseArgs(['--app']), /needs a value/);
});

// ── targets ───────────────────────────────────────────────────────────────────

await test('parseTarget normalises scheme, case and slashes', () => {
  assert.deepEqual(T.parseTarget('https://Example.com/admin/'), { host: 'example.com', path: '/admin', key: 'example.com/admin' });
  assert.deepEqual(T.parseTarget('api.example.com'), { host: 'api.example.com', path: '', key: 'api.example.com' });
  assert.throws(() => T.parseTarget('bad host'), /invalid hostname/);
  assert.throws(() => T.parseTarget('example.com/a b'), /invalid path/);
});

await test('wildcards cover exactly one level', () => {
  assert.ok(T.nameCovers('*.example.com', 'api.example.com'));
  assert.ok(!T.nameCovers('*.example.com', 'example.com'));
  assert.ok(!T.nameCovers('*.example.com', 'a.b.example.com'));
  assert.ok(T.certCovers(['example.com', '*.example.com'], 'example.com'));
});

await test('domainOf picks the longest configured suffix', () => {
  assert.equal(T.domainOf('a.shop.example.com', ['example.com', 'shop.example.com']), 'shop.example.com');
  assert.equal(T.domainOf('other.org', ['example.com']), null);
  assert.equal(T.depthBelow('a.b.example.com', 'example.com'), 2);
});

await test('sourceFromFlags builds each source type', () => {
  assert.deepEqual(T.sourceFromFlags({ app: 'shop/web:3000' }), { type: 'app', app: 'shop', service: 'web', port: 3000 });
  assert.deepEqual(T.sourceFromFlags({ app: 'shop/web' }), { type: 'app', app: 'shop', service: 'web', port: null });
  assert.deepEqual(T.sourceFromFlags({ container: 'my-app:3000' }), { type: 'container', name: 'my-app', port: 3000 });
  assert.deepEqual(T.sourceFromFlags({ port: '3001' }, { gateway: '172.30.0.1' }), { type: 'port', port: 3001, ip: '172.30.0.1' });
  assert.deepEqual(T.sourceFromFlags({ static: 'www/blog/' }), { type: 'static', dir: 'blog' });
  assert.equal(T.sourceFromFlags({}), null);
  assert.throws(() => T.sourceFromFlags({ static: '../etc' }), /invalid static dir/);
  assert.throws(() => T.sourceFromFlags({ port: '3001', static: 'x' }), /choose one source/);
});

await test('validatePath refuses the root and odd characters', () => {
  assert.equal(T.validatePath('/admin'), null);
  assert.match(T.validatePath('/'), /site add/);
  assert.match(T.validatePath('/a b'), /letters/);
});

// ── .env ──────────────────────────────────────────────────────────────────────

await test('parseEnv reads values, strips quotes, skips comments', () => {
  assert.deepEqual(parseEnv('# c\nA=1\nB="two"\n  C = x \n'), { A: '1', B: 'two', C: 'x' });
});

await test('setEnvText replaces in place, uncomments, or appends', () => {
  assert.equal(setEnvText('# top\nA=1\nB=2\n', 'A', '9'), '# top\nA=9\nB=2\n');
  assert.equal(setEnvText('#A=1\n', 'A', '2'), 'A=2\n');
  assert.equal(setEnvText('B=2', 'A', '1'), 'B=2\nA=1\n');
});

// ── rendering ─────────────────────────────────────────────────────────────────

await test('render drops lines that hold only an empty token', () => {
  assert.equal(render('a\n    {{X}}\nb {{Y}}\n', { X: '', Y: '$1 & $&' }), 'a\nb $1 & $&\n');
});

const STATE = {
  version: 1,
  certs: {
    'example.com': { type: 'letsencrypt', names: ['example.com', '*.example.com'] },
    'api.other.org': { type: 'custom', names: ['api.other.org'], aop: true },
  },
  domains: { 'example.com': { cert: 'example.com', zone: null } },
  sites: {
    'example.com': { cert: 'example.com', source: { type: 'static', dir: 'example.com' }, dns: 'proxied' },
    'api.example.com': { cert: 'example.com', source: { type: 'app', app: 'shop', service: 'web', port: 3000, upstream: 'shop-web' } },
    'api.other.org': { cert: 'api.other.org', source: { type: 'port', port: 3001, ip: '172.30.0.1' } },
    'off.example.com': { cert: 'example.com', enabled: false, source: { type: 'container', name: 'x', port: 1 } },
  },
  paths: {
    'example.com/admin': { host: 'example.com', path: '/admin', source: { type: 'container', name: 'admin', port: 3000 }, strip: false },
    'example.com/api/v1': { host: 'example.com', path: '/api/v1', source: { type: 'container', name: 'api', port: 8000 }, strip: true },
    'example.com/docs': { host: 'example.com', path: '/docs', source: { type: 'static', dir: 'docs' } },
  },
};

await test('buildFiles writes one file per cert, enabled site and path', () => {
  const files = buildFiles(STATE);
  assert.deepEqual([...files.keys()].sort(), [
    'conf/locations/example.com/admin.conf',
    'conf/locations/example.com/api-v1.conf',
    'conf/locations/example.com/docs.conf',
    'conf/sites/api.example.com.conf',
    'conf/sites/api.other.org.conf',
    'conf/sites/example.com.conf',
    'conf/snippets/ssl/api.other.org.conf',
    'conf/snippets/ssl/example.com.conf',
  ]);
  for (const content of files.values()) assert.ok(content.startsWith(MARKER));
});

await test('apex gets www when its cert covers it; upstreams and certs are wired', () => {
  const f = buildFiles(STATE);
  assert.match(f.get('conf/sites/example.com.conf'), /server_name example\.com www\.example\.com;/);
  assert.match(f.get('conf/sites/example.com.conf'), /root \/var\/www\/example\.com;/);
  assert.match(f.get('conf/sites/api.example.com.conf'), /set \$upstream shop-web:3000;/);
  assert.match(f.get('conf/sites/api.example.com.conf'), /snippets\/ssl\/example\.com\.conf/);
  assert.match(f.get('conf/sites/api.other.org.conf'), /set \$upstream 172\.30\.0\.1:3001;/);
  assert.match(f.get('conf/snippets/ssl/example.com.conf'), /\/etc\/letsencrypt\/live\/example\.com\/fullchain\.pem/);
  assert.match(f.get('conf/snippets/ssl/api.other.org.conf'), /\/etc\/edge-certs\/api\.other\.org\/privkey\.pem/);
  assert.match(f.get('conf/snippets/ssl/api.other.org.conf'), /include .*cloudflare-aop\.conf;/);
  assert.doesNotMatch(f.get('conf/snippets/ssl/example.com.conf'), /include/);
});

await test('path routes: strip adds a rewrite, keep does not', () => {
  const f = buildFiles(STATE);
  assert.match(f.get('conf/locations/example.com/api-v1.conf'), /rewrite \^\/api\/v1\/\(\.\*\)\$ \/\$1 break;/);
  assert.doesNotMatch(f.get('conf/locations/example.com/admin.conf'), /rewrite/);
  assert.doesNotMatch(f.get('conf/locations/example.com/admin.conf'), /\{\{/);
  assert.match(f.get('conf/locations/example.com/docs.conf'), /alias \/var\/www\/docs\/;/);
});

await test('buildFiles refuses a site whose cert is unknown', () => {
  assert.throws(() => buildFiles({ ...STATE, sites: { 'x.io': { cert: 'nope', source: { type: 'static', dir: 'x' } } } }), /not in edge\.json/);
});

await test('writeFiles syncs managed files and never touches hand-written ones', () => {
  const hand = path.join(SANDBOX, 'conf', 'sites', '00-default.conf');
  fs.writeFileSync(hand, 'server {}\n');
  writeFiles(buildFiles(STATE));
  assert.equal(readManaged().size, 8);
  assert.ok(fs.existsSync(path.join(SANDBOX, 'conf', 'locations', 'api.example.com')), 'every site gets a locations dir');

  const fewer = structuredClone(STATE);
  delete fewer.paths['example.com/docs'];
  writeFiles(buildFiles(fewer));
  assert.ok(!fs.existsSync(path.join(SANDBOX, 'conf/locations/example.com/docs.conf')));
  assert.equal(fs.readFileSync(hand, 'utf8'), 'server {}\n');

  fs.writeFileSync(path.join(SANDBOX, 'conf/sites/api.example.com.conf'), 'mine\n');
  assert.throws(() => writeFiles(buildFiles(STATE)), /not written by edge/);
});

// the last test left a hand-written api.example.com.conf behind
fs.rmSync(path.join(SANDBOX, 'conf/sites/api.example.com.conf'));
const STATE_OK = STATE;

// ── apps ──────────────────────────────────────────────────────────────────────

const CONFIG = {
  name: 'shop',
  networks: { default: { name: 'shop_default' }, proxy: { name: 'proxy', external: true } },
  services: {
    web: { container_name: 'shop_web', expose: ['3000'], networks: { proxy: { aliases: ['shop-web'] }, default: null } },
    api: { ports: [{ target: 8000, published: '8000' }], networks: { default: null } },
    migrate: { restart: 'no', networks: { default: null } },
  },
};

await test('summarizeServices reads ports, networks and aliases only', () => {
  const s = summarizeServices(CONFIG, 'proxy');
  assert.deepEqual(s.map((x) => x.name), ['api', 'web', 'migrate']);
  const web = s.find((x) => x.name === 'web');
  assert.deepEqual(web.ports, [3000]);
  assert.ok(web.onEdge);
  assert.equal(upstreamFor(web), 'shop-web');
  const api = s.find((x) => x.name === 'api');
  assert.ok(!api.onEdge);
  assert.deepEqual(api.ports, [8000]);
  assert.equal(upstreamFor(api), 'shop-api-1');
  assert.ok(s.find((x) => x.name === 'migrate').oneOff);
});

await test('override keeps existing networks and adds the edge one', () => {
  const y = overrideYaml('proxy', { api: { alias: 'shop-api', keys: ['default'] } });
  assert.match(y, /  api:\n    networks:\n      default: \{\}\n      nginx_edge:\n        aliases: \[shop-api\]/);
  assert.match(y, /networks:\n  nginx_edge:\n    name: proxy\n    external: true/);
  assert.match(y, /^# edge: \{"network":"proxy"/m);
});

await test('finds compose files and derives app names', () => {
  const root = path.join(SANDBOX, 'home');
  fs.mkdirSync(path.join(root, 'u', 'My Site'), { recursive: true });
  fs.mkdirSync(path.join(root, 'u', 'node_modules', 'x'), { recursive: true });
  fs.writeFileSync(path.join(root, 'u', 'My Site', 'docker-compose.yml'), 'services: {}\n');
  fs.writeFileSync(path.join(root, 'u', 'node_modules', 'x', 'compose.yaml'), 'services: {}\n');
  const found = scanForProjects([root]);
  assert.equal(found.length, 1);
  assert.equal(findComposeFile(path.join(root, 'u', 'My Site')), found[0]);
  assert.equal(appNameFor(found[0]), 'my-site');
});

// ── removal plans ────────────────────────────────────────────────────────────

const REM = {
  version: 1,
  certs: {
    'example.com': { type: 'letsencrypt', challenge: 'http', names: ['example.com', 'www.example.com'] },
    'wild.example.com': { type: 'letsencrypt', names: ['example.com', '*.example.com'] },
    'api.example.com': { type: 'self-signed', names: ['api.example.com'] },
    'shop.example.com': { type: 'origin', names: ['shop.example.com', '*.shop.example.com'] },
    'spare': { type: 'custom', names: ['spare.org'] },
  },
  domains: {
    'example.com': { cert: 'example.com', zone: null },
    'shop.example.com': { cert: 'shop.example.com', zone: null },
  },
  sites: {
    'example.com': { cert: 'example.com', source: { type: 'static', dir: 'example.com' }, dns: 'proxied' },
    'api.example.com': { cert: 'api.example.com', source: { type: 'app', app: 'shop', service: 'api', port: 8000, upstream: 'shop-api' }, dns: 'dns-only' },
    'blog.example.com': { cert: 'wild.example.com', source: { type: 'static', dir: 'shared' }, dns: 'skip' },
    'shop.example.com': { cert: 'shop.example.com', source: { type: 'app', app: 'shop', service: 'web', port: 3000, upstream: 'shop-web' }, dns: 'proxied' },
  },
  paths: {
    'example.com/docs': { host: 'example.com', path: '/docs', source: { type: 'static', dir: 'shared' } },
    'example.com/api': { host: 'example.com', path: '/api', source: { type: 'app', app: 'shop', service: 'api', port: 8000 } },
  },
};
const sorted = (set) => [...set].sort();

await test('removing a host takes its paths; unused cert, folder and DNS become extras', () => {
  const p = planRemoval(REM, { kind: 'site', key: 'example.com' });
  assert.deepEqual(sorted(p.drop.sites), ['example.com']);
  assert.deepEqual(sorted(p.drop.paths), ['example.com/api', 'example.com/docs']);
  assert.ok(p.next.domains['example.com'], 'the domain stays');
  assert.deepEqual(p.extras.certs, [], 'the domain still uses its cert');
  assert.deepEqual(p.extras.www, ['example.com'], 'shared is still served by blog');
  assert.deepEqual(p.extras.dns, ['example.com', 'www.example.com']);
  assert.ok(cascades(p));
});

await test('removing a domain keeps hosts of a more specific domain', () => {
  const p = planRemoval(REM, { kind: 'domain', key: 'example.com' });
  assert.deepEqual(sorted(p.drop.sites), ['api.example.com', 'blog.example.com', 'example.com']);
  assert.ok(p.next.sites['shop.example.com'] && p.next.domains['shop.example.com']);
  assert.deepEqual(p.extras.certs, ['api.example.com', 'example.com', 'wild.example.com']);
  assert.deepEqual(p.extras.www, ['example.com', 'shared']);
  assert.deepEqual(p.extras.dns, ['example.com', 'www.example.com', 'api.example.com']);
  assert.ok(!p.extras.certs.includes('spare'), 'certs that were unused already are not offered');
});

await test('removing an app drops every route that points at it', () => {
  const p = planRemoval(REM, { kind: 'app', key: 'shop' }, { linked: ['shop', 'other'] });
  assert.deepEqual(sorted(p.drop.apps), ['shop']);
  assert.deepEqual(sorted(p.drop.sites), ['api.example.com', 'shop.example.com']);
  assert.deepEqual(sorted(p.drop.paths), ['example.com/api']);
  assert.deepEqual(p.extras.down, ['shop']);
  assert.throws(() => planRemoval(REM, { kind: 'app', key: 'nope' }, { linked: [] }), /no app named/);
});

await test('removing a cert: move its hosts to a covering cert, or drop them', () => {
  const moved = planRemoval(REM, { kind: 'cert', key: 'example.com', replaceCert: 'wild.example.com' });
  assert.equal(moved.next.sites['example.com'].cert, 'wild.example.com');
  assert.equal(moved.next.domains['example.com'].cert, 'wild.example.com');
  assert.equal(moved.drop.sites.size, 0);
  assert.throws(() => planRemoval(REM, { kind: 'cert', key: 'example.com', replaceCert: 'spare' }), /does not cover example\.com/);
  const dropped = planRemoval(REM, { kind: 'cert', key: 'example.com' });
  assert.deepEqual(sorted(dropped.drop.sites), ['example.com']);
  assert.ok(dropped.next.domains['example.com'], 'a domain is never dropped with its cert');
  assert.equal(dropped.next.domains['example.com'].cert, null);
  assert.ok(!cascades(planRemoval(REM, { kind: 'cert', key: 'spare' })));
});

await test('removing a static folder drops the routes serving it; reset drops everything', () => {
  fs.mkdirSync(path.join(SANDBOX, 'www', 'shared'), { recursive: true });
  const p = planRemoval(REM, { kind: 'www', key: 'shared' });
  assert.deepEqual(sorted(p.drop.sites), ['blog.example.com']);
  assert.deepEqual(sorted(p.drop.paths), ['example.com/docs']);
  assert.throws(() => planRemoval(REM, { kind: 'www', key: '../etc' }), /invalid folder/);
  const all = planRemoval(REM, { kind: 'all', key: '' }, { linked: ['shop'] });
  assert.deepEqual(all.next, { version: 1, certs: {}, domains: {}, sites: {}, paths: {} });
  assert.deepEqual(sorted(all.drop.apps), ['shop']);
});

await test('writeFiles removes empty location dirs of hosts that are gone', () => {
  writeFiles(buildFiles(STATE_OK));
  fs.writeFileSync(path.join(SANDBOX, 'conf/locations/api.example.com/hand.conf'), 'location /x {}\n');
  const fewer = structuredClone(STATE_OK);
  delete fewer.sites['api.other.org'];
  delete fewer.sites['api.example.com'];
  writeFiles(buildFiles(fewer));
  assert.ok(!fs.existsSync(path.join(SANDBOX, 'conf/locations/api.other.org')));
  assert.ok(fs.existsSync(path.join(SANDBOX, 'conf/locations/api.example.com/hand.conf')), 'hand-written files stay');
});

await test('letsencrypt entries without a challenge are DNS-01; labels say which', () => {
  assert.equal(challengeOf({ type: 'letsencrypt' }), 'dns');
  assert.equal(challengeOf({ type: 'letsencrypt', challenge: 'http' }), 'http');
  assert.equal(challengeOf({ type: 'origin' }), null);
  assert.equal(certLabel({ type: 'letsencrypt', challenge: 'http' }), "Let's Encrypt (HTTP)");
  assert.equal(certLabel({ type: 'custom' }), 'custom');
});

await test('plain HTTP: serve adds a :80 server and turns HSTS off for that host only', () => {
  const st = structuredClone(STATE);
  st.sites['api.example.com'].http = 'serve';
  const f = buildFiles(st);
  const api = f.get('conf/sites/api.example.com.conf');
  assert.equal((api.match(/^server \{/gm) || []).length, 2);
  assert.match(api, /listen 80;\n    server_name api\.example\.com;/);
  assert.match(api, /listen 80;[\s\S]*include \/etc\/nginx\/edge\/locations\/api\.example\.com\/\*\.conf;[\s\S]*set \$upstream shop-web:3000;/);
  assert.match(api, /Strict-Transport-Security "max-age=0"/);
  assert.doesNotMatch(api, /max-age=31536000/);
  const apex = f.get('conf/sites/example.com.conf');
  assert.doesNotMatch(apex, /listen 80;/);
  assert.match(apex, /Strict-Transport-Security "max-age=31536000; includeSubDomains" always;/);
});

await test('shared snippets: no HSTS in security-headers, scheme-aware X-Forwarded-Proto', () => {
  const conf = (f) => fs.readFileSync(path.join(REAL_ROOT, 'conf', 'snippets', f), 'utf8');
  assert.doesNotMatch(conf('security-headers.conf'), /^add_header Strict-Transport-Security/m);
  assert.match(conf('proxy.conf'), /X-Forwarded-Proto \$scheme;/);
});

await test('generated sites answer ACME challenges', () => {
  assert.match(buildFiles(STATE).get('conf/sites/example.com.conf'), /include \/etc\/nginx\/edge\/snippets\/acme-challenge\.conf;/);
});

// ── keys ──────────────────────────────────────────────────────────────────────

await test('decode turns escape sequences and control keys into names', () => {
  assert.deepEqual(decode(Buffer.from('\x1b[A\x1b[Bx\r\x7f\x1b')).map((k) => k.name), ['up', 'down', 'char', 'enter', 'backspace', 'escape']);
  assert.deepEqual(decode(Buffer.from('\x15\x17')).map((k) => k.name), ['ctrl-u', 'ctrl-w']);
});

fs.rmSync(SANDBOX, { recursive: true, force: true });
console.log(`\n  ${passed} passed, ${failed} failed`);
process.exitCode = failed ? 1 : 0;
