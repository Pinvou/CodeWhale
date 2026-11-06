import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';
import { isMainModule } from '../../../scripts/account-platform-cli-entry.mjs';

const groupName = 'account-platform.approval-authorization';
const chart = new URL('../helm/account-platform/', import.meta.url);
const templateScope = 'compose/approval-relay-rules';

export function composeApprovalRules(rendered, inputScope, project) {
  assert.match(project, /^[a-z0-9][a-z0-9_-]*$/, 'provide the resolved Compose project name');
  const documents = YAML.parseAllDocuments(rendered);
  assert.ok(documents.every(document => document.errors.length === 0), 'Helm rules YAML could not be parsed');
  const groups = documents.flatMap(document => {
    const item = document.toJS();
    return item?.kind === 'PrometheusRule' ? item.spec.groups.filter(group => group.name === groupName) : [];
  });
  assert.equal(groups.length, 1, 'expected exactly one rendered authorization rule group');
  assert.equal(groups[0].rules.length, 4, 'expected all authorization alert rules');
  const oldSelector = `approval_scope=${JSON.stringify(inputScope)}`;
  for (const rule of groups[0].rules) {
    assert.ok(rule.expr.includes(oldSelector) && rule.labels?.approval_scope === inputScope,
      'rendered rule lost its exact selector or delivery label');
    rule.expr = rule.expr.replaceAll(oldSelector, `approval_scope=${JSON.stringify(project)}`);
    rule.labels.approval_scope = project;
  }
  return YAML.stringify({ groups });
}

if (isMainModule(import.meta.url)) {
  try {
    const [project, replicas, ...extra] = process.argv.slice(2);
    assert.ok(!extra.length && typeof project === 'string' && /^[a-z0-9][a-z0-9_-]*$/.test(project)
      && /^[1-9]\d*$/.test(replicas) && Number.isSafeInteger(Number(replicas)), 'provide the Compose project name and a positive replica count');
    // Reuse the deployed Helm expressions. The test values are render-only
    // inputs and are never installed or included in the output.
    const result = spawnSync('helm', ['template', 'approval-relay-rules', fileURLToPath(chart),
      '--namespace', 'compose', '--show-only', 'templates/prometheus-rules.yaml',
      '-f', fileURLToPath(new URL('values-model-token-test.yaml', chart)),
      '--set', 'observability.prometheusRulesEnabled=true',
      '--set', 'approvalRelay.requireReady=true', '--set', `replicas.eventRelay=${replicas}`],
    { encoding: 'utf8', windowsHide: true, timeout: 30_000 });
    assert.equal(result.status, 0, 'offline Helm render failed');
    process.stdout.write(composeApprovalRules(result.stdout, templateScope, project));
  } catch (error) {
    console.error(`APPROVAL_RELAY_RULE_RENDER_FAILED: ${error.message}`);
    process.exitCode = 1;
  }
}
