'use strict';
// Runs before Rust setup, including on Windows listeners whose inherited
// environment predates the server repair. This never replaces toolchains.
const path = require('node:path');
const os = require('node:os');
const { spawnSync } = require('node:child_process');

const cargo = process.env.CARGO_HOME || path.join(os.homedir(), '.cargo');
const cache = process.env.PINVOU_CI_DOWNLOAD_CACHE || path.join(
  process.env.RUNNER_TOOL_CACHE || process.env.RUNNER_TEMP || os.tmpdir(), 'pinvou-verified-downloads-v1');
const commands = process.platform === 'win32' ? ['python', 'python3'] : ['python3', 'python'];
let result;
for (const executable of commands) {
  result = spawnSync(executable, [path.join(__dirname, 'ci-network-environment.py'),
    '--cargo-home', cargo, '--cache', cache, '--dist-cache'], { stdio: 'inherit', shell: false, timeout: 30000 });
  if (result.error?.code !== 'ENOENT') break;
}
if (result.error || result.status !== 0) {
  console.error('CI Rust network setup failed; no source or certificate bypass was applied');
  process.exit(1);
}
