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
// Refuse an existing directory: never overwrite another run or user files.
await mkdir(outputDirectory);
await writeFile(path.join(outputDirectory, 'event-relay-rules.yaml'), YAML.stringify({ groups }), { flag: 'wx' });
await writeFile(path.join(outputDirectory, 'event-relay-missing.test.yaml'), fixture, { flag: 'wx' });
console.log('prepared actual rendered alert rules and five isolated promtool cases');
