// Install layout: everything lives under one directory (default ~/.llm-router).
//   <root>/bin/llm-router     the binary
//   <root>/config.yaml        router configuration (references ${ENV} names)
//   <root>/.env               secret values (chmod 600, never committed)
//   <root>/downloads/         temp download area
//   <root>/version.json       { version, installedAt, url }
//   <root>/llm-router.pid     pid of the detached server (when started via `run`)
//   <root>/logs/llm-router.log  server stdout/stderr (JSON log lines)
'use strict';

const os = require('node:os');
const path = require('node:path');

function rootDir(override) {
  if (override) return path.resolve(override);
  if (process.env.LLM_ROUTER_HOME) return path.resolve(process.env.LLM_ROUTER_HOME);
  return path.join(os.homedir(), '.llm-router');
}

function pathsFor(root) {
  const r = rootDir(root);
  return {
    root: r,
    binDir: path.join(r, 'bin'),
    bin: path.join(r, 'bin', 'llm-router'),
    config: path.join(r, 'config.yaml'),
    env: path.join(r, '.env'),
    downloads: path.join(r, 'downloads'),
    versionFile: path.join(r, 'version.json'),
    pidFile: path.join(r, 'llm-router.pid'),
    logFile: path.join(r, 'logs', 'llm-router.log'),
  };
}

module.exports = { rootDir, pathsFor };
