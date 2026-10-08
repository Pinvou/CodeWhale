#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const policyPath = path.join(repositoryRoot, 'account-platform', 'deploy', 'ha-policy.json');
const sha256 = /^[a-f0-9]{64}$/;

function required(condition, message) {
  if (!condition) throw new Error(message);
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

export function validateEvidence(policy, evidence, now = Date.now()) {
  required(evidence.schema_version === 1 && evidence.environment === 'production-new', 'production HA evidence is required');
  const generatedAt = parseTime(evidence.generated_at, 'generated_at');
  required(Math.abs(now - generatedAt) <= 24 * 3600 * 1000, 'HA evidence must be generated within 24 hours');
  required(evidence.failure_domains >= policy.minimum_failure_domains, 'insufficient independent failure domains');
  required(evidence.post_failure_headroom_percent >= policy.minimum_post_failure_headroom_percent, 'insufficient post-failure capacity headroom');
  const workloads = new Map(evidence.workloads.map((entry) => [entry.name, entry]));
  for (const name of policy.required_replicated_workloads) {
    const workload = workloads.get(name);
    required(workload?.desired_replicas >= 2 && workload?.ready_replicas >= 2, `${name} is not ready with at least two replicas`);
    required(workload.failure_domains >= policy.minimum_failure_domains, `${name} is not spread across failure domains`);
    required(workload.pdb_enforced === true && workload.max_unavailable <= 1, `${name} disruption protection is invalid`);
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
    required(scenario.core_unavailable_seconds <= policy.maximum_core_failover_seconds, `${name} core failover exceeded limit`);
    required(scenario.noncritical_unavailable_seconds <= policy.maximum_noncritical_failover_seconds, `${name} noncritical failover exceeded limit`);
    required(scenario.real_provider_side_effects === 0, `${name} scenario reached a real provider`);
    required(sha256.test(scenario.evidence_sha256), `${name} evidence digest is invalid`);
  }
  required(evidence.identity_adapter_replacement_seconds <= policy.maximum_noncritical_failover_seconds, 'single-replica Identity Adapter replacement exceeded noncritical limit');
}

async function main() {
  const policy = JSON.parse(await readFile(policyPath, 'utf8'));
  validatePolicy(policy);
  if (process.argv[2]) validateEvidence(policy, JSON.parse(await readFile(path.resolve(process.argv[2]), 'utf8')));
  console.log(process.argv[2] ? 'production HA evidence passed' : 'HA policy passed');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(`[ha-gate] ${error.message}`);
    process.exitCode = 1;
  });
}
