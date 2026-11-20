'use strict';
const fs = require('node:fs');
const path = require('node:path');

function artifactNodeOptions(existing = '', diagnostics = false) {
  if (/[\r\n]/.test(existing)) throw new Error('Multiline Node options are not supported');
  let options = existing;
  if (diagnostics) {
    const { preloadOptions } = require('./ci-artifact-upload-diagnostics.cjs');
    options = preloadOptions(options, path.join(__dirname, 'ci-artifact-upload-diagnostics.cjs'));
  }
  // This stays inside Node's existing TLS >=1.2 policy. Explicit incompatible
  // minimums are retained and rejected by Node, never silently lowered here.
  return `${options} --tls-max-v1.2`.trim();
}
module.exports = { artifactNodeOptions };

if (require.main === module) {
  if (process.argv.slice(2).some(arg => !['--diagnostics', '--network'].includes(arg))) throw new Error('Unknown artifact options argument');
  const options = artifactNodeOptions(process.env.NODE_OPTIONS, process.argv.includes('--diagnostics'));
  // A separate value is applied only to the following artifact action. Do not
  // set job-wide NODE_OPTIONS, which would also change business test processes.
  fs.appendFileSync(process.env.GITHUB_ENV, `PINVOU_CI_ARTIFACT_NODE_OPTIONS=${options}\n`, 'utf8');
  if (process.argv.includes('--network')) {
    // Keep the existing artifact entry and its required success evidence.
    // The common extension supplies diagnostics and archive utilities only
    // to this Action; legacy callers retain the original single-value API.
    const { networkPath } = require('./ci-network-node-options.cjs');
    fs.appendFileSync(process.env.GITHUB_ENV,
      `PINVOU_CI_NETWORK_NODE_OPTIONS=${artifactNodeOptions(process.env.NODE_OPTIONS, true)}\n`
      + `PINVOU_CI_NETWORK_PATH=${networkPath(process.env.PATH)}\n`, 'utf8');
  }
}
