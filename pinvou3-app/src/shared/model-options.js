function isBuiltInAccountPlatformModel(model) {
  return !!(model && (
    model.kind === 'builtin_account_platform' ||
    model.id === 'builtin_account_platform'
  ));
}

function selectableModels(models) {
  // Catalog visibility is independent of the current login/credit state.
  // Token Gateway remains authoritative when each request is prepared.
  return (models || []).filter(model => model && model.id);
}

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

export { isBuiltInAccountPlatformModel, selectableModels, baseUrlIsLoopback, isLocalModel, activeModelIsLocal };
