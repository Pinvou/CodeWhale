import assert from 'node:assert/strict';
import test from 'node:test';

import { validateEvidence, validatePolicy } from '../../../scripts/account-platform-ha-gate.mjs';

const policy = {
  schema_version: 1,
  minimum_failure_domains: 2,
  minimum_post_failure_headroom_percent: 30,
  maximum_core_failover_seconds: 300,
  maximum_noncritical_failover_seconds: 900,
  required_replicated_workloads: ['account-service', 'billing-service', 'token-gateway', 'keycloak'],
  required_dependency_failovers: ['postgresql', 'redis', 'nats'],
  required_scenarios: ['pod_loss', 'node_loss'],
};

function evidence(now) {
  return {
    schema_version: 1,
    environment: 'production-new',
    generated_at: new Date(now).toISOString(),
    failure_domains: 2,
    post_failure_headroom_percent: 40,
    workloads: policy.required_replicated_workloads.map((name) => ({ name, desired_replicas: 2, ready_replicas: 2, failure_domains: 2, pdb_enforced: true, max_unavailable: 1 })),
    dependency_failovers: policy.required_dependency_failovers.map((name) => ({ name, passed: true, committed_record_loss: 0, duplicate_financial_effects: 0 })),
    scenarios: policy.required_scenarios.map((name) => ({ name, passed: true, core_unavailable_seconds: 30, noncritical_unavailable_seconds: 60, real_provider_side_effects: 0, evidence_sha256: 'a'.repeat(64) })),
    identity_adapter_replacement_seconds: 60,
  };
}

test('HA evidence requires independent capacity, disruption protection, and lossless failover', () => {
  const now = Date.parse('2026-08-24T00:00:00Z');
  assert.doesNotThrow(() => validatePolicy(policy));
  assert.doesNotThrow(() => validateEvidence(policy, evidence(now), now));
  const oneZone = evidence(now);
  oneZone.failure_domains = 1;
  assert.throws(() => validateEvidence(policy, oneZone, now), /failure domains/);
  const lostFact = evidence(now);
  lostFact.dependency_failovers[0].committed_record_loss = 1;
  assert.throws(() => validateEvidence(policy, lostFact, now), /lost or duplicated/);
});

test('HA rejects invalid numeric outage and replica evidence', () => {
  const now = Date.parse('2026-08-24T00:00:00Z');
  for (const invalid of [-1, null, '30']) {
    const outage = evidence(now); outage.scenarios[0].core_unavailable_seconds = invalid;
    assert.throws(() => validateEvidence(policy, outage, now), /core failover/);
    const replacement = evidence(now); replacement.identity_adapter_replacement_seconds = invalid;
    assert.throws(() => validateEvidence(policy, replacement, now), /replacement/);
  }
  const replicas = evidence(now); replicas.workloads[0].desired_replicas = '2';
  assert.throws(() => validateEvidence(policy, replicas, now), /replicas/);
  const pdb = evidence(now); pdb.workloads[0].max_unavailable = -1;
  assert.throws(() => validateEvidence(policy, pdb, now), /disruption/);
});
