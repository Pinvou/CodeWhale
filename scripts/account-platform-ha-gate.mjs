#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { artifactGateArguments, verifyArtifactBindings } from './account-platform-artifact-bindings.mjs';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const policyPath = path.join(repositoryRoot, 'account-platform', 'deploy', 'ha-policy.json');
const sha256 = /^[a-f0-9]{64}$/;

function required(condition, message) {
  if (!condition) throw new Error(message);
}

function nonnegative(value) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

function count(value) {
  return Number.isSafeInteger(value) && value >= 0;
}

function parseTime(value, name) {
  const parsed = Date.parse(value);
  required(Number.isFinite(parsed), `${name} must be RFC3339`);
  return parsed;
}

export function validatePolicy(policy) {
  required(policy.schema_version === 1, 'unsupported HA policy schema');
  required(policy.minimum_failure_domains >= 2, 'HA requires at least two failure domains');
  required(policy.minimum_post_failure_headroom_percent >= 30, 'post-failure headroom must be at least 30 percent');
  required(policy.maximum_core_failover_seconds <= 300, 'core failover limit must not exceed five minutes');
  for (const component of ['account-service', 'billing-service', 'token-gateway', 'keycloak']) {
    required(policy.required_replicated_workloads.includes(component), `${component} must be replicated`);
  }
  for (const dependency of ['postgresql', 'redis', 'nats']) {
    required(policy.required_dependency_failovers.includes(dependency), `${dependency} failover must be exercised`);
  }
}

// FORMAT_ONLY: field validation does not authenticate evidence or prove an exercise.
export function validateEvidence(policy, evidence, now = Date.now()) {
  required(evidence.schema_version === 1 && evidence.environment === 'production-new', 'production HA evidence is required');
  const generatedAt = parseTime(evidence.generated_at, 'generated_at');
  required(Math.abs(now - generatedAt) <= 24 * 3600 * 1000, 'HA evidence must be generated within 24 hours');
  required(count(evidence.failure_domains) && evidence.failure_domains >= policy.minimum_failure_domains, 'insufficient independent failure domains');
  required(nonnegative(evidence.post_failure_headroom_percent) && evidence.post_failure_headroom_percent >= policy.minimum_post_failure_headroom_percent, 'insufficient post-failure capacity headroom');
  const workloads = new Map(evidence.workloads.map((entry) => [entry.name, entry]));
  for (const name of policy.required_replicated_workloads) {
    const workload = workloads.get(name);
    required(count(workload?.desired_replicas) && count(workload?.ready_replicas) && workload.desired_replicas >= 2 && workload.ready_replicas >= 2, `${name} is not ready with at least two replicas`);
    required(count(workload.failure_domains) && workload.failure_domains >= policy.minimum_failure_domains, `${name} is not spread across failure domains`);
    required(workload.pdb_enforced === true && count(workload.max_unavailable) && workload.max_unavailable <= 1, `${name} disruption protection is invalid`);
  }
  const dependencyResults = new Map(evidence.dependency_failovers.map((entry) => [entry.name, entry]));
  for (const name of policy.required_dependency_failovers) {
    const result = dependencyResults.get(name);
    required(result?.passed === true, `${name} failover did not pass`);
    required(result.committed_record_loss === 0 && result.duplicate_financial_effects === 0, `${name} failover lost or duplicated committed facts`);
  }
  const scenarios = new Map(evidence.scenarios.map((entry) => [entry.name, entry]));
  for (const name of policy.required_scenarios) {
    const scenario = scenarios.get(name);
    required(scenario?.passed === true, `${name} scenario did not pass`);
    required(nonnegative(scenario.core_unavailable_seconds) && scenario.core_unavailable_seconds <= policy.maximum_core_failover_seconds, `${name} core failover exceeded limit`);
    required(nonnegative(scenario.noncritical_unavailable_seconds) && scenario.noncritical_unavailable_seconds <= policy.maximum_noncritical_failover_seconds, `${name} noncritical failover exceeded limit`);
    required(scenario.real_provider_side_effects === 0, `${name} scenario reached a real provider`);
    required(sha256.test(scenario.evidence_sha256), `${name} evidence digest is invalid`);
  }
  required(nonnegative(evidence.identity_adapter_replacement_seconds) && evidence.identity_adapter_replacement_seconds <= policy.maximum_noncritical_failover_seconds, 'single-replica Identity Adapter replacement exceeded noncritical limit');
}

async function main() {
  const policy = JSON.parse(await readFile(policyPath, 'utf8'));
  validatePolicy(policy);
  const rawArgs = process.argv.slice(2);
  const requireEvidence = rawArgs.includes('--require-evidence');
  required(rawArgs.filter(value => value === '--require-evidence').length <= 1, 'duplicate --require-evidence');
  const options = artifactGateArguments(rawArgs.filter(value => value !== '--require-evidence'));
  required(options.remaining.length <= 1 && !options.remaining[0]?.startsWith('--'), 'unsupported HA arguments');
  if (!options.remaining.length) { required(!requireEvidence && process.argv.length === 2, 'HA evidence file is required'); console.log('HA policy passed'); return; }
  const { evidence, result } = await verifyArtifactBindings({ ...options, evidencePath: options.remaining[0], kind: 'HA', environment: 'production-new' });
  validateEvidence(policy, evidence);
  console.log(JSON.stringify(result));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(`[ha-gate] ${error.message}`);
    process.exitCode = 1;
  });
}
