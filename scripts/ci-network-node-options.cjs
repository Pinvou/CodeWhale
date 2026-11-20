'use strict';

// Storage actions have independent Node processes (including post-job saves).
// Keep this value out of job-wide NODE_OPTIONS and business test processes.
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { artifactNodeOptions } = require('./ci-artifact-node-options.cjs');

function networkNodeOptions(existing = '') {
  return artifactNodeOptions(existing, true);
}

function gitArchiveTools() {
  const git = spawnSync('git', ['--exec-path'], { encoding: 'utf8', timeout: 5000, shell: false });
  if (git.status !== 0) return null;
  const root = path.resolve(git.stdout.trim(), '../../..');
  const folder = path.join(root, 'usr', 'bin');
  return fs.existsSync(path.join(folder, 'tar.exe')) ? folder : null;
}

function networkPath(existing = '', platform = process.platform, findTools = gitArchiveTools) {
  if (/[\r\n]/.test(existing)) throw new Error('Multiline process path is not supported');
  if (platform !== 'win32') return existing;
  const folders = existing.split(';').map(value => value.replace(/^"|"$/g, ''));
  if (folders.some(folder => folder && fs.existsSync(path.join(folder, 'tar.exe')))) return existing;
  const tools = findTools();
  if (!tools || /[\r\n]/.test(tools)) throw new Error('Windows cache archiver unavailable; repair the Runner Git tools');
  // Only the storage Action sees the Git archive utilities. Do not modify
  // GITHUB_PATH or put MSYS find/sort/etc. ahead of business test tools.
  return `${tools};${existing}`;
}

module.exports = { networkNodeOptions, networkPath };

if (require.main === module) {
  if (process.argv.length !== 2) throw new Error('Unexpected CI transport argument');
  const options = networkNodeOptions(process.env.NODE_OPTIONS);
  const scopedPath = networkPath(process.env.PATH);
  fs.appendFileSync(process.env.GITHUB_ENV,
    `PINVOU_CI_NETWORK_NODE_OPTIONS=${options}\nPINVOU_CI_NETWORK_PATH=${scopedPath}\n`, 'utf8');
}
