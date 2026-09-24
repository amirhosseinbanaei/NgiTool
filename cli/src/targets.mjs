// Pure helpers: hostnames, paths, certificate coverage, traffic sources.
// Nothing here touches the disk or the network — test/run.mjs covers it.

const HOST_RE = /^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$/;
const PATH_RE = /^(\/[A-Za-z0-9._~-]+)+$/;
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

export class UsageError extends Error {}

export const isHost = (h) => HOST_RE.test(h);
export const isPath = (p) => PATH_RE.test(p);
export const isName = (n) => NAME_RE.test(n) && n.length <= 64;
export const isPort = (p) => /^\d{1,5}$/.test(String(p)) && +p >= 1 && +p <= 65535;

export function validateHost(h) {
  if (!h) return 'required';
  if (!isHost(h.toLowerCase())) return 'not a valid hostname (e.g. api.example.com)';
  return null;
}

export function validatePath(p) {
  if (!p) return 'required';
  const norm = normalizePath(p);
  if (norm === '/') return 'use `edge site add` for the root of a host';
  if (!isPath(norm)) return 'letters, digits and . _ ~ - only, e.g. /admin or /api/v1';
  return null;
}

export const normalizePath = (p) => '/' + String(p).trim().replace(/^\/+|\/+$/g, '');

/** "https://Example.com/Admin/" → { host: 'example.com', path: '/Admin' } (path '' = whole host) */
export function parseTarget(input) {
  let t = String(input || '').trim().replace(/^https?:\/\//, '').replace(/\/+$/, '');
  const slash = t.indexOf('/');
  const host = (slash === -1 ? t : t.slice(0, slash)).toLowerCase();
  const path = slash === -1 ? '' : normalizePath(t.slice(slash));
  if (!isHost(host)) throw new UsageError(`invalid hostname: ${host || '(empty)'}`);
  if (path && !isPath(path)) throw new UsageError(`invalid path: ${path}`);
  return { host, path, key: host + path };
}

/** File-name-safe slug of a path: /api/v1 → api-v1 */
export const slugPath = (p) => p.replace(/^\//, '').replace(/\//g, '-');

// ── certificates ────────────────────────────────────────────────────────────

/** Does a certificate name (exact or "*.domain") cover this host? Wildcards cover one level only. */
export function nameCovers(name, host) {
  name = name.toLowerCase();
  if (name === host) return true;
  if (name.startsWith('*.')) {
    const base = name.slice(2);
    return host.endsWith('.' + base) && !host.slice(0, -base.length - 1).includes('.');
  }
  return false;
}

export const certCovers = (names, host) => names.some((n) => nameCovers(n, host));

/** Domain in `domains` that `host` belongs to (longest suffix match), or null. */
export function domainOf(host, domains) {
  let best = null;
  for (const d of domains) {
    if ((host === d || host.endsWith('.' + d)) && (!best || d.length > best.length)) best = d;
  }
  return best;
}

/** Levels below the domain: api.example.com → 1, a.b.example.com → 2, example.com → 0 */
export const depthBelow = (host, domain) =>
  host === domain ? 0 : host.slice(0, -domain.length - 1).split('.').length;

/** Best guess at the registrable domain when none is configured: last two labels. */
export const guessDomain = (host) => host.split('.').slice(-2).join('.');

export const CERT_TYPES = {
  letsencrypt: "Let's Encrypt",
  origin: 'Cloudflare Origin CA',
  custom: 'custom',
  'self-signed': 'self-signed',
};

// ── sources ─────────────────────────────────────────────────────────────────
// A source is where a route sends traffic. Stored in edge.json as one of:
//   { type: 'app',       app, service, port, upstream }   linked compose project
//   { type: 'container', name, port }                      any container on the nginx network
//   { type: 'port',      port, ip }                        process on the host
//   { type: 'static',    dir }                             files in www/<dir>

export function upstreamOf(src) {
  switch (src.type) {
    case 'app':
      return `${src.upstream}:${src.port}`;
    case 'container':
      return `${src.name}:${src.port}`;
    case 'port':
      return `${src.ip}:${src.port}`;
    default:
      return null;
  }
}

export function describeSource(src) {
  switch (src.type) {
    case 'app':
      return `app ${src.app}/${src.service}:${src.port}`;
    case 'container':
      return `container ${src.name}:${src.port}`;
    case 'port':
      return `host port ${src.port}`;
    case 'static':
      return `static www/${src.dir}`;
    default:
      return '?';
  }
}

/**
 * Build a source from command-line flags, or null when none was given.
 * --app name/service:port · --container name:port · --port 3001 · --static dir
 */
export function sourceFromFlags(opts, { gateway } = {}) {
  const given = ['app', 'container', 'port', 'static'].filter((k) => opts[k] != null);
  if (!given.length) return null;
  if (given.length > 1) throw new UsageError(`choose one source, got --${given.join(' and --')}`);

  if (opts.app != null) {
    const m = String(opts.app).match(/^([^/:]+)\/([^/:]+)(?::(\d+))?$/);
    if (!m) throw new UsageError('--app expects NAME/SERVICE[:PORT], e.g. shop/web:3000');
    return { type: 'app', app: m[1], service: m[2], port: m[3] ? +m[3] : null };
  }
  if (opts.container != null) {
    const m = String(opts.container).match(/^([A-Za-z0-9][A-Za-z0-9._-]*):(\d{1,5})$/);
    if (!m) throw new UsageError('--container expects NAME:PORT, e.g. my-app:3000');
    return { type: 'container', name: m[1], port: +m[2] };
  }
  if (opts.port != null) {
    if (!isPort(opts.port)) throw new UsageError('--port expects a port number');
    return { type: 'port', port: +opts.port, ip: gateway };
  }
  const dir = String(opts.static).replace(/^www\//, '').replace(/\/+$/, '');
  if (!/^[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)*$/.test(dir) || dir.includes('..')) {
    throw new UsageError(`invalid static dir: ${opts.static}`);
  }
  return { type: 'static', dir };
}
