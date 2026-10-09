#!/usr/bin/env node

import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const policyPath = path.join(root, 'account-platform', 'deploy', 'performance-policy.json');
const hash = /^[a-f0-9]{64}$/;
const required = (condition, message) => { if (!condition) throw new Error(message); };

export function validatePolicy(policy) {
  required(policy.schema_version === 1, 'unsupported performance policy schema');
  required(policy.baseline.steady_minutes >= 30 && policy.baseline.minimum_samples >= 10000 && policy.baseline.minimum_stream_samples >= 1000, 'baseline sample requirements are too small');
  required(policy.api.read_p95_ms === 300 && policy.api.write_p99_ms === 1500, 'API objectives changed');
  required(policy.stream.concurrent >= 200, 'stream concurrency must be at least 200');
  required(policy.holds.active >= 100000 && policy.payments.orders >= 1000000 && policy.payments.transactions >= 3000000, 'capacity dataset is too small');
  required(policy.admin_pagination.payment_orders >= 1000000 && policy.admin_pagination.minimum_pages >= Math.ceil(policy.admin_pagination.payment_orders / 200) && policy.admin_pagination.members_seen >= policy.admin_pagination.payment_orders && policy.admin_pagination.concurrent_creates >= 1 && policy.admin_pagination.concurrent_updates >= 1 && policy.admin_pagination.first_snapshot_p99_ms <= 10000, 'admin pagination capacity, traversal, concurrency, or snapshot objective is invalid');
  required(policy.events.retained_unacked_per_stream >= 1000000 && policy.events.drain_seconds <= 900, 'event retention or drain objective is invalid');
}

function atMost(actual, limit, name) { required(Number.isFinite(actual) && actual >= 0 && actual <= limit, `${name} exceeds ${limit}`); }
function atLeast(actual, limit, name) { required(Number.isFinite(actual) && actual >= limit, `${name} is below ${limit}`); }

export function validateEvidence(policy, evidence, now = Date.now(), provenance = {}) {
  required(evidence.schema_version === 1 && evidence.environment === 'production-equivalent', 'production-equivalent evidence is required');
  const generated = Date.parse(evidence.generated_at);
  required(Number.isFinite(generated) && Math.abs(now - generated) <= 24 * 3600 * 1000, 'performance evidence must be generated within 24 hours');
  required(hash.test(evidence.release_sha256) && hash.test(evidence.dataset_sha256) && hash.test(evidence.report_sha256), 'release, dataset, and report digests are required');
  required(hash.test(provenance.expectedReleaseSHA256) && evidence.release_sha256 === provenance.expectedReleaseSHA256, 'performance evidence release digest does not match the candidate release');
  atLeast(evidence.steady_minutes, policy.baseline.steady_minutes, 'steady duration');
  atMost(evidence.maximum_declared_capacity_percent, policy.baseline.maximum_declared_capacity_percent, 'declared capacity utilization');
  atLeast(evidence.api.samples, policy.baseline.minimum_samples, 'API samples');
  for (const [name, limit] of Object.entries(policy.api)) atMost(evidence.api[name], limit, `api.${name}`);
  for (const [name, limit] of Object.entries(policy.external)) atMost(evidence.external[name], limit, `external.${name}`);
  atLeast(evidence.stream.samples, policy.baseline.minimum_stream_samples, 'stream samples');
  atLeast(evidence.stream.concurrent, policy.stream.concurrent, 'stream concurrency');
  for (const name of ['dispatch_p95_ms', 'dispatch_p99_ms', 'content_p95_ms', 'content_p99_ms', 'unexpected_error_percent']) atMost(evidence.stream[name], policy.stream[name], `stream.${name}`);
  for (const name of ['active', 'steady_tps', 'burst_tps', 'burst_seconds']) atLeast(evidence.holds[name], policy.holds[name], `holds.${name}`);
  atMost(evidence.holds.lock_wait_p99_ms, policy.holds.lock_wait_p99_ms, 'holds.lock_wait_p99_ms');
  required(evidence.holds.conservation_violations === 0 && evidence.holds.duplicate_effects === 0, 'Hold conservation or idempotency failed');
  for (const name of ['orders', 'transactions', 'order_create_tps', 'evidence_merge_tps']) atLeast(evidence.payments[name], policy.payments[name], `payments.${name}`);
  required(evidence.payments.duplicate_financial_effects === 0, 'payment benchmark produced duplicate financial effects');
  atLeast(evidence.admin_pagination.payment_orders, policy.admin_pagination.payment_orders, 'admin_pagination.payment_orders');
  atLeast(evidence.admin_pagination.pages, policy.admin_pagination.minimum_pages, 'admin_pagination.pages');
  atLeast(evidence.admin_pagination.members_seen, policy.admin_pagination.members_seen, 'admin_pagination.members_seen');
  atLeast(evidence.admin_pagination.concurrent_creates, policy.admin_pagination.concurrent_creates, 'admin_pagination.concurrent_creates');
  atLeast(evidence.admin_pagination.concurrent_updates, policy.admin_pagination.concurrent_updates, 'admin_pagination.concurrent_updates');
  atMost(evidence.admin_pagination.first_snapshot_p99_ms, policy.admin_pagination.first_snapshot_p99_ms, 'admin_pagination.first_snapshot_p99_ms');
  required(evidence.admin_pagination.duplicate_members === policy.admin_pagination.duplicate_members && evidence.admin_pagination.omitted_members === policy.admin_pagination.omitted_members, 'admin pagination snapshot produced duplicate or omitted members');
  required(hash.test(evidence.admin_pagination.page_trace_sha256) && hash.test(evidence.admin_pagination.mutation_trace_sha256), 'admin pagination page and mutation trace digests are required');
  required(evidence.admin_pagination.page_trace_sha256 === provenance.pageTraceSHA256 && evidence.admin_pagination.mutation_trace_sha256 === provenance.mutationTraceSHA256, 'admin pagination trace artifacts do not match the evidence');
  atLeast(evidence.events.retained_unacked_per_stream, policy.events.retained_unacked_per_stream, 'event retention');
  atLeast(evidence.events.backlog, policy.events.backlog, 'event backlog');
  atMost(evidence.events.drain_seconds, policy.events.drain_seconds, 'event drain');
  atLeast(evidence.events.realtime_peak_percent, policy.events.realtime_peak_percent, 'event realtime load');
  required(evidence.events.lost_events === 0 && evidence.events.high_risk_gate_triggered === true, 'event loss or high-risk admission gate failed');
  for (const segment of ['platform_ms', 'provider_ms', 'queue_ms', 'client_network_ms']) required(Number.isFinite(evidence.timing_segments[segment]) && evidence.timing_segments[segment] >= 0, `timing segment ${segment} is missing`);
}

