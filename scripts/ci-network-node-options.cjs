'use strict';

// Storage actions have independent Node processes (including post-job saves).
// Keep this value out of job-wide NODE_OPTIONS and business test processes.
const fs = require('node:fs');
const { artifactNodeOptions } = require('./ci-artifact-node-options.cjs');

function networkNodeOptions(existing = '') {
  return artifactNodeOptions(existing, true);
}

module.exports = { networkNodeOptions };

if (require.main === module) {
  if (process.argv.length !== 2) throw new Error('Unexpected CI transport argument');
  const options = networkNodeOptions(process.env.NODE_OPTIONS);
  fs.appendFileSync(process.env.GITHUB_ENV, `PINVOU_CI_NETWORK_NODE_OPTIONS=${options}\n`, 'utf8');
}
