'use strict';
// Runs before Rust setup, including on Windows listeners whose inherited
// environment predates the server repair. This never replaces toolchains.
const path = require('node:path');
const os = require('node:os');
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');

if (process.argv.includes('--report')) {
  try {
    const state = process.env.PINVOU_CI_RUST_DIST_STATE;
    if (!state) throw new Error('no owned listener');
    const file = path.join(path.dirname(state), 'statistics.json');
    if (fs.statSync(file).size > 65536) throw new Error('oversized receipt');
    const receipt = JSON.parse(fs.readFileSync(file, 'utf8'));
    const allowed = new Set(['phase', 'host', 'attempt', 'tls_policy', 'http_code', 'http_connect',
      'time_namelookup', 'time_connect', 'time_appconnect', 'time_starttransfer',
      'size_download', 'num_redirects', 'exit', 'failure_stage', 'bytes', 'sha256', 'error_type', 'reason', 'errno']);
    const events = Array.isArray(receipt.events) ? receipt.events.slice(-32).map(event =>
      Object.fromEntries(Object.entries(event).filter(([key, value]) => allowed.has(key)
        && (typeof value === 'number' && Number.isFinite(value)
          || typeof value === 'string' && /^[A-Za-z0-9_.>=-]{1,80}$/.test(value))))) : [];
    console.log('[ci-rust-dist] ' + JSON.stringify({ phase: 'snapshot', events }));
  } catch {
    console.log('[ci-rust-dist] {"phase":"snapshot-unavailable"}');
  }
  process.exit(0); // Diagnostic reporting must retain the primary step verdict.
}

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
