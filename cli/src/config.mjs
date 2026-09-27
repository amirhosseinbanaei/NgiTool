// Where everything lives, plus the three files the CLI owns:
//   .env                    stack settings (ports, network, email, server IP)
//   edge.json               what is served: certs, domains, sites, paths
//   secrets/cloudflare.ini  the Cloudflare API token (shared with certbot)

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));

// cli/src → the nginx-edge directory. EDGE_ROOT lets tests point at a copy.
export const ROOT = process.env.EDGE_ROOT ? path.resolve(process.env.EDGE_ROOT) : path.resolve(HERE, '..', '..');

export const P = {
  root: ROOT,
  env: path.join(ROOT, '.env'),
  envExample: path.join(ROOT, '.env.example'),
  state: path.join(ROOT, 'edge.json'),
  conf: path.join(ROOT, 'conf'),
  sites: path.join(ROOT, 'conf', 'sites'),
  locations: path.join(ROOT, 'conf', 'locations'),
  sslSnippets: path.join(ROOT, 'conf', 'snippets', 'ssl'),
  realip: path.join(ROOT, 'conf', 'conf.d', 'cloudflare-realip.conf'),
  aopCa: path.join(ROOT, 'conf', 'certs', 'cloudflare-origin-pull-ca.pem'),
  templates: path.join(ROOT, 'templates'),
  www: path.join(ROOT, 'www'),
  letsencrypt: path.join(ROOT, 'data', 'letsencrypt'),
  certs: path.join(ROOT, 'data', 'certs'),
  acme: path.join(ROOT, 'data', 'acme'),
  apps: path.join(ROOT, 'apps'),
  secrets: path.join(ROOT, 'secrets'),
  cfIni: path.join(ROOT, 'secrets', 'cloudflare.ini'),
  cfIniExample: path.join(ROOT, 'secrets', 'cloudflare.ini.example'),
};

/** Paths as the nginx container sees them. */
export const IN = {
  conf: '/etc/nginx/edge/nginx.conf',
  letsencrypt: '/etc/letsencrypt',
  certs: '/etc/edge-certs',
  acme: '/var/acme',
};

// ── .env ────────────────────────────────────────────────────────────────────

export function parseEnv(text) {
  const out = {};
  for (const line of text.split('\n')) {
    const m = line.match(/^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$/);
    if (!m) continue;
    let v = m[2];
    if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) v = v.slice(1, -1);
    out[m[1]] = v;
  }
  return out;
}

/** Replace KEY=… in place (keeping comments and order), or append it. */
export function setEnvText(text, key, value) {
  const re = new RegExp(`^(\\s*#?\\s*)${key}\\s*=.*$`, 'm');
  const line = `${key}=${value}`;
  if (re.test(text)) return text.replace(re, line);
  return text.replace(/\n*$/, '\n') + line + '\n';
}

export const ENV_DEFAULTS = {
  ACME_EMAIL: '',
  HTTP_PORT: '80',
  HTTPS_PORT: '443',
  EDGE_NETWORK: 'edge',
  SERVER_IP: '',
  CF_PROPAGATION_SECONDS: '30',
};

export function readEnv() {
  const file = fs.existsSync(P.env) ? parseEnv(fs.readFileSync(P.env, 'utf8')) : {};
  return { ...ENV_DEFAULTS, ...file };
}

export function writeEnv(values) {
  let text = fs.existsSync(P.env)
    ? fs.readFileSync(P.env, 'utf8')
    : fs.existsSync(P.envExample)
      ? fs.readFileSync(P.envExample, 'utf8')
      : '';
  for (const [k, v] of Object.entries(values)) text = setEnvText(text, k, v);
  fs.writeFileSync(P.env, text);
}

// ── edge.json ───────────────────────────────────────────────────────────────

export const emptyState = () => ({ version: 1, certs: {}, domains: {}, sites: {}, paths: {} });

export function loadState() {
  if (!fs.existsSync(P.state)) return emptyState();
  const s = JSON.parse(fs.readFileSync(P.state, 'utf8'));
  return { ...emptyState(), ...s };
}

export function saveState(state) {
  const sorted = (o) => Object.fromEntries(Object.entries(o).sort(([a], [b]) => a.localeCompare(b)));
  const out = {
    version: 1,
    certs: sorted(state.certs),
    domains: sorted(state.domains),
    sites: sorted(state.sites),
    paths: sorted(state.paths),
  };
  fs.writeFileSync(P.state, JSON.stringify(out, null, 2) + '\n');
}

export const clone = (o) => JSON.parse(JSON.stringify(o));

// ── Cloudflare token ────────────────────────────────────────────────────────

const PLACEHOLDER = 'PASTE_TOKEN_HERE';

export function readToken() {
  if (!fs.existsSync(P.cfIni)) return null;
  const m = fs.readFileSync(P.cfIni, 'utf8').match(/^\s*dns_cloudflare_api_token\s*=\s*(\S+)/m);
  return m && m[1] !== PLACEHOLDER ? m[1] : null;
}

export function writeToken(token) {
  fs.mkdirSync(P.secrets, { recursive: true, mode: 0o700 });
  fs.chmodSync(P.secrets, 0o700);
  fs.writeFileSync(
    P.cfIni,
    `# Cloudflare API token — used by certbot (DNS-01) and by the edge CLI (DNS records).\n` +
      `dns_cloudflare_api_token = ${token || PLACEHOLDER}\n`,
    { mode: 0o600 },
  );
  fs.chmodSync(P.cfIni, 0o600);
}
