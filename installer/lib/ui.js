// Terminal UI helpers: colors, prompts (plain + masked), confirm, choice,
// spinner. Zero dependencies; colors disabled when not a TTY or NO_COLOR.
'use strict';

const readline = require('node:readline');

const isTTY = Boolean(process.stdout.isTTY);
const noColor = process.env.NO_COLOR || !isTTY;

function wrap(code) {
  return (s) => (noColor ? String(s) : `\x1b[${code}m${s}\x1b[0m`);
}

const c = {
  bold: wrap('1'),
  dim: wrap('2'),
  italic: wrap('3'),
  red: wrap('31'),
  green: wrap('32'),
  yellow: wrap('33'),
  blue: wrap('34'),
  magenta: wrap('35'),
  cyan: wrap('36'),
  gray: wrap('90'),
};

// ---------------------------------------------------------------------------
// Input layer.
//
// A single readline interface feeds a line queue. Lines are captured as they
// arrive — even in bursts (e.g. piped through a PTY) — and consumed one by
// one by ask/confirm/choice. This avoids the classic readline pitfall where
// lines buffered between two question() calls are lost across the async gap.
// ---------------------------------------------------------------------------

let rl = null;
let eof = false;
let selfClosing = false;
const lineQueue = [];
const lineWaiters = [];

function onLine(line) {
  const w = lineWaiters.shift();
  if (w) w(line);
  else lineQueue.push(line);
}

function getRL() {
  if (!rl) {
    rl = readline.createInterface({
      input: process.stdin,
      output: process.stdout,
      terminal: Boolean(process.stdin.isTTY),
    });
    rl.on('line', onLine);
    rl.on('close', () => {
      const wasSelf = selfClosing;
      selfClosing = false;
      rl = null;
      if (!wasSelf) {
        // stdin hit EOF: fail any pending question.
        eof = true;
        while (lineWaiters.length) lineWaiters.shift()(null);
      }
    });
  }
  return rl;
}

function closeRL() {
  if (rl) {
    selfClosing = true;
    rl.close();
    rl = null;
  }
}

// nextLine() -> string | null (null on EOF).
function nextLine() {
  getRL(); // ensure the interface is up and capturing lines
  if (lineQueue.length) return Promise.resolve(lineQueue.shift());
  if (eof) return Promise.resolve(null);
  return new Promise((resolve) => lineWaiters.push(resolve));
}

async function readLine() {
  const line = await nextLine();
  if (line === null) throw new Error('EOF');
  return line;
}

// ask(question, { def, validate }) -> string.
// Empty input returns the default. validate may return an error string.
async function ask(question, { def = '', validate } = {}) {
  const suffix = def !== '' ? c.gray(` [${def}]`) : '';
  for (;;) {
    process.stdout.write(c.bold(question) + suffix + ' ');
    const answer = (await readLine()).trim();
    if (!process.stdin.isTTY) process.stdout.write('\n');
    const value = answer === '' ? def : answer;
    if (value === '') continue;
    if (validate) {
      const err = validate(value);
      if (err) {
        console.log(c.yellow(`  ! ${err}`));
        continue;
      }
    }
    return value;
  }
}

// askSecret(question) -> string. Masked input (* per char). Falls back to a
// plain line read when stdin is not a TTY or when lines are already buffered
// (piped/burst input — raw mode would wait for characters that readline has
// already consumed as lines).
async function askSecret(question) {
  const suffix = c.gray(' (input hidden)');
  process.stdout.write(c.bold(question) + suffix + ' ');
  if (lineQueue.length > 0) {
    const answer = lineQueue.shift();
    process.stdout.write('\n');
    return answer.trim();
  }
  if (!process.stdin.isTTY) {
    const answer = (await readLine()).trim();
    process.stdout.write('\n');
    return answer;
  }
  // Take stdin out of readline's hands for raw masked input (readline would
  // echo the characters and queue the secret as a line).
  closeRL();
  const stdin = process.stdin;
  stdin.setRawMode(true);
  stdin.resume();
  let input = '';
  await new Promise((resolve) => {
    const onData = (buf) => {
      for (const ch of String(buf)) {
        if (ch === '\r' || ch === '\n') {
          process.stdout.write('\n');
          stdin.removeListener('data', onData);
          resolve(input);
          return;
        }
        if (ch === '\u007f' || ch === '\b') {
          if (input.length > 0) {
            input = input.slice(0, -1);
            process.stdout.write('\b \b');
          }
          continue;
        }
        if (ch === '\u0003' || ch === '\u0004') {
          process.stdout.write('\n');
          process.exit(130);
        }
        input += ch;
        process.stdout.write('*');
      }
    };
    stdin.on('data', onData);
  });
  stdin.setRawMode(false);
  return input;
}