export function performanceGateArguments(args) {
  const requireEvidence = args.includes('--require-evidence');
  const expectedRelease = args.find(value => value.startsWith('--expected-release-sha256='))?.split('=', 2)[1];
  const pageTracePath = args.find(value => value.startsWith('--page-trace='))?.split('=', 2)[1];
  const mutationTracePath = args.find(value => value.startsWith('--mutation-trace='))?.split('=', 2)[1];
  const valuedOptions = ['--expected-release-sha256=', '--page-trace=', '--mutation-trace='];
  for (const value of args.filter(value => value.startsWith('--'))) required(value === '--require-evidence' || valuedOptions.some(prefix => value.startsWith(prefix)), `unknown performance gate option ${value}`);
  const positional = args.filter(value => !value.startsWith('--'));
  required(positional.length <= 1, 'only one performance evidence file may be provided');
  if (requireEvidence) required(positional.length === 1, 'production performance evidence file is required');
  if (positional.length > 0) {
    required(hash.test(expectedRelease || ''), 'candidate release SHA-256 is required');
    required(pageTracePath && mutationTracePath, 'admin pagination trace artifact files are required');
  }
  return { evidencePath: positional[0], expectedReleaseSHA256: expectedRelease, pageTracePath, mutationTracePath };
}

async function main() {
  const policy = JSON.parse(await readFile(policyPath, 'utf8'));
  validatePolicy(policy);
  const arguments_ = performanceGateArguments(process.argv.slice(2));
  if (arguments_.evidencePath) {
    const [evidence, pageTrace, mutationTrace] = await Promise.all([
      readFile(path.resolve(arguments_.evidencePath), 'utf8').then(JSON.parse),
      readFile(path.resolve(arguments_.pageTracePath)),
      readFile(path.resolve(arguments_.mutationTracePath)),
    ]);
    validateEvidence(policy, evidence, Date.now(), {
      expectedReleaseSHA256: arguments_.expectedReleaseSHA256,
      pageTraceSHA256: createHash('sha256').update(pageTrace).digest('hex'),
      mutationTraceSHA256: createHash('sha256').update(mutationTrace).digest('hex'),
    });
  }
  console.log(arguments_.evidencePath ? 'production performance evidence passed' : 'performance policy passed');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main().catch((error) => { console.error(`[performance-gate] ${error.message}`); process.exitCode = 1; });
