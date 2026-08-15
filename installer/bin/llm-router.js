#!/usr/bin/env node
// chinchilla-llm-router — installer + setup wizard for llm-router.
//
//   npx chinchilla-llm-router              install binary + interactive setup
//   npx chinchilla-llm-router install      install the binary only
//   npx chinchilla-llm-router init         run the config wizard (or use flags)
//   npx chinchilla-llm-router doctor       check binary / config / running server
//   npx chinchilla-llm-router run          start the router detached (restarts if running)
//   npx chinchilla-llm-router stop         stop the detached router
//   npx chinchilla-llm-router status       show whether the router is running
//   npx chinchilla-llm-router version      print versions
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const ui = require('../lib/ui');
const { pathsFor } = require('../lib/paths');
const inst = require('../lib/install');
const gen = require('../lib/configgen');
const wiz = require('../lib/wizard');
const doctor = require('../lib/doctor');
const { execFileSync } = require('node:child_process');

const pkg = require('../package.json');

const HELP = `
${ui.c.bold('chinchilla-llm-router')} — install & configure llm-router

${ui.c.bold('usage:')}
  npx chinchilla-llm-router [command] [flags]

${ui.c.bold('commands:')}
  setup      (default) install the binary, then run the interactive setup wizard
  install    install the binary only (skips if already installed)
  init       generate config.yaml + .env — interactively, or with flags:
               --cred name:kind:url:key        (repeatable; kind: openai|local|azure)
               --model name:domain:mode:cred:upstream-model   (repeatable)
               --port 8080 --host 127.0.0.1
               --auth --api-key sk-...         (client auth; key auto-generated if omitted)
               --timeout 120s --reroute-timeout 10s --cooldown 30s
  doctor     check binary, config validity and running server
  run        start the router in the background (detached) and return immediately;
             restarts it first if an instance is already running
               --foreground   block the terminal instead (old behavior, for debugging)
  stop       stop the background router (started by \`run\`)
  status     show whether the background router is running
  version    print installer + installed binary versions

${ui.c.bold('flags:')}
  --dir <dir>        install root (default: ~/.llm-router, or $LLM_ROUTER_HOME)
  --version <v>      binary version to install (default: pinned in package.json)
  --force            reinstall even if the version is already installed
  -h, --help         show this help
  -v, --version-flag print versions and exit

${ui.c.bold('environment:')}
  LLM_ROUTER_HOME          install root override
  LLM_ROUTER_VERSION       binary version override
  LLM_ROUTER_RELEASE_BASE  release base URL override (self-hosted releases)
`;

function parseArgs(argv) {
  const flags = { cred: [], model: [] };
  const pos = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`flag ${a} needs a value`);
      return v;
    };
    switch (a) {
      case '-h':
      case '--help':
        flags.help = true;
        break;
      case '-v':
      case '--version-flag':
        flags.versionFlag = true;
        break;
      case '--dir':
        flags.dir = next();
        break;
      case '--force':
        flags.force = true;
        break;
      case '--foreground':
        flags.foreground = true;
        break;
      case '--auth':
        flags.auth = true;
        break;
      case '--no-auth':
        flags.auth = false;
        break;
      case '--port':
        flags.port = next();
        break;
      case '--host':
        flags.host = next();
        break;
      case '--api-key':
        flags.apiKey = next();
        break;
      case '--timeout':
        flags.timeout = next();
        break;
      case '--reroute-timeout':
        flags.rerouteTimeout = next();
        break;
      case '--cooldown':
        flags.cooldown = next();
        break;
      case '--cred':
        flags.cred.push(next());
        break;
      case '--model':
        flags.model.push(next());
        break;
      default:
        if (a.startsWith('--version=')) flags.version = a.slice('--version='.length);
        else if (a === '--version') flags.version = next();
        else if (a.startsWith('-') && a.length > 1) throw new Error(`unknown flag: ${a} (see --help)`);
        else pos.push(a);
    }
  }
  return { cmd: pos[0] || 'setup', flags };
}

function printVersion(p) {
  console.log(`chinchilla-llm-router ${pkg.version} (installer)`);
  const v = inst.installedVersion(p);
  console.log(v ? `llm-router ${v} (installed at ${p.bin})` : 'llm-router not installed yet');
}

function validateConfig(p) {
  try {
    execFileSync(p.bin, ['-config', p.config, '-check'], { stdio: ['ignore', 'pipe', 'pipe'], cwd: p.root });
    ui.ok('config validated by the router itself');
    return true;
  } catch (err) {
    ui.fail('config validation failed:');
    console.log((err.stdout || err.stderr || '').toString().trim().split('\n').map((l) => '    ' + l).join('\n'));
    return false;
  }
}

