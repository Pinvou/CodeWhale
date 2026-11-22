import { acceptFreshDevelopmentReceipt } from './account-platform-dev-nats.mjs';

// Only a newly allocated test project may call this sequence. Existing dev
// environments use operator-supplied receipts and never enter this function.
export function initializeFreshDevelopmentNATS({ run, capture, args, owned }) {
  let endpoint;
  try { endpoint = JSON.parse(capture('docker', ['context', 'inspect', '--format', '{{json .Endpoints.docker.Host}}'])); } catch { throw new Error('DEV_NATS_LOCAL_DOCKER_REQUIRED'); }
  if (typeof endpoint !== 'string' || !(endpoint.startsWith('unix:///') || endpoint.startsWith('npipe:////./pipe/'))) throw new Error('DEV_NATS_LOCAL_DOCKER_REQUIRED');
  for (const command of [
    ['ps', '-aq', '--filter', `label=com.docker.compose.project=${owned.project}`],
    ['volume', 'ls', '-q', '--filter', `label=com.docker.compose.project=${owned.project}`],
    ['volume', 'ls', '-q', '--filter', `name=${owned.project}_`],
    ['network', 'ls', '-q', '--filter', `label=com.docker.compose.project=${owned.project}`],
    ['network', 'ls', '-q', '--filter', `name=${owned.project}_`],
  ]) if (capture('docker', command).trim() !== '') throw new Error('DEV_NATS_TEST_PROJECT_ALREADY_EXISTS');
  owned.mayCleanCompose = true;
  const invoke = (...command) => run('docker', [...args, ...command]);
  invoke('run', '--rm', '--no-deps', 'deployment-credentials-check');
  invoke('up', '--build', '-d', '--wait', 'postgres', 'nats');
  invoke('run', '--rm', '--build', '--no-deps', 'account-migrate');
  invoke('run', '--rm', '--build', '--no-deps', 'dev-event-resources', '-create-only');
  invoke('run', '--rm', '--no-deps', 'dev-event-resources');
  const control = (action, version) => {
    const raw = capture('docker', [...args, 'run', '--rm', '--build', '--quiet-build', '--no-deps', 'dev-approval-resources', '-action', action, '-expected-version', String(version)]);
    let value;
    try { value = JSON.parse(raw); } catch { throw new Error('DEV_NATS_CONTROL_RESPONSE_INVALID'); }
    if (value.ControlID !== 'ADMIN_APPROVAL' || value.Version !== version + (action === 'quiesce' || action === 'fence' ? 1 : 0)) throw new Error('DEV_NATS_CONTROL_RESPONSE_INVALID');
    return value;
  };
  const initial = control('status', 1);
  if (initial.Mode !== 'LEGACY' || initial.AuthorizationRelayState !== 'RUNNING' || initial.ApprovalEpoch !== 1 || initial.AuthorizationRelayEpoch !== 1) throw new Error('DEV_NATS_CONTROL_NOT_FRESH');
  control('quiesce', 1);
  control('fence', 2);
  const initialized = control('initialize-consumers', 3);
  if (initialized.Mode !== 'QUIESCING' || initialized.AuthorizationRelayState !== 'FENCED' || initialized.ApprovalEpoch !== 2 || initialized.AuthorizationRelayEpoch !== 2) throw new Error('DEV_NATS_CONTROL_RESPONSE_INVALID');
  // The digest is accepted only after this owned project's real CLI has
  // captured the broker watermark under Account's fresh initialization lock.
  acceptFreshDevelopmentReceipt(owned);
}

// Only the fresh disposable payment journeys use the existing simulator/refund
// contract. Never use these defaults for dev up or an existing project.
export function developmentTestPaymentEnvironment(testFiles, environment) {
  const paymentJourney = testFiles.some(file => file === 'compose-acceptance.mjs' || file === 'compose-fault.mjs');
  if (!paymentJourney) return {};
  if (environment.BILLING_PAYMENT_PROVIDER !== undefined && environment.BILLING_PAYMENT_PROVIDER.trim().toLowerCase() !== 'development_simulator') throw new Error('DEV_NATS_TEST_SIMULATOR_REQUIRED');
  const result = { BILLING_PAYMENT_PROVIDER: 'development_simulator' };
  if (testFiles.includes('compose-acceptance.mjs')) {
    if (environment.BILLING_NORMAL_REFUND_CREATION_ENABLED !== undefined && environment.BILLING_NORMAL_REFUND_CREATION_ENABLED.trim().toLowerCase() !== 'true') throw new Error('DEV_NATS_TEST_NORMAL_REFUND_REQUIRED');
    result.BILLING_NORMAL_REFUND_CREATION_ENABLED = 'true';
  }
  return result;
}
