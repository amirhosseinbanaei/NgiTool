// Interactive prompts on raw stdin: select, multiselect, input, confirm, paste.
// Every prompt redraws in place and collapses to one "✔ question › answer" line.
// Esc throws Cancelled (the caller goes back); Ctrl-C exits the program.

import readline from 'node:readline';
import { c, sym, cursor, columns, rows, truncate, pad, width } from './ui.mjs';

export class Cancelled extends Error {
  constructor() {
    super('cancelled');
  }
}

export const canPrompt = () => Boolean(process.stdin.isTTY && process.stdout.isTTY);

/** Turn a raw stdin chunk into a list of logical keys. */
export function decode(chunk) {
  const s = chunk.toString('utf8');
  const keys = [];
  let i = 0;

  while (i < s.length) {
    const ch = s[i];

    if (ch === '\x1b') {
      const rest = s.slice(i);
      const seq = rest.match(/^\x1b\[(\d*)([A-Z~])/) || rest.match(/^\x1bO([A-Z])/);
      if (seq) {
        const code = seq[2] ?? seq[1];
        const num = seq[2] ? seq[1] : '';
        const named = { A: 'up', B: 'down', C: 'right', D: 'left', H: 'home', F: 'end', Z: 'shift-tab' }[code];
        if (named) keys.push({ name: named });
        else if (code === '~') keys.push({ name: { 5: 'pageup', 6: 'pagedown', 1: 'home', 4: 'end', 3: 'delete' }[num] || 'unknown' });
        else keys.push({ name: 'unknown' });
        i += seq[0].length;
        continue;
      }
      keys.push({ name: 'escape' });
      i += 1;
      continue;
    }

    if (ch === '\r' || ch === '\n') keys.push({ name: 'enter' });
    else if (ch === '\x03') keys.push({ name: 'ctrl-c' });
    else if (ch === '\x04') keys.push({ name: 'ctrl-d' });
    else if (ch === '\x15') keys.push({ name: 'ctrl-u' });
    else if (ch === '\x17') keys.push({ name: 'ctrl-w' });
    else if (ch === '\x7f' || ch === '\b') keys.push({ name: 'backspace' });
    else if (ch === '\t') keys.push({ name: 'tab' });
    else if (ch >= ' ') keys.push({ name: 'char', value: ch });
    i += 1;
  }

  return keys;
}

const CANCEL = Symbol('cancel');
const done = (value, summary) => ({ done: true, value, summary });

const answered = (message, answer) => `${c.green(sym.ok)} ${message} ${c.gray('›')} ${c.cyan(answer)}`;
const asking = (message, hint) => `${c.cyan('?')} ${c.bold(message)}${hint ? `  ${c.gray(hint)}` : ''}`;

const frameHeight = (lines) =>
  lines.reduce((n, l) => n + Math.max(1, Math.ceil(Math.max(1, width(l)) / columns())), 0);

/** The shared raw-mode loop: draw frame(), feed keys to onKey() until it returns done/CANCEL. */
function run({ frame, onKey }) {
  if (!canPrompt()) {
    return Promise.reject(new Error('this step needs an interactive terminal — pass the value as a flag instead'));
  }
  return new Promise((resolve, reject) => {
    const input = process.stdin;
    let last = 0;
    let finished = false;

    const draw = () => {
      const lines = frame();
      cursor.up(last);
      cursor.column0();
      cursor.clearDown();
      process.stdout.write(lines.join('\n') + '\n');
      last = frameHeight(lines);
    };

    const cleanup = () => {
      finished = true;
      input.off('data', onData);
      process.stdout.off('resize', draw);
      input.setRawMode(false);
      input.pause();
      cursor.up(last);
      cursor.column0();
      cursor.clearDown();
      cursor.show();
    };

    const onData = (chunk) => {
      for (const key of decode(chunk)) {
        if (finished) return;
        if (key.name === 'ctrl-c') {
          cleanup();
          process.stdout.write(c.gray('Interrupted.') + '\n');
          process.exit(130);
        }
        const r = onKey(key);
        if (r === CANCEL) {
          cleanup();
          return reject(new Cancelled());
        }
        if (r && r.done) {
          cleanup();
          if (r.summary) process.stdout.write(r.summary + '\n');
          return resolve(r.value);
        }
      }
      draw();
    };

    input.setRawMode(true);
    input.resume();
    cursor.hide();
    input.on('data', onData);
    process.stdout.on('resize', draw);
    draw();
  });
}

/**
 * Pick one. choices: [{ value, label, hint, disabled: false | 'reason' }]
 * or { separator: 'Title' } for a non-selectable heading.
 */
export function select({ message, choices, initial, hint, filterable }) {
  const items = choices.filter(Boolean);
  const selectable = (it) => it && !it.separator && !it.disabled;
  if (!items.some(selectable)) return Promise.reject(new Error(`nothing to choose for "${message}"`));

  const canFilter = filterable ?? items.filter((it) => !it.separator).length > 7;
  let filter = '';
  const visible = () => {
    if (!filter) return items;
    const q = filter.toLowerCase();
    return items.filter((it) => !it.separator && `${it.label} ${it.hint || ''}`.toLowerCase().includes(q));
  };
  const firstSelectable = () => Math.max(0, visible().findIndex(selectable));

  let index = items.findIndex((it) => selectable(it) && it.value === initial);
  if (index < 0) index = firstSelectable();
  let offset = 0;
  const labelWidth = Math.min(34, Math.max(...items.filter((it) => !it.separator).map((it) => width(it.label))));

  const move = (d) => {
    const list = visible();
    if (!list.length) return;
    let i = index;
    for (let n = 0; n < list.length; n++) {
      i = (i + d + list.length) % list.length;
      if (selectable(list[i])) {
        index = i;
        return;
      }
    }
  };

  const frame = () => {
    const list = visible();
    const cols = columns();
    const lines = [asking(message, hint)];
    const maxVisible = Math.max(3, Math.min(list.length, rows() - 5));
    if (index >= list.length) index = firstSelectable();
    if (index < offset) offset = index;
    if (index >= offset + maxVisible) offset = index - maxVisible + 1;
    if (offset > Math.max(0, list.length - maxVisible)) offset = Math.max(0, list.length - maxVisible);
    // keep a separator visible when its first item is at the top
    if (offset > 0 && list[offset - 1]?.separator && index === offset) offset -= 1;

    if (!list.length) lines.push(`  ${c.yellow('no match for')} ${c.bold(filter)}`);
    const win = list.slice(offset, offset + maxVisible);
    win.forEach((it, i) => {
      if (it.separator) {
        lines.push(`  ${c.gray(it.separator)}`);
        return;
      }
      const active = offset + i === index;
      const pointer = active ? c.cyan(sym.pointer) : ' ';
      const raw = pad(it.label, labelWidth);
      const label = it.disabled ? c.gray(raw) : active ? c.cyan(c.bold(raw)) : raw;
      const note = it.disabled ? (typeof it.disabled === 'string' ? it.disabled : '') : it.hint || '';
      const head = `  ${pointer} ${label}  `;
      const room = cols - width(head) - 1;
      lines.push(head + (room > 6 ? (it.disabled ? c.gray(c.italic(truncate(note, room))) : c.gray(truncate(note, room))) : ''));
    });
    const more = list.length - offset - win.length;
    if (offset > 0 || more > 0) {
      lines.push(c.gray(`    ${offset > 0 ? `↑ ${offset} more` : ''}${offset > 0 && more > 0 ? '   ' : ''}${more > 0 ? `↓ ${more} more` : ''}`));
    }
    lines.push(
      filter
        ? `  ${c.cyan('filter')} ${filter}${c.inverse(' ')}  ${c.gray('esc clears')}`
        : c.gray(`  ↑↓ move · ↵ select · esc back${canFilter ? ' · type to filter' : ''}`),
    );
    return lines;
  };

  const onKey = (key) => {
    switch (key.name) {
      case 'up':
      case 'shift-tab':
        move(-1);
        break;
      case 'down':
      case 'tab':
        move(1);
        break;
      case 'pageup':
        for (let n = 0; n < 5; n++) move(-1);
        break;
      case 'pagedown':
        for (let n = 0; n < 5; n++) move(1);
        break;
      case 'home':
        index = firstSelectable();
        break;
      case 'end': {
        const list = visible();
        for (let i = list.length - 1; i >= 0; i--) if (selectable(list[i])) { index = i; break; }
        break;
      }
      case 'enter': {
        const it = visible()[index];
        if (selectable(it)) return done(it.value, answered(message, it.answer || strip2(it.label)));
        break;
      }
      case 'escape':
        if (!filter) return CANCEL;
        filter = '';
        index = firstSelectable();
        break;
      case 'backspace':
        filter = filter.slice(0, -1);
        index = firstSelectable();
        break;
      case 'char':
        if (canFilter) {
          filter += key.value;
          index = firstSelectable();
        } else if (key.value === 'k') move(-1);
        else if (key.value === 'j') move(1);
        else if (key.value === 'q') return CANCEL;
        break;
      default:
        break;
    }
    return null;
  };

  return run({ frame, onKey });
}

const strip2 = (s) => String(s).trim();

/** Pick several. Resolves to an array of values. */
export function multiselect({ message, choices, initial = [], hint }) {
  const items = choices.filter(Boolean);
  const selected = new Set(initial);
  let index = 0;
  let offset = 0;
  const labelWidth = Math.min(34, Math.max(12, ...items.map((it) => width(it.label))));

  const toggle = () => {
    const it = items[index];
    if (!it || it.disabled) return;
    selected.has(it.value) ? selected.delete(it.value) : selected.add(it.value);
  };

  const frame = () => {
    const cols = columns();
    const lines = [asking(message, hint)];
    const maxVisible = Math.max(3, Math.min(items.length, rows() - 5));
    if (index < offset) offset = index;
    if (index >= offset + maxVisible) offset = index - maxVisible + 1;
    const win = items.slice(offset, offset + maxVisible);
    win.forEach((it, i) => {
      const active = offset + i === index;
      const on = selected.has(it.value);
      const pointer = active ? c.cyan(sym.pointer) : ' ';
      const box = it.disabled ? c.gray('-') : on ? c.green(sym.checked) : c.gray(sym.unchecked);
      const raw = pad(it.label, labelWidth);
      const label = it.disabled ? c.gray(raw) : active ? c.cyan(c.bold(raw)) : raw;
      const badge = it.badge ? ` ${c.yellow(it.badge)}` : '';
      const head = `  ${pointer} ${box} ${label}${badge}  `;
      const note = it.disabled && typeof it.disabled === 'string' ? it.disabled : it.hint || '';
      const room = cols - width(head) - 1;
      lines.push(head + (room > 6 ? c.gray(truncate(note, room)) : ''));
    });
    const more = items.length - offset - win.length;
    if (offset > 0 || more > 0) {
      lines.push(c.gray(`    ${offset > 0 ? `↑ ${offset} more` : ''}${offset > 0 && more > 0 ? '   ' : ''}${more > 0 ? `↓ ${more} more` : ''}`));
    }
    lines.push(`  ${selected.size ? c.green(`${selected.size} selected`) : c.gray('nothing selected')}`);
    lines.push(c.gray('  ↑↓ move · space toggle · a all · n none · ↵ confirm · esc back'));
    return lines;
  };

  const onKey = (key) => {
    switch (key.name) {
      case 'up':
        index = Math.max(0, index - 1);
        break;
      case 'down':
        index = Math.min(items.length - 1, index + 1);
        break;
      case 'tab':
        toggle();
        index = Math.min(items.length - 1, index + 1);
        break;
      case 'enter': {
        const values = items.filter((it) => selected.has(it.value)).map((it) => it.value);
        return done(values, answered(message, values.length ? `${values.length} selected` : 'none'));
      }
      case 'escape':
        return CANCEL;
      case 'char':
        if (key.value === ' ') toggle();
        else if (key.value === 'a') {
          const all = items.filter((it) => !it.disabled);
          const allOn = all.every((it) => selected.has(it.value));
          for (const it of all) allOn ? selected.delete(it.value) : selected.add(it.value);
        } else if (key.value === 'n') selected.clear();
        else if (key.value === 'j') index = Math.min(items.length - 1, index + 1);
        else if (key.value === 'k') index = Math.max(0, index - 1);
        break;
      default:
        break;
    }
    return null;
  };

  return run({ frame, onKey });
}

/**
 * Free text. `defaultValue` is used when the answer is left empty (shown as a
 * placeholder); `validate(v)` returns an error string or nothing.
 */
export function input({ message, defaultValue = '', placeholder = '', hint, validate, mask = false, optional = false }) {
  let value = '';
  let error = '';
  const ghost = placeholder || defaultValue;

  const frame = () => {
    const shown = mask ? '•'.repeat(value.length) : value;
    const tail = value ? c.inverse(' ') : ghost ? c.inverse(ghost[0]) + c.gray(ghost.slice(1)) : c.inverse(' ');
    const lines = [`${c.cyan('?')} ${c.bold(message)} ${c.gray('›')} ${shown}${tail}`];
    if (error) lines.push(`  ${c.red(error)}`);
    else if (hint) lines.push(c.gray(`  ${hint}`));
    return lines;
  };

  const onKey = (key) => {
    switch (key.name) {
      case 'char':
        value += key.value;
        error = '';
        break;
      case 'backspace':
        value = value.slice(0, -1);
        error = '';
        break;
      case 'ctrl-u':
        value = '';
        break;
      case 'ctrl-w':
        value = value.replace(/\S*\s*$/, '');
        break;
      case 'tab':
        if (!value && ghost) value = ghost;
        break;
      case 'enter': {
        const v = value.trim() || defaultValue;
        if (!v && !optional) {
          error = 'required';
          break;
        }
        const problem = v ? validate?.(v) : null;
        if (problem) {
          error = problem;
          break;
        }
        return done(v, answered(message, mask && v ? '•'.repeat(8) : v || c.gray('skipped')));
      }
      case 'escape':
        return CANCEL;
      default:
        break;
    }
    return null;
  };

  return run({ frame, onKey });
}

/** Yes / no on one keystroke. */
export function confirm({ message, initial = true, hint }) {
  const frame = () => {
    const lines = [`${asking(message)} ${c.gray(initial ? '(Y/n)' : '(y/N)')}`];
    if (hint) lines.push(c.gray(`  ${hint}`));
    return lines;
  };
  const onKey = (key) => {
    const yes = done(true, answered(message, 'yes'));
    const no = done(false, answered(message, 'no'));
    if (key.name === 'enter') return initial ? yes : no;
    if (key.name === 'escape') return CANCEL;
    if (key.name === 'char' && /[yY]/.test(key.value)) return yes;
    if (key.name === 'char' && /[nN]/.test(key.value)) return no;
    return null;
  };
  return run({ frame, onKey });
}

/** Multi-line paste (PEM blocks). Ends at an empty line after an -----END marker, or Ctrl-D. */
export function paste({ message, hint = 'Paste it, then press Enter on an empty line.' }) {
  if (!canPrompt()) return Promise.reject(new Error('pasting needs an interactive terminal — use a file path instead'));
  console.log(asking(message));
  console.log(c.gray(`  ${hint}`));
  const rl = readline.createInterface({ input: process.stdin, terminal: false });
  const lines = [];
  return new Promise((resolve) => {
    rl.on('line', (line) => {
      if (!line.trim() && lines.some((l) => /-----END [A-Z ]+-----/.test(l))) rl.close();
      else lines.push(line);
    });
    rl.on('close', () => {
      process.stdin.pause();
      const text = lines.join('\n').trim();
      console.log(answered(message, `${lines.length} lines`));
      resolve(text ? text + '\n' : '');
    });
  });
}
