// Interactive setup wizard: walks the user through server, credentials,
// logical models and timeouts, then writes config.yaml + .env.
'use strict';

const ui = require('./ui');
const gen = require('./configgen');

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const DUR_RE = /^\d+(ms|s|m|h)$/;

const vPort = (s) => (Number.isInteger(Number(s)) && Number(s) >= 1 && Number(s) <= 65535 ? null : 'port must be a number 1-65535');
const vName = (s) => (NAME_RE.test(s) ? null : 'use letters, digits, . _ - (must start with a letter or digit)');
const vDur = (s) => (DUR_RE.test(s) ? null : 'duration like 120s, 30s, 5m, 2h');
const vUrl = (s) => (/^https?:\/\//.test(s) ? null : 'must be a full URL starting with http:// or https://');

// ---------------------------------------------------------------------------
// Interactive wizard
// ---------------------------------------------------------------------------

async function wizard(existing) {
  const { c, ask, askSecret, confirm, choice, banner, step, ok } = ui;
  const d = existing || {};

  banner('llm-router setup', 'configure your LLM router in ~1 minute');

  // -- 1. server -----------------------------------------------------------
  step(1, 4, 'Server');
  const server = {
    host: await ask('  Host to listen on', { def: d.server?.host || '127.0.0.1', validate: null }),
    port: await ask('  Port', { def: String(d.server?.port || 8080), validate: vPort }),
  };
  server.auth = d.server?.auth !== undefined ? await confirm('  Require client auth (Bearer API key)?', d.server.auth) : await confirm('  Require client auth (Bearer API key)?', true);
  if (server.auth) {
    const have = d.server?.apiKey;
    server.apiKey = have
      ? (await confirm('  Keep existing API key?', true) ? have : await genKeyPrompt())
      : await genKeyPrompt();
  } else {
    server.apiKey = '';
  }
  ok(`server on ${c.cyan(server.host + ':' + server.port)}${server.auth ? ' with auth' : ' (no auth)'}`);

  // -- 2. credentials ------------------------------------------------------
  step(2, 4, 'Upstream credentials');
  const credentials = d.credentials ? [...d.credentials] : [];
  for (;;) {
    const add = credentials.length === 0 ? true : await confirm('  Add another credential?', false);
    if (!add) break;
    credentials.push(await credentialPrompt(credentials));
  }
  if (credentials.length === 0) throw new Error('at least one credential is required');
  ok(`${credentials.length} credential(s): ${credentials.map((x) => x.name).join(', ')}`);

  // -- 3. models -----------------------------------------------------------
  step(3, 4, 'Logical models');
  const models = d.models ? [...d.models] : [];
  for (;;) {
    const add = models.length === 0 ? true : await confirm('  Add another model?', false);
    if (!add) break;
    models.push(await modelPrompt(credentials, models));
  }
  if (models.length === 0) throw new Error('at least one model is required');
  ok(`${models.length} model(s): ${models.map((x) => x.name).join(', ')}`);

  // -- 4. defaults ---------------------------------------------------------
  step(4, 4, 'Timeouts (defaults for all models)');
  const defaults = {
    timeout: await ask('  Overall request budget (timeout)', { def: d.defaults?.timeout || '120s', validate: vDur }),
    rerouteTimeout: await ask('  First-response deadline (reroute_timeout)', { def: d.defaults?.rerouteTimeout || '10s', validate: vDur }),
    cooldown: await ask('  Backend cooldown after failure', { def: d.defaults?.cooldown || '30s', validate: vDur }),
  };

  const data = { server, defaults, credentials, models };
  if (await confirm('  Looks good — write config?', true)) {
    return data;
  }
  throw new Error('cancelled');
}

async function genKeyPrompt() {
  const { c, ask, askSecret, confirm } = ui;
  if (await confirm('  Auto-generate an API key?', true)) {
    const key = await ask('  Generated key (press Enter to use)', { def: gen.genApiKey() });
    return key;
  }
  return askSecret('  API key');
}

async function credentialPrompt(existing) {
  const { c, ask, askSecret, choice } = ui;
  const kind = await choice('  Provider type', [
    { value: 'openai', label: 'OpenAI (api.openai.com)' },
    { value: 'local', label: 'Local / OpenAI-compatible (vLLM, Ollama, LiteLLM, …)' },
    { value: 'azure', label: 'Azure OpenAI / Azure AI Foundry' },
  ], 'openai');

  const used = new Set(existing.map((x) => x.name));
  const defName = kind === 'openai' ? 'openai' : kind === 'azure' ? 'azure' : 'local';
  let name = await ask('  Credential name', {
    def: defName,
    validate: (s) => (vName(s) || (used.has(s) ? 'name already in use' : null)),
  });

  const cred = { name, kind };
  if (kind === 'azure') {
    cred.endpoint = await ask('  Resource endpoint', { def: '', validate: vUrl });
    cred.apiVersion = await ask('  API version', { def: '2024-10-21' });
  } else {
    const defUrl = kind === 'openai' ? 'https://api.openai.com/v1' : '';
    cred.baseUrl = await ask('  Base URL', { def: defUrl, validate: kind === 'local' ? vUrl : (s) => (s === '' || vUrl(s)) });
    if (cred.baseUrl === '') cred.baseUrl = undefined;
  }
  cred.key = await askSecret(`  ${kind === 'azure' ? 'Azure' : 'API'} key for "${name}"`);
  if (!cred.key) throw new Error('API key is required');
  return cred;
}

async function modelPrompt(credentials, existing) {
  const { c, ask, choice, confirm } = ui;
  const used = new Set(existing.map((x) => x.name));
  const name = await ask('  Model name (what clients request)', {
    def: 'chat',
    validate: (s) => (vName(s) || (used.has(s) ? 'name already in use' : null)),
  });
  const domain = await ask('  Domain / app group', { def: 'chat', validate: vName });
  const mode = await choice('  Routing mode', [
    { value: 'fallback', label: 'fallback — try backends in order, reroute on failure' },
    { value: 'round_robin', label: 'round_robin — distribute across healthy backends' },
  ], 'fallback');

  const backends = [];
  for (;;) {
    const credName = await choice('  Backend credential', credentials.map((x) => ({ value: x.name, label: `${x.name} (${x.kind})` })), credentials[0].name);
    const model = await ask('  Upstream model / deployment name', { def: '', validate: vName });
    backends.push({ credential: credName, model });
    if (!(await confirm('  Add another backend?', false))) break;
  }

  const m = { name, domain, mode, backends };
  if (await confirm('  Override timeouts for this model?', false)) {
    m.timeout = await ask('  timeout', { def: '', validate: (s) => (s === '' ? null : vDur(s)) });
    m.rerouteTimeout = await ask('  reroute_timeout', { def: '', validate: (s) => (s === '' ? null : vDur(s)) });
    m.cooldown = await ask('  cooldown', { def: '', validate: (s) => (s === '' ? null : vDur(s)) });
    if (m.timeout === '') m.timeout = undefined;
    if (m.rerouteTimeout === '') m.rerouteTimeout = undefined;
    if (m.cooldown === '') m.cooldown = undefined;
  }
  return m;
}

// ---------------------------------------------------------------------------
// Non-interactive builder (from CLI flags)
// ---------------------------------------------------------------------------

// parseCredSpec("name:kind:url:key") — kind: openai|local|azure.
// For azure, url is the endpoint. URL may contain colons (scheme/host:port);
// the key is the last field and must not contain ":".
function parseCredSpec(spec) {
  const parts = spec.split(':');
  if (parts.length < 4) throw new Error(`--cred needs name:kind:url:key (got: ${spec})`);
  const name = parts[0];
  const kind = parts[1];
  const key = parts[parts.length - 1];
  const url = parts.slice(2, -1).join(':');
  if (!['openai', 'local', 'azure'].includes(kind)) throw new Error(`--cred kind must be openai|local|azure (got: ${kind})`);
  if (!NAME_RE.test(name)) throw new Error(`--cred: bad name "${name}"`);
  if (!/^https?:\/\//.test(url)) throw new Error(`--cred: url must start with http(s):// (got: ${url})`);
  if (!key) throw new Error('--cred: key is required (and must not contain ":")');
  const cred = { name, kind, key };
  if (kind === 'azure') cred.endpoint = url;
  else cred.baseUrl = url;
  return cred;
}

// parseModelSpec("name:domain:mode:cred:model")
function parseModelSpec(spec) {
  const parts = spec.split(':');
  if (parts.length < 5) throw new Error(`--model needs name:domain:mode:cred:model (got: ${spec})`);
  const [name, domain, mode, cred] = parts;
  const model = parts.slice(4).join(':');
  if (!['fallback', 'round_robin'].includes(mode)) throw new Error(`--model mode must be fallback|round_robin (got: ${mode})`);
  return { name, domain, mode, backends: [{ credential: cred, model }] };
}

function fromFlags(flags) {
  const credentials = (flags.cred || []).map(parseCredSpec);
  const models = (flags.model || []).map(parseModelSpec);
  if (credentials.length === 0) throw new Error('non-interactive init needs at least one --cred name:kind:url:key');
  if (models.length === 0) throw new Error('non-interactive init needs at least one --model name:domain:mode:cred:model');
  const auth = flags.auth !== undefined ? flags.auth : false;
  return {
    server: {
      host: flags.host || '127.0.0.1',
      port: Number(flags.port || 8080),
      auth,
      apiKey: auth ? flags.apiKey || gen.genApiKey() : '',
    },
    defaults: {
      timeout: flags.timeout || '120s',
      rerouteTimeout: flags.rerouteTimeout || '10s',
      cooldown: flags.cooldown || '30s',
    },
    credentials,
    models,
  };
}

module.exports = { wizard, fromFlags };
