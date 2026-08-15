// Daemon lifecycle for the installed router: pidfile, health probes,
// detached start, graceful stop. POSIX-first; best effort on Windows.
'use strict';

const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { spawn } = require('node:child_process');

// serverAddr(p) -> { host, port } — parse the server: block of config.yaml
// (best effort, no YAML dep). Same heuristic as doctor.
function serverAddr(p) {
  let host = '127.0.0.1';
  let port = 8080;
  try {
    const text = fs.readFileSync(p.config, 'utf8');
    const block = (text.match(/^server:[ \t]*\n((?:[ \t]+.*\n?)*)/m) || [])[1] || text;
    host = (block.match(/^[ \t]+host:[ \t]*["']?([\w.\-]+)["']?/m) || [])[1] || host;
    port = Number((block.match(/^[ \t]+port:[ \t]*(\d+)/m) || [])[1] || port);
  } catch {
    /* no config yet; defaults */
  }
  return { host: host === '0.0.0.0' ? '127.0.0.1' : host, port };
}

function readPid(p) {
  try {
    const n = Number(fs.readFileSync(p.pidFile, 'utf8').trim());
    return Number.isInteger(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err.code === 'EPERM'; // exists, but not owned by us
  }
}

// looksLikeRouter(pid) — best-effort guard against pid reuse (Linux /proc only).
function looksLikeRouter(pid) {
  if (process.platform !== 'linux') return true;
  try {
    return fs.readFileSync(`/proc/${pid}/cmdline`, 'utf8').includes('llm-router');
  } catch {
    return true;
  }
}

// healthOk(p) -> Promise<bool> — is something answering /healthz on the
// configured host:port?
function healthOk(p) {
  const { host, port } = serverAddr(p);
  return new Promise((resolve) => {
    const req = http.get({ host, port, path: '/healthz', timeout: 1500 }, (res) => {
      res.resume();
      resolve(res.statusCode === 200);
    });
    req.on('timeout', () => req.destroy(new Error('timeout')));
    req.on('error', () => resolve(false));
  });
}

// waitForHealth(p, timeoutMs) — poll until /healthz answers or the deadline.
function waitForHealth(p, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  return (async () => {
    for (;;) {
      if (await healthOk(p)) return true;
      if (Date.now() >= deadline) return false;
      await new Promise((r) => setTimeout(r, 200));
    }
  })();
}

// startDetached(p) -> { pid, logFile } — spawn the router in its own session
// (survives the installer exiting), stdio appended to the log file, pid
// recorded in the pidfile.
function startDetached(p) {
  if (!fs.existsSync(p.bin)) {
    throw new Error(`binary not found at ${p.bin} — run: npx chinchilla-llm-router install`);
  }
  fs.mkdirSync(path.dirname(p.logFile), { recursive: true });
  const fd = fs.openSync(p.logFile, 'a');
  const child = spawn(p.bin, ['-config', p.config], {
    cwd: p.root, // so ./.env is loaded from the install root
    detached: true,
    stdio: ['ignore', fd, fd],
    windowsHide: true,
  });
  child.unref();
  fs.writeFileSync(p.pidFile, String(child.pid) + '\n');
  return { pid: child.pid, logFile: p.logFile };
}

// stopServer(p) -> { stopped, pid, reason } — SIGTERM, wait for exit,
// SIGKILL if it lingers. Cleans up the pidfile.
async function stopServer(p, { timeoutMs = 10000 } = {}) {
  const pid = readPid(p);
  if (!pid) return { stopped: false, pid: null, reason: 'no-pidfile' };
  if (!pidAlive(pid) || !looksLikeRouter(pid)) {
    fs.rmSync(p.pidFile, { force: true });
    return { stopped: false, pid: null, reason: 'not-running' };
  }
  try {
    process.kill(pid, 'SIGTERM');
  } catch {
    /* already gone */
  }
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline && pidAlive(pid)) {
    await new Promise((r) => setTimeout(r, 100));
  }
  if (pidAlive(pid)) {
    try {
      process.kill(pid, 'SIGKILL');
    } catch {
      /* already gone */
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  fs.rmSync(p.pidFile, { force: true });
  return { stopped: true, pid, reason: null };
}

// logTail(file, lines) -> string — last N lines (for failure diagnostics).
function logTail(file, lines = 12) {
  try {
    return fs
      .readFileSync(file, 'utf8')
      .split('\n')
      .filter(Boolean)
      .slice(-lines)
      .join('\n');
  } catch {
    return '';
  }
}

module.exports = { serverAddr, readPid, pidAlive, looksLikeRouter, healthOk, waitForHealth, startDetached, stopServer, logTail };
