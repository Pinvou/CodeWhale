import assert from 'node:assert/strict';
import test from 'node:test';
import { checkApprovalRelay } from '../../../scripts/account-platform-approval-relay-gate.mjs';

const args = ['release-context', 'release-namespace', 'release-account-event-relay'];
const url = 'http://127.0.0.1:9090/health/ready/approval-authorization';
const controller = (kind, uid) => [{ controller: true, kind, uid }];
const component = 'app.kubernetes.io/component';

function fixture({
  disable = false, absent = false, replaced = false, otherOwner = false, failed = false,
} = {}) {
  const deployment = {
    metadata: { uid: 'deployment-uid', generation: 3, annotations: { 'deployment.kubernetes.io/revision': '8' } },
    spec: { replicas: 2, selector: { matchLabels: { [component]: 'account-event-relay' } } },
    status: { observedGeneration: 3, updatedReplicas: 2, readyReplicas: 2 },
  };
  const replicaSet = {
    metadata: { uid: 'rs-uid', ownerReferences: controller('Deployment', 'deployment-uid'), annotations: { 'deployment.kubernetes.io/revision': '8' } },
  };
  const pod = index => ({
    metadata: { uid: `uid-${index}`, name: `relay-${index}`, ownerReferences: controller('ReplicaSet', otherOwner ? 'old-rs' : 'rs-uid') },
    spec: { containers: [{ name: 'event-relay', env: [{ name: 'HEALTHCHECK_URL', value: disable ? '' : url }] }] },
    status: { conditions: [{ type: 'Ready', status: 'True' }], containerStatuses: [{ name: 'event-relay', containerID: `container-${index}` }] },
  });
  const executions = [];
  let deploymentReads = 0;
  let restarted = false;
  const run = command => {
    assert.deepEqual(command.slice(0, 4), ['--context', args[0], '--namespace', args[1]]);
    const [action, object, ...rest] = command.slice(4);
    if (action === 'rollout') {
      assert.equal(object, 'status');
      assert.deepEqual(rest, [`deployment/${args[2]}`, '--timeout=120s']);
      return '';
    }
    if (action === 'get') {
      if (object === `deployment/${args[2]}`) {
        if (++deploymentReads > 1 && replaced) restarted = true;
        return JSON.stringify(deployment);
      }
      if (object === 'replicasets') return JSON.stringify({ items: [replicaSet] });
      assert.equal(object, 'pods');
      const items = [pod(1), ...(absent ? [] : [pod(2)])];
      if (restarted) items[0].status.containerStatuses[0].containerID = 'restarted';
      return JSON.stringify({ items });
    }
    assert.equal(action, 'exec');
    assert.deepEqual(rest, ['-c', 'event-relay', '--', '/healthcheck']);
    executions.push(object);
    if (failed && object === 'relay-2') throw new Error('specialized readiness failed');
    return '';
  };
  return { run, executions };
}

test('release gate executes the specialized healthcheck on each stable current replica', () => {
  const { run, executions } = fixture();
  assert.equal(checkApprovalRelay(args, run), 2);
  assert.deepEqual(executions, ['relay-1', 'relay-2']);
});

test('release gate rejects ordinary-only, missing, foreign, unhealthy and replaced replicas', () => {
  for (const [options, error] of [
    [{ disable: true }, /requireReady/], [{ absent: true }, /must be present/],
    [{ otherOwner: true }, /unexpected Account Relay pod owner/], [{ failed: true }, /specialized readiness failed/],
    [{ replaced: true }, /pods changed/],
  ]) {
    assert.throws(() => checkApprovalRelay(args, fixture(options).run), error);
  }
});
