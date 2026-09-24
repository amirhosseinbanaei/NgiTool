#!/usr/bin/env node
import { main, ExitError } from '../src/main.mjs';
import { cursor, log, c } from '../src/ui.mjs';

const restore = () => cursor.show();
process.on('exit', restore);
process.on('SIGINT', () => {
  restore();
  process.exit(130);
});
process.on('SIGTERM', () => {
  restore();
  process.exit(143);
});

try {
  process.exitCode = await main(process.argv.slice(2));
} catch (err) {
  restore();
  if (err instanceof ExitError) {
    process.exitCode = err.code;
  } else {
    log.err(err?.message || String(err));
    if (process.env.DEBUG) console.error(c.gray(err?.stack || ''));
    process.exitCode = 1;
  }
}
