// Terminal primitives: colour, width-aware layout, cursor control, spinners.
// No dependencies — this file is the whole "UI library".

const FORCED_OFF =
  process.env.NO_COLOR !== undefined ||
  process.env.TERM === 'dumb' ||
  !process.stdout.isTTY;

let enabled = !FORCED_OFF;

export function setColor(on) {
  enabled = on && !FORCED_OFF;
}

const ESC = '\x1b';
const wrap = (open, close) => (s) =>
  enabled ? `${ESC}[${open}m${s}${ESC}[${close}m` : String(s);

export const c = {
  bold: wrap(1, 22),
  dim: wrap(2, 22),
  italic: wrap(3, 23),
  underline: wrap(4, 24),
  inverse: wrap(7, 27),
  red: wrap(31, 39),
  green: wrap(32, 39),
  yellow: wrap(33, 39),
  blue: wrap(34, 39),
  magenta: wrap(35, 39),
  cyan: wrap(36, 39),
  gray: wrap(90, 39),
};

const ANSI = /\x1b\[[0-9;]*m/g;

export const strip = (s) => String(s).replace(ANSI, '');
export const width = (s) => [...strip(s)].length;

/** Truncate to `max` visible columns. Coloured input degrades to plain text. */
export function truncate(s, max) {
  if (max <= 0) return '';
  const plain = [...strip(s)];
  if (plain.length <= max) return s;
  return plain.slice(0, Math.max(0, max - 1)).join('') + '…';
}

export const pad = (s, n) => s + ' '.repeat(Math.max(0, n - width(s)));

export const columns = () => process.stdout.columns || 80;
export const rows = () => process.stdout.rows || 24;

// Cursor control is meaningless off a terminal — writing it would land in a pipe.
const esc = (s) => process.stdout.isTTY && process.stdout.write(s);

export const cursor = {
  hide: () => esc(`${ESC}[?25l`),
  show: () => esc(`${ESC}[?25h`),
  up: (n) => n > 0 && esc(`${ESC}[${n}A`),
  column0: () => esc(`${ESC}[0G`),
  clearLine: () => esc(`${ESC}[2K`),
  clearDown: () => esc(`${ESC}[0J`),
};

export const sym = {
  checked: '◉',
  unchecked: '◯',
  pointer: '❯',
  ok: '✔',
  warn: '!',
  err: '✖',
  arrow: '→',
  dot: '●',
  ring: '○',
  bar: '│',
  info: 'ℹ',
};

export const log = {
  plain: (m = '') => console.log(m),
  step: (m) => console.log(`${c.cyan('•')} ${m}`),
  info: (m) => console.log(`${c.blue(sym.info)} ${m}`),
  ok: (m) => console.log(`${c.green(sym.ok)} ${m}`),
  warn: (m) => console.log(`${c.yellow(sym.warn)} ${m}`),
  err: (m) => console.error(`${c.red(sym.err)} ${m}`),
  hint: (m) => console.log(c.gray(`  ${m}`)),
};

/** A section heading: blank line, bold title, optional gray note. */
export function heading(title, note) {
  console.log('');
  console.log(`  ${c.bold(title)}${note ? `  ${c.gray(note)}` : ''}`);
}

/**
 * Align rows of cells into columns. Cells may be coloured; widths are measured
 * on visible text. The last column is truncated to the terminal width.
 */
export function table(rowsIn, { indent = 4, gap = 2 } = {}) {
  if (!rowsIn.length) return [];
  const n = Math.max(...rowsIn.map((r) => r.length));
  const widths = Array.from({ length: n - 1 }, (_, i) =>
    Math.max(...rowsIn.map((r) => (r[i] === undefined ? 0 : width(r[i])))),
  );
  return rowsIn.map((r) => {
    let line = ' '.repeat(indent);
    for (let i = 0; i < r.length; i++) {
      const cell = r[i] ?? '';
      if (i < r.length - 1) line += pad(cell, widths[i]) + ' '.repeat(gap);
      else line += truncate(cell, Math.max(8, columns() - width(line) - 1));
    }
    return line.trimEnd();
  });
}

const FRAMES = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];

/**
 * Run `fn` behind a spinner. Resolves to fn's result; on throw, marks the line
 * failed and rethrows. Off a terminal it prints one line when done.
 */
export async function task(text, fn) {
  const tty = process.stdout.isTTY;
  let label = text;
  let i = 0;
  const started = Date.now();
  const secs = () => {
    const s = Math.round((Date.now() - started) / 1000);
    return s >= 3 ? c.gray(` ${s}s`) : '';
  };
  const draw = () => {
    cursor.column0();
    cursor.clearLine();
    process.stdout.write(`${c.cyan(FRAMES[i++ % FRAMES.length])} ${label}${secs()}`);
  };
  let timer = null;
  if (tty) {
    cursor.hide();
    draw();
    timer = setInterval(draw, 80);
  }
  const end = (mark, msg) => {
    if (timer) clearInterval(timer);
    if (tty) {
      cursor.column0();
      cursor.clearLine();
      cursor.show();
    }
    console.log(`${mark} ${msg}`);
  };
  try {
    const result = await fn({ update: (t) => (label = t) });
    end(c.green(sym.ok), label + secs());
    return result;
  } catch (err) {
    end(c.red(sym.err), label);
    throw err;
  }
}

/** Human "in 83 days" / "expired 2 days ago" with a colour by urgency. */
export function daysLeft(date) {
  if (!date) return c.gray('unknown');
  const days = Math.floor((date.getTime() - Date.now()) / 86_400_000);
  if (days < 0) return c.red(`expired ${-days}d ago`);
  const text = days > 730 ? `${Math.round(days / 365)} years left` : `${days} days left`;
  if (days < 14) return c.red(text);
  if (days < 30) return c.yellow(text);
  return c.green(text);
}

/** Shorten an absolute path for display: ./rel inside cwd, ~ for home. */
export function shorten(p, home = process.env.HOME || '/root') {
  if (!p) return '';
  if (p.startsWith(home + '/')) return '~' + p.slice(home.length);
  return p;
}
