// Certificates. Four kinds, one shape in edge.json: { type, names, aop? }
//   letsencrypt   certbot + Cloudflare DNS-01 → data/letsencrypt/live/<name>/   (auto-renewed)
//   origin        Cloudflare Origin CA via API → data/certs/<name>/             (15 years, proxied only)
//   custom        your own PEM files           → data/certs/<name>/
//   self-signed   openssl, for testing         → data/certs/<name>/

import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { P, readEnv } from './config.mjs';
import { compose, must, hasCommand } from './exec.mjs';
import { certPaths } from './nginx.mjs';
import { certCovers } from './targets.mjs';
import * as cf from './cloudflare.mjs';

/** Parse a PEM chain; facts about its first (leaf) certificate. */
export function inspectPem(pem) {
  const first = pem.match(/-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/);
  if (!first) throw new Error('no PEM certificate found');
  const x = new crypto.X509Certificate(first[0]);
  const names = (x.subjectAltName || '')
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.startsWith('DNS:'))
    .map((s) => s.slice(4));
  const cn = (x.subject.match(/CN=([^\n,]+)/) || [])[1];
  return {
    x509: x,
    names: names.length ? names : cn ? [cn] : [],
    validTo: new Date(x.validTo),
    validFrom: new Date(x.validFrom),
    issuer: (x.issuer.match(/O=([^\n]+)/) || x.issuer.match(/CN=([^\n]+)/) || [])[1] || x.issuer,
    selfSigned: x.issuer === x.subject,
  };
}

/** Everything in edge.json with what is actually on disk. */
export function listCerts(state) {
  return Object.entries(state.certs).map(([name, cert]) => {
    const p = certPaths(name, cert);
    let info = null;
    let problem = null;
    try {
      info = inspectPem(fs.readFileSync(p.crt, 'utf8'));
    } catch (err) {
      problem = fs.existsSync(p.crt) ? err.message : 'certificate file missing';
    }
    return { name, ...cert, names: info?.names || cert.names || [], info, problem, paths: p };
  });
}

/** Certificates in edge.json whose names cover `host`, longest-lived first. */
export function certsFor(state, host) {
  return listCerts(state)
    .filter((c) => !c.problem && certCovers(c.names, host))
    .sort((a, b) => (b.info?.validTo || 0) - (a.info?.validTo || 0));
}

/** A cert name that is free in edge.json and on disk: api.example.com, api.example.com-2 … */
export function freeCertName(state, base) {
  let name = base;
  for (let i = 2; state.certs[name] || fs.existsSync(path.join(P.certs, name)) || fs.existsSync(path.join(P.letsencrypt, 'live', name)); i++) {
    name = `${base}-${i}`;
  }
  return name;
}

// ── Let's Encrypt ───────────────────────────────────────────────────────────

export async function issueLetsEncrypt(name, names) {
  const env = readEnv();
  if (!env.ACME_EMAIL) throw new Error('ACME_EMAIL is not set — run `edge init` or add it to .env');
  const args = [
    'run', '--rm', '--no-deps', '--entrypoint', 'certbot', 'certbot', 'certonly',
    '--dns-cloudflare', '--dns-cloudflare-credentials', '/secrets/cloudflare.ini',
    '--dns-cloudflare-propagation-seconds', env.CF_PROPAGATION_SECONDS || '30',
    '--cert-name', name,
    ...names.flatMap((n) => ['-d', n]),
    '--email', env.ACME_EMAIL, '--agree-tos', '--no-eff-email',
    '--non-interactive', '--keep-until-expiring',
  ];
  await must('docker', ['compose', '--progress', 'quiet', ...args]);
  return { type: 'letsencrypt', names };
}

export async function renewLetsEncrypt() {
  const res = await compose(['--progress', 'quiet', 'run', '--rm', '--no-deps', '--entrypoint', 'certbot', 'certbot', 'renew']);
  return res;
}

async function deleteLetsEncrypt(name) {
  await compose(['--progress', 'quiet', 'run', '--rm', '--no-deps', '--entrypoint', 'certbot', 'certbot', 'delete', '--non-interactive', '--cert-name', name]);
}

// ── files in data/certs ─────────────────────────────────────────────────────

/** Validate a cert + key pair and store it as data/certs/<name>/. */
export function storePem(name, certPem, keyPem) {
  const info = inspectPem(certPem);
  let key;
  try {
    key = crypto.createPrivateKey(keyPem);
  } catch {
    throw new Error('the private key could not be read (PEM, unencrypted)');
  }
  if (!info.x509.checkPrivateKey(key)) throw new Error('the private key does not belong to this certificate');
  if (info.validTo < new Date()) throw new Error(`the certificate expired on ${info.validTo.toISOString().slice(0, 10)}`);
  const dir = path.join(P.certs, name);
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  fs.writeFileSync(path.join(dir, 'fullchain.pem'), certPem.trim() + '\n');
  fs.writeFileSync(path.join(dir, 'privkey.pem'), keyPem.trim() + '\n', { mode: 0o600 });
  return info;
}

export async function importCustom(name, certPem, keyPem) {
  const info = storePem(name, certPem, keyPem);
  return { type: 'custom', names: info.names };
}

export async function selfSigned(name, names) {
  if (!(await hasCommand('openssl'))) throw new Error('openssl is needed for a self-signed certificate');
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'edge-'));
  try {
    await must('openssl', [
      'req', '-x509', '-nodes', '-newkey', 'rsa:2048', '-days', '90',
      '-subj', `/CN=${names[0]}`, '-addext', `subjectAltName=${names.map((n) => `DNS:${n}`).join(',')}`,
      '-keyout', path.join(tmp, 'key.pem'), '-out', path.join(tmp, 'crt.pem'),
    ]);
    storePem(name, fs.readFileSync(path.join(tmp, 'crt.pem'), 'utf8'), fs.readFileSync(path.join(tmp, 'key.pem'), 'utf8'));
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  return { type: 'self-signed', names };
}

/** Cloudflare Origin CA: key + CSR made here, signed by Cloudflare's API (15 years). */
export async function originCert(name, names, token) {
  if (!(await hasCommand('openssl'))) throw new Error('openssl is needed to create the key and CSR');
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'edge-'));
  try {
    await must('openssl', [
      'req', '-new', '-nodes', '-newkey', 'rsa:2048', '-subj', `/CN=${names[0]}`,
      '-keyout', path.join(tmp, 'key.pem'), '-out', path.join(tmp, 'csr.pem'),
    ]);
    const csr = fs.readFileSync(path.join(tmp, 'csr.pem'), 'utf8');
    const certPem = await cf.createOriginCert(token, names, csr);
    storePem(name, certPem, fs.readFileSync(path.join(tmp, 'key.pem'), 'utf8'));
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  return { type: 'origin', names };
}

export async function removeCertFiles(name, cert) {
  if (cert.type === 'letsencrypt') await deleteLetsEncrypt(name);
  else fs.rmSync(path.join(P.certs, name), { recursive: true, force: true });
}
