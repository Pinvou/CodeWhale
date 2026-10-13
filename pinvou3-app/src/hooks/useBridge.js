import { useEffect, useState } from 'react';
import { can, isWeb } from '../shared/platform.js';
import { CHAT_RECOVERY_COPY } from '../shared/i18n.js';

const bridge = window.TauriBridge || { available: false };

function mergeSlices(domains) {
  if (!bridge.available || !bridge.state) return null;
  return bridge.state.getMany(domains);
}

function useBridgeState(domains) {
  const domainKey = domains.join('|');
  const [bridgeState, setBridgeState] = useState(() => mergeSlices(domains));
  useEffect(() => {
    if (!bridge.available) return undefined;
    bridge.lifecycle.init({ uiDictionary: CHAT_RECOVERY_COPY }).catch(e => console.warn('[TauriBridge] init failed', e));
    return bridge.state.subscribeMany(domains, setBridgeState);
  }, [domainKey]);
  return bridgeState;
}

function usePlatformCapability(capability) {
  const [supported, setSupported] = useState(() => can(capability));
  useEffect(() => {
    const refresh = () => setSupported(can(capability));
    if (!isWeb) {
      if (capability !== 'desktopRemoteControl' || !bridge.available || !bridge.state) return undefined;
      return bridge.state.subscribe('platform', refresh);
    }
    window.addEventListener('pinvou:web-capabilities', refresh);
    window.addEventListener('pinvou:web-connection', refresh);
    refresh();
    return () => {
      window.removeEventListener('pinvou:web-capabilities', refresh);
      window.removeEventListener('pinvou:web-connection', refresh);
    };
  }, [capability]);
  return supported;
}

    /* ==========================================
       自定义标题栏（无边框窗口）—— 最小化 / 最大化 / 关闭
       ========================================== */

// 只把真正的 loopback URL 视为本地端点，避免正则把
// `https://localhost.example.com` / `http://127.0.0.10.example.com` 误判为本地。
function baseUrlIsLoopback(baseUrl) {
  try {
    const hostname = new URL(baseUrl).hostname.replace(/^\[|\]$/g, '').replace(/\.$/, '').toLowerCase();
    if (hostname === 'localhost' || hostname === '::1') return true;
    const octets = hostname.split('.');
    return octets.length === 4
      && octets.every(part => /^\d+$/.test(part) && Number(part) <= 255)
      && Number(octets[0]) === 127;
  } catch {
    return false;
  }
}

// 判定单个模型是否本地推理后端（local_vllm 预设，或 base_url 指向 loopback）。
// 集中放这里，供加载提示与 API Key gate 共用，避免两套规则漂移。
function isLocalModel(model) {
  return !!(model && (model.preset === 'local_vllm' || baseUrlIsLoopback(model.base_url || '')));
}

// 当前激活模型是否本地推理后端；拿不到模型信息时默认 false（按在线口径显示，绝不误称本地）。
function activeModelIsLocal(bs) {
  if (!bs || !Array.isArray(bs.savedModels) || !bs.activeModelId) return false;
  const m = bs.savedModels.find(x => x && x.id === bs.activeModelId);
  return isLocalModel(m);
}

// API Key gate 只覆盖正在交互的聊天页。设置页必须始终可达，否则首次启动时
// “去配置”按钮会把用户送到仍被 gate 盖住的设置页，形成无法录入 Key 的死锁。
function shouldShowApiKeyGate(bs, currentView, bridgeAvailable, isControlledChatActive = false) {
  const inChat = currentView === 'chat'
    || (currentView === 'scheduled' && !!(bs && bs.scheduledRunContext));
  // 被控端连接后，聊天页承载的是远端任务和稍后创建的权威 Session，不能再用
  // 当前本机普通会话的模型配置弹出 API Key 门控。重连期间也保持这一语义，避免
  // 链路短暂抖动时弹窗盖住仍在恢复的被控对话；连接终止后则自然恢复本机门控。
  const config = bs && bs.effectiveModelConfig;
  const missingCredential = config
    && (config.credential_state === 'missing' || config.credential_state === 'unavailable');
  // 旧后端没有返回 requires_user_api_key 时保持原有安全门控；显式 false 表示凭据由
  // 运行时或无鉴权端点负责，前端不能要求用户手工填写 API Key。
  const requiresUserApiKey = config && config.requires_user_api_key !== false;
  return !!(bridgeAvailable && inChat && !isControlledChatActive
    && missingCredential && requiresUserApiKey && !isLocalModel(config));
}

export {
  bridge,
  useBridgeState,
  usePlatformCapability,
  isLocalModel,
  activeModelIsLocal,
  shouldShowApiKeyGate,
};
