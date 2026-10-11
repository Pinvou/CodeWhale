// Deployment inputs are private operator configuration, never logged or inferred.
const { lstatSync, readFileSync } = require('node:fs');
const { resolve, dirname } = require('node:path');

const limits = {
  MAX_ROOMS: 2000, ROOM_CREATE_LIMIT: 20, ROOM_CREATE_WINDOW_MS: 60000,
  MAX_PAYLOAD_BYTES: 4194304, MAX_WS_CONNECTIONS: 5000,
  WS_CONNECT_LIMIT: 120, WS_CONNECT_WINDOW_MS: 60000, WS_AUTH_TIMEOUT_MS: 10000,
};
try {
  if (!process.argv[2]) throw new Error();
  const path = resolve(process.argv[2]);
  if (!lstatSync(path).isFile()) throw new Error();
  for (let current = path; ; current = dirname(current)) {
    if (lstatSync(current).isSymbolicLink()) throw new Error();
    if (dirname(current) === current) break;
  }
  const entries = new Map();
  let section = false;
  for (const raw of readFileSync(path, 'utf8').split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#') || line.startsWith(';')) continue;
    if (line === '[Service]' && !section) { section = true; continue; }
    const match = /^Environment=([A-Z_]+)=([A-Za-z0-9.,]+)$/.exec(line);
    // Instance addresses have one authority: the protected controller env.
    // Reject proxy (and all other unknown) keys rather than shadowing it.
    if (!section || !match || !Object.hasOwn(limits, match[1]) || entries.has(match[1])) throw new Error();
    entries.set(match[1], match[2]);
  }
  if (entries.size !== Object.keys(limits).length) throw new Error();
  for (const [name, limit] of Object.entries(limits)) {
    const value = entries.get(name);
    if (!/^[1-9][0-9]*$/.test(value ?? '') || Number(value) > limit) throw new Error();
    // Rate windows are not quantity caps: shrinking them weakens protection.
    if (name.endsWith('_WINDOW_MS') && Number(value) !== limit) throw new Error();
  }
} catch {
  console.error('deploy_hardening_configuration_invalid');
  process.exitCode = 64;
}
