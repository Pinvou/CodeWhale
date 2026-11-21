'use strict';
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');

const run = process.env.GITHUB_RUN_ID;
const attempt = process.env.GITHUB_RUN_ATTEMPT;
const commit = process.env.GITHUB_SHA;
const os = process.env.RUNNER_OS;
if (!/^\d+$/.test(run || '') || !/^\d+$/.test(attempt || '') || !/^[a-f0-9]{40}$/.test(commit || '') || !/^(Linux|Windows|macOS)$/.test(os || '')) {
  throw new Error('Invalid network probe identity');
}
const root = path.join(process.env.RUNNER_TEMP, `ci-network-probe-${run}-${attempt}`);
const cache = path.join(root, 'cache');
const artifact = path.join(root, 'artifact');
const downloaded = path.join(root, 'downloaded');
const expected = Buffer.from(JSON.stringify({ schema: 1, run, attempt, commit, os }) + '\n');
const hash = bytes => crypto.createHash('sha256').update(bytes).digest('hex');

switch (process.argv[2]) {
  case 'create':
    for (const directory of [cache, artifact, downloaded]) fs.mkdirSync(directory, { recursive: true });
    for (const directory of [cache, artifact]) fs.writeFileSync(path.join(directory, 'probe.json'), expected);
    for (const [key, value] of Object.entries({ cache, artifact, downloaded })) {
      if (/[\r\n]/.test(value)) throw new Error('Invalid probe output path');
      fs.appendFileSync(process.env.GITHUB_OUTPUT, `${key}=${value}\n`);
    }
    break;
  case 'remove-cache-file':
    fs.unlinkSync(path.join(cache, 'probe.json'));
    break;
  case 'verify':
    for (const [transport, directory] of [['cache', cache], ['artifact', downloaded]]) {
      const actual = fs.readFileSync(path.join(directory, 'probe.json'));
      if (!actual.equals(expected)) throw new Error(`${transport} returned different tested bytes`);
      console.log(JSON.stringify({ phase: 'verified', transport, run, attempt, commit, bytes: actual.length, sha256: hash(actual) }));
    }
    break;
  default:
    throw new Error('Unknown network probe operation');
}
