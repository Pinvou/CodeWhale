// Deployment inputs are private operator configuration, never logged or inferred.
const { lstatSync, readFileSync, readlinkSync, realpathSync } = require('node:fs');
const { resolve, dirname, posix } = require('node:path');
const { isIP } = require('node:net');

// Darwin's OS-owned temporary roots are aliases, not operator-selected links.
// Do not canonicalize arbitrary input paths: that would hide user symlinks.
function isTrustedSystemAlias(path, platform, linkTarget, canonicalPath) {
  const expected = path === '/tmp' ? '/private/tmp' : path === '/var' ? '/private/var' : null;
  return platform === 'darwin' && expected !== null
    && posix.resolve(posix.dirname(path), linkTarget) === expected
    && canonicalPath === expected;
}

function assertPlainAncestors(path) {
  for (let current = path; ; current = dirname(current)) {
    if (lstatSync(current).isSymbolicLink()) throw new Error();
    if (dirname(current) === current) break;
  }
}

function validateInputPath(input) {
  const path = resolve(input);
  if (!lstatSync(path).isFile()) throw new Error();
  for (let current = path; ; current = dirname(current)) {
    const stat = lstatSync(current);
    if (stat.isSymbolicLink()) {
      const canonical = realpathSync(current);
      if (stat.uid !== 0 || !isTrustedSystemAlias(current, process.platform, readlinkSync(current), canonical)) {
        throw new Error();
      }
      // The fixed /private target and every ancestor must also be plain.
      assertPlainAncestors(canonical);
    }
    if (dirname(current) === current) break;
  }
  return path;
}

const limits = {
  MAX_ROOMS: 2000, ROOM_CREATE_LIMIT: 20, ROOM_CREATE_WINDOW_MS: 60000,
  MAX_PAYLOAD_BYTES: 4194304, MAX_WS_CONNECTIONS: 5000,
  WS_CONNECT_LIMIT: 120, WS_CONNECT_WINDOW_MS: 60000, WS_AUTH_TIMEOUT_MS: 10000,
};
function validateConfiguration(input) {
  if (!input) throw new Error();
  const path = validateInputPath(input);
  const entries = new Map();
  let section = false;
  for (const raw of readFileSync(path, 'utf8').split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#') || line.startsWith(';')) continue;
    if (line === '[Service]' && !section) { section = true; continue; }
    const match = /^Environment=([A-Z_]+)=([A-Za-z0-9.,]+)$/.exec(line);
    if (!section || !match || entries.has(match[1])) throw new Error();
    entries.set(match[1], match[2]);
  }
  const proxyNames = ['PINVOU_REMOTE_ALLOWED_PROXY_IPS', 'PINVOU_REMOTE_TRUSTED_PROXY_IPS'];
  const sets = proxyNames.map((name) => {
    const value = entries.get(name);
    if (!value) throw new Error();
    const addresses = value.split(',');
    if (addresses.some((ip) => isIP(ip) !== 4 || ip === '0.0.0.0' || ip === '255.255.255.255')) throw new Error();
    return [...new Set(addresses)].sort().join(',');
  });
  if (sets[0] !== sets[1] || entries.size !== Object.keys(limits).length + 2) throw new Error();
  for (const [name, limit] of Object.entries(limits)) {
    const value = entries.get(name);
    if (!/^[1-9][0-9]*$/.test(value ?? '') || Number(value) > limit) throw new Error();
    // Rate windows are not quantity caps: shrinking them weakens protection.
    if (name.endsWith('_WINDOW_MS') && Number(value) !== limit) throw new Error();
  }
}

module.exports = { isTrustedSystemAlias };
if (require.main === module) {
  try {
    validateConfiguration(process.argv[2]);
  } catch {
    console.error('deploy_hardening_configuration_invalid');
    process.exitCode = 64;
  }
}
