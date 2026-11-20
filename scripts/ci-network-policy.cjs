'use strict';

const fs = require('node:fs');
const path = require('node:path');

const stepPattern = /^      - [\s\S]*?(?=^      - |^  [A-Za-z0-9_-]+:|$(?![\s\S]))/gm;
const options = '${{ env.PINVOU_CI_NETWORK_NODE_OPTIONS }}';
const families = /^(actions\/(upload-artifact|download-artifact|attest-build-provenance|cache(?:\/(restore|save))?|setup-node|setup-go|setup-python)|swatinem\/rust-cache)$/;

function storageAction(step) {
  const action = step.match(/^ {6}(?:- uses:|  uses:) ([^\s]+)\s*$/m)?.[1]?.toLowerCase().split('@')[0];
  if (action && /cache|(?:upload|download)-artifact/.test(action) && !families.test(action)) {
    throw new Error(`Unreviewed storage action: ${action}`);
  }
  return action && families.test(action);
}

function field(step, name) {
  return step.match(new RegExp(`^ {8}${name}: (.*)$`, 'm'))?.[1];
}

function checkWorkflow(source, filename = 'workflow') {
  const text = source.replaceAll('\r\n', '\n');
  const steps = [...text.matchAll(stepPattern)].map(m => m[0]);
  let count = 0;
  const fail = message => { throw new Error(`${filename}: ${message}`); };
  for (let i = 0; i < steps.length; i++) {
    const step = steps[i];
    if (/^node scripts\/ci-artifact-node-options\.cjs(?: |$)/.test(field(step, 'run') || '')) {
      if (!/^ {8}uses: actions\/upload-artifact@/m.test(steps[i + 1] || '')) {
        fail('artifact preparation is duplicated or unpaired');
      }
    }
    if (/^ {6}(?:- uses:|  uses:) dtolnay\/rust-toolchain@/m.test(step)
        || /^          rustup toolchain install /m.test(step)) {
      if (field(steps[i - 1] || '', 'run') !== 'node scripts/ci-rust-network.cjs') fail('Rust setup lacks mirror retirement');
      if (field(steps[i - 1], 'if') !== field(step, 'if')) fail('Rust source preparation conditions differ');
      const report = steps[i + 1] || '';
      if (field(report, 'run') !== 'node scripts/ci-rust-network.cjs --report'
          || field(report, 'if') !== '${{ always() }}' || field(report, 'timeout-minutes') !== '1') {
        fail('Rust setup lacks bounded diagnostics on failure');
      }
    }
    if (!storageAction(step)) continue;
    count++;
    const prep = steps[i - 1] || '';
    const artifact = /^ {8}uses: actions\/upload-artifact@/m.test(step);
    const command = field(prep, 'run');
    if (artifact ? !/^node scripts\/ci-artifact-node-options\.cjs --network(?: --diagnostics)?$/.test(command || '')
        : command !== 'node scripts/ci-network-node-options.cjs') fail('storage action lacks immediate transport preparation');
    if (field(prep, 'timeout-minutes') !== '1') fail('transport preparation must remain bounded');
    const unknownPrep = prep.split('\n').filter(line => line.trim() && !line.trimStart().startsWith('#') &&
      !/^      - name: [^\r\n]+$/.test(line) &&
      !/^        (?:if|continue-on-error|timeout-minutes|working-directory|run): [^\r\n]+$/.test(line));
    if (unknownPrep.length || [...prep.matchAll(/^        run:/gm)].length !== 1) fail('transport preparation contains unreviewed work');
    if (field(prep, 'working-directory') !== '${{ github.workspace }}') fail('transport helper must use the repository root');
    if (field(prep, 'if') !== field(step, 'if')) fail('transport and action conditions differ');
    if (artifact ? field(prep, 'continue-on-error') !== undefined
        : field(prep, 'continue-on-error') !== field(step, 'continue-on-error')) fail('transport changes the action failure policy');
    const assignments = [...step.matchAll(/^ {10}NODE_OPTIONS: (.*)$/gm)];
    if (assignments.length !== 1 || assignments[0][1] !== options) fail('storage action lacks inherited scoped TLS options');
    if (!/^ {10}PATH: \$\{\{ env\.PINVOU_CI_NETWORK_PATH \}\}$/m.test(step)) fail('storage action lacks the scoped archive tools path');
    if (!/^ {10}PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1'$/m.test(step)) fail('storage action lacks passive stage diagnostics');
  }
  const remainder = text.replace(stepPattern, step => storageAction(step) ? '' : step);
  if (/(?:NODE_OPTIONS:.*PINVOU_CI_NETWORK_NODE_OPTIONS|PATH:.*PINVOU_CI_NETWORK_PATH|--tls-max-v1\.2)/.test(remainder)) fail('transport options leaked into a business step or job environment');
  if (/NODE_TLS_REJECT_UNAUTHORIZED:|--tls-min-v1\.[01]\b/.test(text)) fail('TLS verification/minimum must not be weakened');
  // Valid workflow layout is checked by actionlint; a future alternate step
  // layout must not silently bypass this scanner.
  const activeUses = text.split('\n').filter(line => /^\s+(?:- )?uses:/.test(line) && !line.trimStart().startsWith('#'));
  const declared = activeUses.filter(line => {
    const action = line.split('uses:')[1].trim().split('@')[0].toLowerCase();
    if (/cache|(?:upload|download)-artifact/.test(action) && !families.test(action)) fail('unreviewed storage action layout');
    return families.test(action);
  }).length;
  if (declared !== count) fail('unsupported storage step layout');
  return count;
}

module.exports = { checkWorkflow, storageAction };
if (require.main === module) {
  const root = path.resolve(__dirname, '..', '.github', 'workflows');
  let count = 0;
  for (const name of fs.readdirSync(root).filter(name => /\.ya?ml$/.test(name))) {
    count += checkWorkflow(fs.readFileSync(path.join(root, name), 'utf8'), name);
  }
  console.log(`CI storage transport policy: ${count} action entries checked`);
}
