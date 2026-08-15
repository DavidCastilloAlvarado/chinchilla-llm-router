// Platform detection for binary downloads.
'use strict';

// target() -> { os, arch } or throws with a helpful message.
function target() {
  let os;
  if (process.platform === 'linux') os = 'linux';
  else if (process.platform === 'darwin') os = 'darwin';
  else if (process.platform === 'win32') {
    throw new Error(
      'Windows is not supported directly. Use WSL2 (Ubuntu) or the Docker image: docker pull llm-router'
    );
  } else {
    throw new Error(`unsupported platform: ${process.platform} (linux and darwin are supported)`);
  }
  let arch;
  if (process.arch === 'x64' || process.arch === 'amd64') arch = 'amd64';
  else if (process.arch === 'arm64' || process.arch === 'aarch64') arch = 'arm64';
  else throw new Error(`unsupported architecture: ${process.arch} (amd64 and arm64 are supported)`);
  return { os, arch };
}

module.exports = { target };
