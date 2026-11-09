#!/usr/bin/env node
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { isMainModule } from './account-platform-cli-entry.mjs';

const component = 'account-event-relay';
const target = 'http://127.0.0.1:9090/health/ready/approval-authorization';
const kubernetesName = /^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$/;

function kubectl(args) {
  const result = spawnSync('kubectl', args, { encoding: 'utf8', timeout: 130_000, windowsHide: true, maxBuffer: 4 * 1024 * 1024 });
  assert.equal(result.status, 0, 'kubectl command failed or timed out');
  return result.stdout;
}

export function checkApprovalRelay(args, run = kubectl) {
  assert.equal(args.length, 3, 'provide the Kubernetes context, namespace and Account Relay deployment');
  const [context, namespace, deploymentName] = args;
  assert.ok(context && !context.startsWith('-') && kubernetesName.test(namespace) && kubernetesName.test(deploymentName),
    'invalid context, namespace or deployment');
  const command = (...action) => run(['--context', context, '--namespace', namespace, ...action]);
  const deploymentRef = `deployment/${deploymentName}`;
  const readDeployment = () => JSON.parse(command('get', deploymentRef, '-o', 'json'));

  command('rollout', 'status', deploymentRef, '--timeout=120s');
  const deployment = readDeployment();
  const labels = deployment.spec.selector.matchLabels;
  assert.equal(labels?.['app.kubernetes.io/component'], component, 'select the Account Relay deployment');
  assert.ok(!deployment.spec.selector.matchExpressions?.length, 'expected the Account Relay chart selector');
  const selector = Object.entries(labels).map(([key, value]) => `${key}=${value}`).join(',');
  const replicas = deployment.spec.replicas;
  assert.ok(Number.isSafeInteger(replicas) && replicas > 0, 'expect a nonzero desired replica count');
  assert.equal(deployment.status.observedGeneration, deployment.metadata.generation);
  assert.equal(deployment.status.updatedReplicas, replicas);
  assert.equal(deployment.status.readyReplicas, replicas);

  const revision = deployment.metadata.annotations?.['deployment.kubernetes.io/revision'];
  assert.ok(revision, 'current deployment revision is missing');
  const sets = JSON.parse(command('get', 'replicasets', '-l', selector, '-o', 'json')).items;
  const owners = sets.filter(set => set.metadata.ownerReferences?.some(owner => owner.controller
    && owner.kind === 'Deployment' && owner.uid === deployment.metadata.uid)
    && set.metadata.annotations?.['deployment.kubernetes.io/revision'] === revision);
  assert.equal(owners.length, 1, 'expected the current ReplicaSet of the Account Relay deployment');
  const readPods = () => JSON.parse(command('get', 'pods', '-l', selector, '-o', 'json')).items
    .filter(pod => !pod.metadata.deletionTimestamp);
  const pods = readPods();
  assert.equal(pods.length, replicas, 'every expected Account Relay pod must be present');
  const podIdentity = pod => {
    assert.ok(pod.metadata.ownerReferences?.some(owner => owner.controller
      && owner.kind === 'ReplicaSet' && owner.uid === owners[0].metadata.uid), 'unexpected Account Relay pod owner');
    assert.ok(pod.status.conditions?.some(condition => condition.type === 'Ready' && condition.status === 'True'),
      'every Account Relay pod must be ordinarily ready');
    const container = pod.spec.containers.find(item => item.name === 'event-relay');
    const url = container?.env?.filter(variable => variable.name === 'HEALTHCHECK_URL');
    assert.ok(url?.length === 1 && url[0].value === target,
      'enable approvalRelay.requireReady and roll out the new Account Relay template');
    const containerID = pod.status.containerStatuses?.find(item => item.name === 'event-relay')?.containerID;
    assert.ok(containerID, 'event-relay container identity is missing');
    return [pod.metadata.name, pod.metadata.uid, containerID].join(':');
  };
  const expectedIdentities = pods.map(podIdentity).sort();
  for (const pod of pods) {
    try {
      command('exec', pod.metadata.name, '-c', 'event-relay', '--', '/healthcheck');
    } catch {
      throw new Error(`specialized readiness failed for Pod ${pod.metadata.name}`);
    }
  }
  const current = readDeployment();
  assert.equal(current.metadata.uid, deployment.metadata.uid, 'Account Relay deployment was replaced');
  assert.equal(current.metadata.generation, deployment.metadata.generation, 'Account Relay rollout changed during validation');
  assert.deepEqual(readPods().map(podIdentity).sort(), expectedIdentities, 'Account Relay pods changed during validation');
  return replicas;
}

if (isMainModule(import.meta.url)) {
  try {
    const replicas = checkApprovalRelay(process.argv.slice(2));
    console.log(`Approval authorization readiness passed on ${replicas} Account Relay replicas.`);
  } catch (error) {
    console.error(`APPROVAL_RELAY_RELEASE_GATE_FAILED: ${error.message}`);
    process.exitCode = 1;
  }
}
