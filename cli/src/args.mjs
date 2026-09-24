import { c } from './ui.mjs';

const BOOLEAN = new Set([
  'yes', 'force', 'help', 'version', 'no-color', 'strip', 'all', 'purge-dns', 'no-token', 'wildcard',
]);
const VALUE = new Set([
  'app', 'container', 'port', 'static', 'cert', 'dns', 'cert-file', 'key-file', 'name', 'email',
  'network', 'subnet', 'token', 'server-ip', 'apex', 'attach', 'tail',
]);
const ALIAS = { y: 'yes', f: 'force', h: 'help', v: 'version' };

/** argv → { _: [positional…], flag: value }. Booleans also take --no-<flag>. */
export function parseArgs(argv) {
  const opts = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === '--') {
      opts._.push(...argv.slice(i + 1));
      break;
    }
    if (arg.startsWith('--')) {
      const eq = arg.indexOf('=');
      const key = eq === -1 ? arg.slice(2) : arg.slice(2, eq);
      const inline = eq === -1 ? undefined : arg.slice(eq + 1);
      if (BOOLEAN.has(key)) {
        opts[key] = inline === undefined ? true : inline !== 'false';
      } else if (key.startsWith('no-') && BOOLEAN.has(key.slice(3))) {
        opts[key.slice(3)] = false;
      } else if (VALUE.has(key)) {
        const value = inline !== undefined ? inline : argv[++i];
        if (value === undefined) throw new Error(`--${key} needs a value`);
        opts[key] = value;
      } else {
        throw new Error(`unknown option --${key}`);
      }
      continue;
    }
    if (arg.startsWith('-') && arg.length > 1 && !/^-\d/.test(arg)) {
      for (const ch of arg.slice(1)) {
        const key = ALIAS[ch];
        if (!key) throw new Error(`unknown option -${ch}`);
        opts[key] = true;
      }
      continue;
    }
    opts._.push(arg);
  }
  return opts;
}

export function helpText() {
  const b = c.bold;
  const g = c.gray;
  return `
${b('edge')} — one nginx for every project on this server ${g('(nginx-edge, Docker Compose)')}

${b('USAGE')}
  edge                                   interactive menu
  edge <command> [args] [flags]          run one thing and exit

${b('SETUP')}
  edge init                              network, Cloudflare token, email, public IP
  edge status                            stack, certificates, routes, apps at a glance

${b('SERVE A PROJECT')}
  edge domain add [example.com]          certificate (+ apex site, DNS) for a new domain
  edge domain ls | rm <domain>
  edge site add [api.example.com]        subdomain → project; asks for DNS mode and certificate
  edge path add [example.com/admin]      path on an existing host → project
  edge ls                                everything that is served
  edge enable | disable | rm <target>    target = host or host/path

  Where traffic goes ${g('(asked interactively when not given)')}:
    --app NAME/SERVICE[:PORT]            a service of a project in apps/
    --container NAME:PORT                any container (joined to the nginx network)
    --port PORT                          a process on this server
    --static DIR                         files in www/DIR
  --dns proxied|dns-only|skip            Cloudflare record for the host
  --cert auto|NAME|letsencrypt|origin|custom|self-signed
         --cert-file F --key-file F      for --cert custom
  --strip | --no-strip                   path routes: remove the prefix before proxying
  --attach override|manual               service not on the nginx network yet

${b('APPS')}  ${g('apps/<name>/ holds a symlink to each project’s compose file')}
  edge app ls
  edge app link [dir|compose-file]       --name NAME
  edge app scan [dir…]                   find compose projects (default /home/*) and pick
  edge app unlink <name>
  edge app up|down|restart|ps|logs|pull|build <name> [service…]

${b('CERTIFICATES')}
  edge cert ls
  edge cert add [host,*.host]            --cert letsencrypt|origin|custom|self-signed
  edge cert renew                        renew due Let's Encrypt certs now, then reload
  edge cert rm <name>
  edge cert aop <name> on|off            Authenticated Origin Pulls (Cloudflare-only access)

${b('STACK')}
  edge up | down | restart               nginx + certbot
  edge reload | test                     nginx -t, then graceful reload
  edge logs [host]                       follow nginx logs, optionally one host
  edge sync                              regenerate every nginx file from edge.json
  edge cf-sync                           refresh Cloudflare IP ranges + origin-pull CA
  edge install                           put \`edge\` on PATH (/usr/local/bin/edge)

${b('FLAGS')}
  -y, --yes        don't ask for confirmation     -f, --force   replace / remove regardless
      --no-color   plain output                   -h, --help    this text
`.trimStart();
}
