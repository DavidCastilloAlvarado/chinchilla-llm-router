// doctor: verify the install (binary, config, running server).
'use strict';

const fs = require('node:fs');
const http = require('node:http');
const { execFileSync } = require('node:child_process');

const { c, ok, fail, info } = require('./ui');

function checkBinary(p) {
  if (!fs.existsSync(p.bin)) {
    fail(`binary not found at ${p.bin}`);
    info(`fix: npx chinchilla-llm-router install`);
    return false;
  }
  let version = '?';
  try {
    version = execFileSync(p.bin, ['-version'], { stdio: ['ignore', 'pipe', 'pipe'] }).toString().trim();
  } catch {
    fail(`binary at ${p.bin} is not executable`);
    return false;
  }
  ok(`binary: ${version}`);
  return true;
}

function checkConfig(p) {
  if (!fs.existsSync(p.config)) {
    fail(`config not found at ${p.config}`);
    info('fix: npx chinchilla-llm-router init');
    return false;
  }
  if (!fs.existsSync(p.env)) {
    fail(`.env not found at ${p.env} (config ${'${VAR}'} refs will expand to empty)`);
    return false;
  }
  try {
    const out = execFileSync(p.bin, ['-config', p.config, '-check'], {
      stdio: ['ignore', 'pipe', 'pipe'],
      cwd: p.root, // so ./.env is found
    }).toString();
    ok(`config: ${p.config} (valid)`);
    const line = out.split('\n').find((l) => l.includes('"msg":"config ok"'));
    if (line) {
      try {
        const j = JSON.parse(line);
        info(`models: ${j.models}, credentials: ${j.credentials}`);
      } catch {
        /* non-JSON line; ignore */
      }
    }
    return true;
  } catch (err) {
    fail('config is invalid');
    const out = (err.stdout || '').toString().split('\n').filter(Boolean).pop();
    if (out) info(out);
    return false;
  }
}

function checkServer(p) {
  // Parse host:port from the server: block of config.yaml (best effort, no YAML dep).
  const text = fs.readFileSync(p.config, 'utf8');
  const block = (text.match(/^server:[ \t]*\n((?:[ \t]+.*\n?)*)/m) || [])[1] || text;
  const host = (block.match(/^[ \t]+host:[ \t]*["']?([\w.\-]+)["']?/m) || [])[1] || '127.0.0.1';
  const port = Number((block.match(/^[ \t]+port:[ \t]*(\d+)/m) || [])[1] || 8080);
  const h = host === '0.0.0.0' ? '127.0.0.1' : host;

  return new Promise((resolve) => {
    const req = http.get({ host: h, port, path: '/healthz', timeout: 1500 }, (res) => {
      res.resume();
      if (res.statusCode === 200) {
        ok(`server: running at http://${h}:${port} (healthz ok)`);
        resolve(true);
      } else {
        fail(`server at http://${h}:${port} returned HTTP ${res.statusCode}`);
        resolve(false);
      }
    });
    req.on('timeout', () => req.destroy(new Error('timeout')));
    req.on('error', () => {
      fail(`server: not reachable at http://${h}:${port}`);
      info(`start it: npx chinchilla-llm-router run   (or: ${p.bin} -config ${p.config})`);
      resolve(false);
    });
  });
}

async function doctor(p) {
  console.log('');
  console.log(c.bold('  llm-router doctor'));
  console.log(c.gray('  ' + '─'.repeat(24)));
  const a = checkBinary(p);
  const b = a && checkConfig(p);
  const d = b ? await checkServer(p) : false;
  console.log('');
  if (a && b && d) {
    ok(c.green('all good — llm-router is installed, configured and running'));
    return 0;
  }
  if (a && b) {
    ok(c.green('install and config are good — start the server: ' + c.cyan('npx chinchilla-llm-router run')));
    return 0;
  }
  fail('issues found — see above');
  return 1;
}

module.exports = { doctor };
