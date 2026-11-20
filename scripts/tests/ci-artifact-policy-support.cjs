'use strict';
// Render only the independently guarded transport extension in the legacy
// artifact contract's vocabulary. Product commands, conditions and policies
// remain byte-for-byte visible to main's existing negative controls.
const { checkWorkflow, storageAction } = require('../ci-network-policy.cjs');
const pattern = /^      - [\s\S]*?(?=^      - |^  [A-Za-z0-9_-]+:|$(?![\s\S]))/gm;
function legacyArtifactPolicy(text) {
  if (!text.includes('PINVOU_CI_NETWORK_NODE_OPTIONS')) return text;
  checkWorkflow(text);
  return text.replace(pattern, step => {
    if (/^        run: node scripts\/ci-network-node-options\.cjs$/m.test(step)) return '';
    if (/^        run: node scripts\/ci-artifact-node-options\.cjs --network/m.test(step)) {
      return step.replace('ci-artifact-node-options.cjs --network', 'ci-artifact-node-options.cjs')
        .replace(/^        timeout-minutes: 1\n/m, '');
    }
    if (!storageAction(step)) return step;
    const isUpload = /^        uses: actions\/upload-artifact@/m.test(step);
    const diagnostics = /^      - name: (?:Windows atomic lifecycle receipts|Preserve Windows release contract process receipts)$/m.test(step);
    return step.replace("        env:\n          NODE_OPTIONS: ${{ env.PINVOU_CI_NETWORK_NODE_OPTIONS }}\n          PATH: ${{ env.PINVOU_CI_NETWORK_PATH }}\n          PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1'\n",
      isUpload ? '        env:\n' + (diagnostics ? "          PINVOU_CI_ARTIFACT_DIAGNOSTICS: '1'\n" : '')
        + '          NODE_OPTIONS: ${{ env.PINVOU_CI_ARTIFACT_NODE_OPTIONS }}\n' : '');
  });
}
module.exports = { legacyArtifactPolicy };
if (require.main === module) {
  let input = '';
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', chunk => { input += chunk; if (input.length > 2 * 1024 * 1024) process.exit(1); });
  process.stdin.on('end', () => {
    try { console.log(JSON.stringify(legacyArtifactPolicy(JSON.parse(input)))); }
    catch { console.error('Unreviewed CI transport policy'); process.exitCode = 1; }
  });
}