// confirm(question, def) -> boolean.
async function confirm(question, def = true) {
  const hint = def ? c.gray('[Y/n]') : c.gray('[y/N]');
  for (;;) {
    process.stdout.write(c.bold(question) + ' ' + hint + ' ');
    const answer = (await readLine()).trim().toLowerCase();
    if (!process.stdin.isTTY) process.stdout.write('\n');
    if (answer === '') return def;
    if (['y', 'yes'].includes(answer)) return true;
    if (['n', 'no'].includes(answer)) return false;
    console.log(c.yellow('  ! please answer y or n'));
  }
}

// choice(question, options, def) -> option value.
// options: [{ value, label? }] — label defaults to value.
async function choice(question, options, def) {
  const lines = options.map((o, i) => {
    const label = o.label || o.value;
    const marker = o.value === def ? c.green('●') : c.gray('○');
    return `  ${marker} ${i + 1}) ${label}`;
  });
  console.log(c.bold(question));
  console.log(lines.join('\n'));
  const defIndex = options.findIndex((o) => o.value === def);
  for (;;) {
    process.stdout.write(c.gray(`select [${defIndex + 1}]`) + ' ');
    const answer = (await readLine()).trim();
    if (!process.stdin.isTTY) process.stdout.write('\n');
    if (answer === '') return def;
    const n = Number(answer);
    if (Number.isInteger(n) && n >= 1 && n <= options.length) return options[n - 1].value;
    const byValue = options.find((o) => o.value === answer);
    if (byValue) return byValue.value;
    console.log(c.yellow(`  ! pick 1..${options.length}`));
  }
}

// Spinner: start(label) -> stop(). No-op output when not a TTY.
function spinner(label) {
  const frames = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];
  let i = 0;
  let text = label;
  const timer = isTTY
    ? setInterval(() => {
        process.stdout.write(`\r\x1b[2K${c.cyan(frames[(i = (i + 1) % frames.length)])} ${text}`);
      }, 80)
    : null;
  if (!isTTY) process.stdout.write(label + ' ... ');
  return {
    set(t) {
      text = t;
    },
    stop() {
      if (timer) clearInterval(timer);
      if (isTTY) process.stdout.write('\r\x1b[2K');
    },
  };
}

function banner(title, subtitle) {
  const inner = 44;
  console.log('');
  console.log(c.bold(c.cyan('  ┌' + '─'.repeat(inner) + '┐')));
  console.log(c.bold(c.cyan('  │')) + c.bold(`  ${title.padEnd(inner - 4)}  `) + c.bold(c.cyan('│')));
  if (subtitle) console.log(c.bold(c.cyan('  │')) + c.gray(`  ${subtitle.padEnd(inner - 4)}  `) + c.bold(c.cyan('│')));
  console.log(c.bold(c.cyan('  └' + '─'.repeat(inner) + '┘')));
  console.log('');
}

function step(n, total, title) {
  console.log('');
  console.log(c.bold(c.magenta(`  [${n}/${total}] `)) + c.bold(title));
  console.log(c.gray('  ' + '─'.repeat(Math.max(0, 34 - title.length))));
}

function ok(msg) {
  console.log('  ' + c.green('✓') + ' ' + msg);
}

function info(msg) {
  console.log('  ' + c.gray('·') + ' ' + msg);
}

function warn(msg) {
  console.log('  ' + c.yellow('!') + ' ' + msg);
}

function fail(msg) {
  console.log('  ' + c.red('✗') + ' ' + msg);
}

module.exports = { c, ask, askSecret, confirm, choice, spinner, banner, step, ok, info, warn, fail, closeRL };
