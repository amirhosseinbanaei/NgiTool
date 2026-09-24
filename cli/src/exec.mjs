// Child processes: docker, docker compose, openssl. Output is captured unless
// `inherit` is set (logs, interactive certbot), so spinners stay clean.

import { spawn } from 'node:child_process';
import { P } from './config.mjs';

export class CommandError extends Error {
  constructor(cmd, res) {
    const tail = (res.stderr || res.stdout || '').trim().split('\n').slice(-12).join('\n');
    super(`${cmd} failed (exit ${res.code})${tail ? `\n${tail}` : ''}`);
    this.res = res;
  }
}

export function run(cmd, args, { cwd = P.root, input, inherit = false, env } = {}) {
  return new Promise((resolve) => {
    const child = spawn(cmd, args, {
      cwd,
      env: env ? { ...process.env, ...env } : process.env,
      stdio: inherit ? 'inherit' : ['pipe', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    if (!inherit) {
      child.stdout.on('data', (d) => (stdout += d));
      child.stderr.on('data', (d) => (stderr += d));
      if (input !== undefined) child.stdin.end(input);
      else child.stdin.end();
    }
    child.on('error', (err) => resolve({ code: 127, stdout, stderr: err.message }));
    child.on('close', (code) => resolve({ code: code ?? 1, stdout, stderr }));
  });
}

/** run(), but throw CommandError on a non-zero exit. */
export async function must(cmd, args, opts) {
  const res = await run(cmd, args, opts);
  if (res.code !== 0) throw new CommandError(`${cmd} ${args[0] ?? ''}`.trim(), res);
  return res;
}

export const docker = (args, opts) => run('docker', args, opts);
export const compose = (args, opts) => run('docker', ['compose', ...args], opts);

const jsonLines = (text) =>
  text
    .split('\n')
    .filter((l) => l.trim().startsWith('{'))
    .map((l) => JSON.parse(l));

// ── the edge stack itself ───────────────────────────────────────────────────

/** State of the nginx-edge services: { nginx: {state, health}, certbot: {...} } */
export async function stackStatus() {
  const res = await compose(['ps', '--all', '--format', 'json']);
  const out = {};
  if (res.code !== 0) return out;
  // compose prints either one JSON array or one object per line depending on version
  const text = res.stdout.trim();
  const list = text.startsWith('[') ? JSON.parse(text) : jsonLines(text);
  for (const s of list) out[s.Service] = { state: s.State, health: s.Health || '', name: s.Name };
  return out;
}

export async function nginxRunning() {
  return (await stackStatus()).nginx?.state === 'running';
}

let imageCache = null;
/** The nginx image the stack uses, read from compose.yaml (so `nginx -t` matches). */
export async function nginxImage() {
  if (imageCache) return imageCache;
  const res = await compose(['config', '--format', 'json']);
  imageCache = res.code === 0 ? JSON.parse(res.stdout).services?.nginx?.image || 'nginx:stable-alpine' : 'nginx:stable-alpine';
  return imageCache;
}

// ── docker objects ──────────────────────────────────────────────────────────

export async function networkInfo(name) {
  const res = await docker(['network', 'inspect', name]);
  if (res.code !== 0) return null;
  const n = JSON.parse(res.stdout)[0];
  const ipam = n.IPAM?.Config?.[0] || {};
  return {
    name: n.Name,
    id: n.Id,
    driver: n.Driver,
    subnet: ipam.Subnet || '',
    gateway: ipam.Gateway || '',
    bridge: n.Options?.['com.docker.network.bridge.name'] || `br-${n.Id.slice(0, 12)}`,
    containers: Object.values(n.Containers || {}).map((c) => c.Name),
    compose: n.Labels?.['com.docker.compose.project'] || '',
  };
}

/** User-defined bridge networks (the ones containers can share with nginx). */
export async function listNetworks() {
  const res = await docker(['network', 'ls', '--filter', 'driver=bridge', '--format', '{{.Name}}']);
  if (res.code !== 0) return [];
  const names = res.stdout.split('\n').filter((n) => n && n !== 'bridge');
  return (await Promise.all(names.map(networkInfo))).filter(Boolean);
}

export async function createNetwork(name, subnet) {
  const args = ['network', 'create', '--driver', 'bridge'];
  if (subnet) args.push('--subnet', subnet);
  if (name === 'edge') args.push('--opt', 'com.docker.network.bridge.name=br-edge');
  args.push(name);
  return must('docker', args);
}

/** Containers: [{ name, image, state, networks: [], ports, project, service, workingDir }] */
export async function listContainers({ all = false } = {}) {
  const fmt = [
    '{{.Names}}', '{{.Image}}', '{{.State}}', '{{.Status}}', '{{.Networks}}', '{{.Ports}}',
    '{{.Label "com.docker.compose.project"}}', '{{.Label "com.docker.compose.service"}}',
    '{{.Label "com.docker.compose.project.working_dir"}}',
  ].join('\t');
  const res = await docker(['ps', ...(all ? ['--all'] : []), '--format', fmt]);
  if (res.code !== 0) return [];
  return res.stdout
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      const [name, image, state, status, networks, ports, project, service, workingDir] = line.split('\t');
      return {
        name, image, state, status, ports, project, service, workingDir,
        networks: (networks || '').split(',').filter(Boolean),
      };
    });
}

/** Ports a container exposes (from its image/config): [3000, 8080] */
export async function exposedPorts(container) {
  const res = await docker(['inspect', '--format', '{{json .Config.ExposedPorts}}', container]);
  if (res.code !== 0) return [];
  const obj = JSON.parse(res.stdout.trim() || 'null') || {};
  return Object.keys(obj)
    .filter((k) => k.endsWith('/tcp'))
    .map((k) => Number(k.split('/')[0]))
    .sort((a, b) => a - b);
}

export async function connectNetwork(network, container) {
  return must('docker', ['network', 'connect', network, container]);
}

export async function hasCommand(cmd) {
  const res = await run('sh', ['-c', `command -v ${cmd}`]);
  return res.code === 0;
}
