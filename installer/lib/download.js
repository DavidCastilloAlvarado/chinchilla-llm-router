// Download + verify + extract the llm-router binary from GitHub releases.
// Asset layout per release (built by .github/workflows/release.yml):
//   llm-router_<ver>_<os>_<arch>.tar.gz   (contains a single file "llm-router")
//   checksums.txt                          (sha256 of every tarball)
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const https = require('node:https');
const http = require('node:http');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const pkg = require('../package.json');

// resolveVersion(explicit) -> "x.y.z" (no leading v).
function resolveVersion(explicit) {
  const v = explicit || process.env.LLM_ROUTER_VERSION || (pkg.llmRouter && pkg.llmRouter.version) || '0.1.0';
  return String(v).replace(/^v/, '');
}

// releaseBase() -> e.g. https://github.com/DavidCastilloAlvarado/chinchilla-llm-router
function releaseBase() {
  if (process.env.LLM_ROUTER_RELEASE_BASE) return process.env.LLM_ROUTER_RELEASE_BASE.replace(/\/+$/, '');
  return `https://github.com/${pkg.llmRouter.repo}`;
}

function assetName(version, os, arch) {
  return `llm-router_${version}_${os}_${arch}.tar.gz`;
}

function assetUrl(version, os, arch) {
  return `${releaseBase()}/releases/download/v${version}/${assetName(version, os, arch)}`;
}

// fetch(url, { redirects }) -> { status, body: Buffer } (small payloads only).
function fetchBuffer(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    const lib = url.startsWith('https:') ? https : http;
    const req = lib.get(url, (res) => {
      if ([301, 302, 303, 307, 308].includes(res.statusCode)) {
        res.resume();
        if (redirects <= 0) return reject(new Error(`too many redirects for ${url}`));
        const loc = res.headers.location;
        if (!loc) return reject(new Error(`redirect without location for ${url}`));
        const next = new URL(loc, url).toString();
        return resolve(fetchBuffer(next, redirects - 1));
      }
      const chunks = [];
      res.on('data', (d) => chunks.push(d));
      res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks) }));
      res.on('error', reject);
    });
    req.on('error', reject);
    req.setTimeout(15000, () => req.destroy(new Error(`timeout fetching ${url}`)));
  });
}

// download(url, dest, onProgress) -> dest. Streams to disk, follows redirects.
function download(url, dest, onProgress) {
  return new Promise((resolve, reject) => {
    const lib = url.startsWith('https:') ? https : http;
    const file = fs.createWriteStream(dest);
    let total = 0;
    const req = lib.get(url, (res) => {
      if ([301, 302, 303, 307, 308].includes(res.statusCode)) {
        res.resume();
        const loc = res.headers.location;
        if (!loc) return reject(new Error(`redirect without location for ${url}`));
        file.close();
        fs.unlinkSync(dest);
        return resolve(download(new URL(loc, url).toString(), dest, onProgress));
      }
      if (res.statusCode !== 200) {
        res.resume();
        file.close();
        fs.unlinkSync(dest);
        return reject(new Error(`download failed: HTTP ${res.statusCode} for ${url}`));
      }
      const len = Number(res.headers['content-length'] || 0);
      res.on('data', (d) => {
        total += d.length;
        if (onProgress && len) onProgress(total, len);
      });
      res.pipe(file);
      file.on('finish', () => file.close(() => resolve(dest)));
      file.on('error', (err) => {
        file.close();
        fs.unlinkSync(dest);
        reject(err);
      });
      res.on('error', reject);
    });
    req.on('error', reject);
  });
}

// verifyChecksum(tarball, version, os, arch) -> { verified: bool, note }.
// Best effort: if the release ships checksums.txt we verify; otherwise we
// note that verification was skipped.
async function verifyChecksum(tarball, version, os, arch) {
  const url = `${releaseBase()}/releases/download/v${version}/checksums.txt`;
  let res;
  try {
    res = await fetchBuffer(url);
  } catch {
    return { verified: false, note: 'checksums.txt not available — skipped verification' };
  }
  if (res.status !== 200) return { verified: false, note: 'checksums.txt not found — skipped verification' };
  const name = assetName(version, os, arch);
  const line = res.body
    .toString('utf8')
    .split('\n')
    .find((l) => l.trim().endsWith(name));
  if (!line) return { verified: false, note: `no checksum entry for ${name} — skipped verification` };
  const expected = line.trim().split(/\s+/)[0].toLowerCase();
  const actual = crypto.createHash('sha256').update(fs.readFileSync(tarball)).digest('hex');
  if (expected !== actual) throw new Error(`checksum mismatch for ${name} (expected ${expected}, got ${actual})`);
  return { verified: true, note: 'sha256 verified' };
}

// extract(tarball, destBin) — untar the single "llm-router" file and move it.
function extract(tarball, destBin) {
  const tmp = path.join(path.dirname(tarball), 'extract-' + Date.now());
  fs.mkdirSync(tmp, { recursive: true });
  try {
    execFileSync('tar', ['-xzf', tarball, '-C', tmp], { stdio: 'pipe' });
    const found = fs
      .readdirSync(tmp)
      .find((f) => f === 'llm-router' || f.endsWith('-llm-router'));
    if (!found) throw new Error('tarball does not contain the llm-router binary');
    fs.copyFileSync(path.join(tmp, found), destBin);
    fs.chmodSync(destBin, 0o755);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

module.exports = { resolveVersion, releaseBase, assetUrl, download, verifyChecksum, extract };
