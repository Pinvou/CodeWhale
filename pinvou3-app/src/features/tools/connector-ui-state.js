import { errorCode, localizedErrorMessage } from '../../shared/user-facing-error.js';

const STEP_ERROR_CODES = {
  runtime: 'runtime_prepare_failed',
  cli: 'cli_install_failed',
  connect: 'auth_start_failed',
  register: 'registration_failed',
  authorize: 'auth_failed',
  qr: 'auth_failed',
};

function connectorErrorCodeForStep(step) {
  return STEP_ERROR_CODES[String(step || '').trim().toLowerCase()] || 'unknown';
}

function connectorFailure(value, step) {
  return {
    errorCode: errorCode(value) || connectorErrorCodeForStep(step),
  };
}

function connectorUiStep(flow, phase) {
  const active = String((flow && flow.active) || '').trim().toLowerCase();
  const normalizedPhase = String(phase || '').trim().toLowerCase();
  if (normalizedPhase === 'authorize') return 'qr';
  if (normalizedPhase === 'register') return active === 'qr' ? 'qr' : 'connect';
  if (['runtime', 'cli', 'connect', 'qr'].includes(normalizedPhase)) return normalizedPhase;
  return active || 'cli';
}

function applyConnectorFailure(flow, value, phase) {
  const current = flow || { steps: {} };
  const step = connectorUiStep(current, phase);
  return {
    ...current,
    phase: 'error',
    ...connectorFailure(value, phase || step),
    errStep: step,
    steps: { ...(current.steps || {}), [step]: 'error' },
  };
}

function localizeConnectorAlert(alert, storeCopy, detailCopy) {
  if (!alert || !alert.connectorAlert) return alert;
  const toolId = alert.toolId;
  const toolName = (storeCopy.toolNames && storeCopy.toolNames[toolId]) || toolId;
  if (alert.connectorAlert === 'connected') {
    return {
      ...alert,
      title: storeCopy.connectedTool(toolName),
      subtitle: alert.showEnabled ? detailCopy.actions.enabled : '',
    };
  }
  if (alert.connectorAlert === 'disconnected') {
    return { ...alert, title: storeCopy.disconnectedTool(toolName), subtitle: '' };
  }
  return {
    ...alert,
    title: storeCopy.connectFailed(toolName),
    subtitle: localizedErrorMessage(
      { code: alert.errorCode },
      storeCopy.connectorErrors,
      storeCopy.connectorErrors.unknown,
    ),
  };
}

export { applyConnectorFailure, connectorErrorCodeForStep, connectorFailure, connectorUiStep, localizeConnectorAlert };
