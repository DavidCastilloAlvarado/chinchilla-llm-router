// Install the downloaded binary into the llm-router home and wire up PATH.
'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const { target } = require('./platform');
const { pathsFor } = require('./paths');
const dl = require('./download');
const ui = require('./ui');

function installedVersion(p) {
  try {
    return JSON.parse(fs.readFileSync(p.versionFile, 'utf8')).version || null;
  } catch {
    return null;
  }
}

// installBinary({ version, force, root }) -> { version, installed: bool }
async function installBinary({ version, force = false, root } = {}) {
  const { c, spinner, ok, info } = ui;
  const p = pathsFor(root);
  const want = dl.resolveVersion(version);
  const have = installedVersion(p);

  if (have === want && fs.existsSync(p.bin) && !force) {
    ok(`llm-router ${c.bold(have)} already installed at ${p.bin}`);
    return { version: have, installed: false };
  }

  const { os: osName, arch } = target();
  const url = dl.assetUrl(want, osName, arch);
  fs.mkdirSync(p.downloads, { recursive: true });
  const tarball = path.join(p.downloads, path.basename(url));

  const sp = spinner(`downloading llm-router ${want} (${osName}/${arch})`);
  let last = -1;
  try {
    await dl.download(url, tarball, (done, total) => {
      const pct = Math.floor((done / total) * 100);
      if (pct !== last) {
        last = pct;
        sp.set(`downloading llm-router ${want} (${osName}/${arch}) — ${pct}% (${(done / 1024 / 1024).toFixed(1)} MiB)`);
      }
    });
  } finally {
    sp.stop();
  }

  const sum = await dl.verifyChecksum(tarball, want, osName, arch);
  if (sum.verified) ok('checksum verified (sha256)');
  else info(sum.note);

  const sp2 = spinner('installing binary');
  try {
    fs.mkdirSync(p.binDir, { recursive: true });
    dl.extract(tarball, p.bin);
    fs.rmSync(tarball, { force: true });
    fs.writeFileSync(
      p.versionFile,
      JSON.stringify({ version: want, installedAt: new Date().toISOString(), url }, null, 2) + '\n'
    );
  } finally {
    sp2.stop();
  }
  ok(`installed ${c.bold('llm-router ' + want)} → ${p.bin}`);
  return { version: want, installed: true };
}

// ensurePath(p) -> { inPath: bool, updated: [files] }
// Appends the bin dir to the user's shell rc file(s) when missing.
function ensurePath(p) {
  const binDir = p.binDir;
  if ((process.env.PATH || '').split(':').includes(binDir)) return { inPath: true, updated: [] };

  const home = os.homedir();
  const shell = (process.env.SHELL || '').toLowerCase();
  const exportLine = `export PATH="${binDir}:$PATH"`;
  const fishLine = `set -gx PATH ${binDir} $PATH`;
  const marker = '# llm-router';
  const candidates = [];
  if (shell.includes('zsh')) candidates.push(path.join(home, '.zshrc'));
  if (shell.includes('bash')) candidates.push(path.join(home, '.bashrc'), path.join(home, '.bash_profile'));
  if (shell.includes('fish')) candidates.push(path.join(home, '.config', 'fish', 'config.fish'));
  candidates.push(path.join(home, '.profile'));

  const updated = [];
  for (const rc of candidates) {
    if (!fs.existsSync(rc)) continue;
    const content = fs.readFileSync(rc, 'utf8');
    if (content.includes(binDir)) continue;
    const line = rc.includes('fish') ? fishLine : exportLine;
    fs.appendFileSync(rc, `\n${marker}\n${line}\n`);
    updated.push(rc);
    if (updated.length >= 2) break; // don't touch more rc files than needed
  }
  return { inPath: false, updated };
}

// printPathHint(p) — tell the user how to get the binary on PATH now.
function printPathHint(p) {
  const { c, info } = ui;
  info(`add to PATH:  ${c.cyan(`export PATH="${p.binDir}:$PATH"`)}`);
  info(`or run directly: ${c.cyan(p.bin)}`);
}

// runBinary(args, p) — exec the installed binary (used by `run`).
function runBinary(args, p) {
  if (!fs.existsSync(p.bin)) {
    ui.fail(`binary not found at ${p.bin} — run: npx chinchilla-llm-router install`);
    process.exit(1);
  }
  try {
    execFileSync(p.bin, args, { stdio: 'inherit', cwd: p.root }); // ./.env is loaded from cwd
  } catch (err) {
    process.exit(err.status || 1);
  }
}

module.exports = { installBinary, ensurePath, printPathHint, runBinary, installedVersion };
