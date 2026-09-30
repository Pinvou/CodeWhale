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

// Own-key lookup for the flow card's copy. A bare `errors[code]` would resolve
// Object.prototype members (`constructor`, `toString`, `valueOf` all pass the
// snake_case pattern) to inherited functions — truthy, and crashing React when
// rendered. Codes only ever come from this repo's emitters today; this keeps
// the render safe if a future source ever forwards an outside string.
function connectorErrorCopy(errors, code) {
  if (!errors || typeof code !== 'string' || !Object.prototype.hasOwnProperty.call(errors, code)) return '';
  const copy = errors[code];
  return typeof copy === 'string' ? copy : '';
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
    steps: { ...current.steps, [step]: 'error' },
  };
}

/**
 * 释放共享忙碌槽位：只释放属于自己的那次操作。
 *
 * 连接器的完成/失败事件是**异步**送达的，期间用户完全可能已经收起了本连接器的
 * 登录弹窗（收起不取消后台流程）并发起了另一个工具操作，此时 busyId 已经易主。
 * 无条件 `setBusyId(null)` 会把别人未完成的忙碌态一并抹掉：按钮只按
 * `busyId === tool.backendId` 判断禁用，于是正在跑的安装/卸载/导入会提前放闸，
 * 可以被重复触发。
 *
 * 用法：`setBusyId(current => releaseBusy(current, 'eip'))`。
 */
function releaseBusy(busyId, toolId) {
  return busyId === toolId ? null : busyId;
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
  // 'retry' = 后端在等上一个流程归还门闩,不是连接失败,别用失败文案吓人。
  // - subtitle 必须非空:否则 TsAlert 会落到 installHint/removeHint 的兜底文案;
  // - 关掉"新建会话"按钮:那是给"配置成功、需新建会话才生效"用的提示,
  //   挂在"稍后重试"上语义不符。
  if (alert.connectorAlert === 'retry') {
    return {
      ...alert,
      title: storeCopy.connectBusy(toolName),
      subtitle: storeCopy.connectBusyHint,
      suppressNewChat: true,
    };
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

export { applyConnectorFailure, connectorErrorCopy, connectorErrorCodeForStep, connectorFailure, connectorUiStep, localizeConnectorAlert, releaseBusy };
