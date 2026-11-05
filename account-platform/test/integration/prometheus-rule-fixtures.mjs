// Prepare promtool fixtures from the actual Helm-rendered PrometheusRule.
// No live Prometheus, API, credentials, or production data is accessed.
import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import YAML from 'yaml';

const [renderedFile, outputDirectory] = process.argv.slice(2);
assert.ok(renderedFile && outputDirectory, 'provide Helm-rendered YAML and a new test output directory');
const rendered = YAML.parseAllDocuments(await readFile(renderedFile, 'utf8'));
assert.ok(rendered.every(document => document.errors.length === 0), 'rendered YAML must parse without errors');
const resources = rendered.map(document => document.toJS()).filter(resource => resource?.kind === 'PrometheusRule');
assert.equal(resources.length, 1, 'render exactly one chart PrometheusRule');
const groups = resources[0].spec.groups;
const missing = groups.flatMap(group => group.rules).filter(rule => rule.alert === 'PinvouAccountPlatformEventRelayMissing');
assert.equal(missing.length, 1, 'render the existing missing-relay alert once');
assert.equal(missing[0].for, '5m');
assert.equal(missing[0].labels.severity, 'p1');
const fixture = await readFile(new URL('../observability/event-relay-missing.test.yaml', import.meta.url), 'utf8');
// promtool accepts an empty tests list. Fail closed before reporting coverage.
const suite = YAML.parseDocument(fixture);
assert.equal(suite.errors.length, 0, 'Prometheus fixture YAML must parse');
const cases = suite.toJS();
assert.deepEqual(cases.rule_files, ['event-relay-rules.yaml']);
assert.equal(cases.evaluation_interval, '1m');
const expectedCases = [
  ['all-supported-domains-missing', [], true],
  ['only-account-domain-present', ['account'], true],
  ['only-billing-domain-present', ['billing'], true],
  ['missing-billing-is-not-hidden-by-account-or-retired-token-replicas', ['account', 'account', 'token', 'token', 'other'], true],
  ['complete-supported-domains-and-replicas-do-not-alert', ['account', 'account', 'billing', 'billing'], false],
  ['complete-two-domains-one-replica-do-not-alert', ['account', 'billing'], false],
  ['unknown-and-retired-sources-do-not-fill-supported-domains', ['other', 'token', 'token'], true],
  ['missing-domains-before-five-minute-hold-do-not-alert', [], false],
];
assert.ok(Array.isArray(cases.tests), 'Prometheus fixture must contain test cases');
assert.deepEqual(cases.tests.map(test => test.name), expectedCases.map(([name]) => name), 'retain all eight distinct scenarios');
for (const [index, test] of cases.tests.entries()) {
  const [name, sources, alerting] = expectedCases[index];
  assert.equal(test.interval, '1m', name);
  assert.deepEqual(test.input_series.map(input => {
    assert.equal(input.values, '0+0x10', name);
    const series = /^pinvou_outbox_pending_events\{source="(account|billing|token|other)",instance="[abtx][12]"\}$/.exec(input.series);
    assert.ok(series, `${name}: use the supported relay metric fixture`);
    return series[1];
  }), sources, `${name}: preserve source coverage`);
  assert.equal(new Set(test.input_series.map(input => input.series)).size, sources.length, `${name}: distinct replicas`);
  assert.equal(test.alert_rule_test.length, 1, name);
  const check = test.alert_rule_test[0];
  assert.equal(check.alertname, 'PinvouAccountPlatformEventRelayMissing', name);
  assert.equal(check.eval_time, index === 7 ? '4m' : '6m', name);
  assert.equal(check.exp_alerts.length, alerting ? 1 : 0, `${name}: preserve alert/control expectation`);
}
// Refuse an existing directory: never overwrite another run or user files.
await mkdir(outputDirectory);
await writeFile(path.join(outputDirectory, 'event-relay-rules.yaml'), YAML.stringify({ groups }), { flag: 'wx' });
await writeFile(path.join(outputDirectory, 'event-relay-missing.test.yaml'), fixture, { flag: 'wx' });
const alertingCount = cases.tests.filter(test => test.alert_rule_test[0].exp_alerts.length > 0).length;
console.log(`prepared actual rendered alert rules and ${cases.tests.length} isolated promtool cases (${alertingCount} alerting cases and ${cases.tests.length - alertingCount} non-alert controls)`);