function finishSetup(p, data) {
  const { c, ok, info } = ui;
  const files = gen.writeConfig(p, data);
  ok(`wrote ${c.cyan(files.config)}`);
  ok(`wrote ${c.cyan(files.env)} (chmod 600)`);
  if (fs.existsSync(p.bin)) validateConfig(p);

  const { inPath, updated } = inst.ensurePath(p);
  if (!inPath) {
    if (updated.length) ok(`added ${p.binDir} to PATH in: ${updated.join(', ')}`);
    inst.printPathHint(p);
  } else {
    ok(`${p.binDir} is already on PATH`);
  }

  console.log('');
  ui.banner('done', 'next steps');
  info(`start the router:   ${c.cyan('npx chinchilla-llm-router run')}   (detached; restarts if already running)`);
  info(`health check:       ${c.cyan('npx chinchilla-llm-router doctor')}`);
  info(`stop the router:    ${c.cyan('npx chinchilla-llm-router stop')}`);
  const port = data.server.port;
  const auth = data.server.auth ? ` -H "Authorization: Bearer <ROUTER_API_KEY>"` : '';
  info(`try it:             ${c.cyan(`curl -s http://127.0.0.1:${port}/v1/models${auth ? '' : ''}`)}`);
  info(`example request:    ${c.cyan(`curl -s http://127.0.0.1:${port}/v1/chat/completions -d '{"model":"${data.models[0].name}","messages":[{"role":"user","content":"hi"}]}'`)}`);
  console.log('');
}

async function main() {
  let parsed;
  try {
    parsed = parseArgs(process.argv.slice(2));
  } catch (err) {
    console.error(ui.c.red('error: ' + err.message));
    process.exit(2);
  }
  const { cmd, flags } = parsed;
  const p = pathsFor(flags.dir);

  if (flags.help) {
    console.log(HELP);
    return;
  }
  if (flags.versionFlag) {
    printVersion(p);
    return;
  }

  switch (cmd) {
    case 'version':
      printVersion(p);
      return;

    case 'install': {
      const r = await inst.installBinary({ version: flags.version, force: flags.force, root: flags.dir });
      if (r.installed) {
        const { inPath, updated } = inst.ensurePath(p);
        if (!inPath) {
          if (updated.length) ui.ok(`added ${p.binDir} to PATH in: ${updated.join(', ')}`);
          inst.printPathHint(p);
        }
      }
      return;
    }

    case 'init': {
      let data;
      const interactive = flags.cred.length === 0 && flags.model.length === 0;
      if (interactive) {
        if (!process.stdin.isTTY) {
          console.error(ui.c.red('error: interactive init needs a TTY — pass --cred/--model flags instead (see --help)'));
          process.exit(2);
        }
        if (fs.existsSync(p.config)) {
          const keep = await ui.confirm(`config already exists at ${p.config} — overwrite?`, false);
          if (!keep) {
            ui.info('keeping existing config; nothing to do');
            return;
          }
        }
        data = await wiz.wizard();
      } else {
        data = wiz.fromFlags(flags);
      }
      finishSetup(p, data);
      return;
    }

    case 'doctor': {
      process.exitCode = await doctor.doctor(p);
      return;
    }

    case 'run': {
      await inst.cmdRun(p, { foreground: flags.foreground });
      return;
    }

    case 'stop': {
      await inst.cmdStop(p);
      return;
    }

    case 'status': {
      await inst.cmdStatus(p);
      return;
    }

    case 'setup':
    default: {
      if (cmd !== 'setup') {
        console.error(ui.c.red(`unknown command: ${cmd} (see --help)`));
        process.exit(2);
      }
      // 1. binary
      await inst.installBinary({ version: flags.version, force: flags.force, root: flags.dir });
      // 2. wizard (interactive only)
      if (!process.stdin.isTTY) {
        console.error(ui.c.red('error: setup needs a TTY for the wizard — use `init` with flags or `install` (see --help)'));
        process.exit(2);
      }
      if (fs.existsSync(p.config)) {
        const redo = await ui.confirm(`config already exists at ${p.config} — reconfigure?`, false);
        if (!redo) {
          ui.info('keeping existing config');
          ui.ok('start the router: ' + ui.c.cyan('npx chinchilla-llm-router run'));
          return;
        }
      }
      const data = await wiz.wizard();
      finishSetup(p, data);
      return;
    }
  }
}

main()
  .then(() => ui.closeRL())
  .catch((err) => {
    ui.closeRL();
    if (err && err.message === 'cancelled') {
      console.log(ui.c.yellow('setup cancelled — no files were written'));
      process.exit(1);
    }
    console.error(ui.c.red('error: ' + (err && err.message ? err.message : String(err))));
    process.exit(1);
  });
