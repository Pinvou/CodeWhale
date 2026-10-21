/**
 * MegaCube 面板的纯视图模型：cmsd JSON → 渲染就绪的行结构。
 * 不做任何 IO，node:test 直接验收。
 */

/** @param {number|null|undefined} gib @returns {string} 保留一位小数的 GiB 文本。 */
export function formatGiB(gib) {
  if (typeof gib !== 'number' || !Number.isFinite(gib)) return '—';
  return `${gib.toFixed(1)} GiB`;
}

/** @param {number|null|undefined} seconds @returns {{value:number, unit:'h'|'m'|'s'}|null} 运行时长粗粒度化。 */
export function coarseUptime(seconds) {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) return null;
  if (seconds >= 3600) return { value: Math.floor(seconds / 3600), unit: 'h' };
  if (seconds >= 60) return { value: Math.floor(seconds / 60), unit: 'm' };
  return { value: seconds, unit: 's' };
}

/** @param {number|null|undefined} epoch @returns {string|null} 本地 "MM-DD HH:MM"（诊断面板用数字格式，不随语言变）。 */
export function formatEpoch(epoch) {
  if (typeof epoch !== 'number' || !Number.isFinite(epoch) || epoch <= 0) return null;
  const d = new Date(epoch * 1000);
  /** @param {number} n */ const pad = (n) => String(n).padStart(2, '0');
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * @typedef {object} StatusView
 * @property {string} version
 * @property {{value:number, unit:'h'|'m'|'s'}|null} uptime
 * @property {string} gateway
 * @property {string} systemdMode
 * @property {string|null} activeModel
 * @property {{card:string, reason:string}[]} degraded
 */
/**
 * /v1/status → 状态条视图模型。
 * @param {Record<string, any>|undefined} data
 * @returns {StatusView|null}
 */
export function normalizeStatus(data) {
  if (!data || typeof data !== 'object') return null;
  const degraded = Array.isArray(data.degraded) ? data.degraded : [];
  return {
    version: typeof data.version === 'string' ? data.version : '—',
    uptime: coarseUptime(data.uptime_s),
    gateway: typeof data.gateway === 'string' ? data.gateway : '—',
    systemdMode: typeof data.systemd_mode === 'string' ? data.systemd_mode : '—',
    activeModel: typeof data.active_model === 'string' ? data.active_model : null,
    storeAdoption: data.store_adoption && typeof data.store_adoption === 'object' ? data.store_adoption : null,
    degraded: degraded
      .filter((d) => d && typeof d === 'object')
      .map((d) => ({ card: String(d.card ?? ''), reason: String(d.reason ?? '') })),
  };
}

/**
 * @typedef {object} ModelRow
 * @property {string} card
 * @property {string} modelId
 * @property {string} status
 * @property {boolean} managed
 * @property {boolean|null} healthy
 * @property {number|null} port
 * @property {number} restartCount
 * @property {string|null} lastRestart
 * @property {number|null} footprintGiB
 * @property {boolean|null} bootEnabled
 * @property {boolean} canSetAutostart
 * @property {string|null} degradedReason
 * @property {number|null} weightsGiB
 */
/**
 * @typedef {object} ModelsView
 * @property {string|null} activeModel
 * @property {ModelRow[]} models
 */
/**
 * /v1/models → 模型行列表。status 原样透传（running/starting/stopped/degraded），
 * 展示层映射颜色与文案。
 * @param {Record<string, any>|undefined} payload
 * @returns {ModelsView}
 */
export function normalizeModels(payload) {
  const list = payload && Array.isArray(payload.models) ? payload.models : [];
  return {
    activeModel: typeof payload?.active_model === 'string' ? payload.active_model : null,
    models: list
      .filter((m) => m && typeof m === 'object')
      .map((m) => ({
        card: String(m.card ?? ''),
        modelId: typeof m.model_id === 'string' && m.model_id.trim() ? m.model_id : String(m.card || '—'),
        status: typeof m.status === 'string' ? m.status : 'stopped',
        managed: m.managed === true,
        canAdopt: m.can_adopt === true,
        canReserveUninstall: m.can_reserve_uninstall === true,
        healthy: typeof m.healthy === 'boolean' ? m.healthy : null,
        port: typeof m.port === 'number' && m.port > 0 ? m.port : null,
        restartCount: typeof m.restart_count === 'number' ? m.restart_count : 0,
        lastRestart: formatEpoch(m.last_restart_at),
        footprintGiB: typeof m.measured_footprint_gib === 'number' ? m.measured_footprint_gib : null,
        bootEnabled: typeof m.boot_enabled === 'boolean' ? m.boot_enabled : null,
        canSetAutostart: m.managed === true && m.can_set_autostart === true && typeof m.boot_enabled === 'boolean',
        recoveryOwner: typeof m.recovery_owner === 'string' ? m.recovery_owner : null,
        degradedReason: typeof m.degraded_reason === 'string' ? m.degraded_reason : null,
        weightsGiB: typeof m.weights_bytes === 'number' ? m.weights_bytes / 1073741824 : null,
      })),
  };
}

/**
 * @typedef {object} EngineInstance
 * @property {'managed'|'external'} source
 * @property {number|null} pid
 * @property {number|null} port
 * @property {boolean|null} healthy
 * @property {boolean} alive
 */
/**
 * @typedef {object} EngineRow
 * @property {string} id
 * @property {string} engineType
 * @property {string} path
 * @property {EngineInstance[]} instances
 */
/**
 * /v1/engines → 引擎行列表（实例区分 managed/external，external 是只读收编）。
 * @param {Record<string, any>|undefined} payload
 * @returns {EngineRow[]}
 */
export function normalizeEngines(payload) {
  const list = payload && Array.isArray(payload.engines) ? payload.engines : [];
  const native = list
    .filter((e) => e && typeof e === 'object')
    .map((e) => ({
      id: String(e.id ?? ''),
      engineType: typeof e.engine_type === 'string' ? e.engine_type : 'native',
      path: typeof e.path === 'string' ? e.path : '',
      instances: (Array.isArray(e.instances) ? e.instances : [])
        .filter(/** @param {any} i */ (i) => i && typeof i === 'object')
        .map(/** @param {any} i */ (i) => ({
          source: i.source === 'external' ? 'external' : 'managed',
          modelId: typeof i.model_id === 'string' ? i.model_id : null,
          pid: typeof i.pid === 'number' ? i.pid : null,
          port: typeof i.port === 'number' ? i.port : null,
          healthy: typeof i.healthy === 'boolean' ? i.healthy : null,
          alive: i.alive !== false,
        })),
    }));
  const containers = Array.isArray(payload?.containers) ? payload.containers : [];
  return [...native, ...containers.filter((c) => c && typeof c === 'object').map((c) => ({
    id: String(c.name || ''), engineType: 'container', path: String(c.image || ''),
    instances: [{ source: c.card && c.managed !== false ? 'managed' : 'external', modelId: typeof c.health_model_id === 'string' ? c.health_model_id : typeof c.served_model_name === 'string' ? c.served_model_name : null, pid: typeof c.pid === 'number' ? c.pid : null,
      port: typeof c.host_port === 'number' ? c.host_port : null, healthy: typeof c.healthy === 'boolean' ? c.healthy : null,
      alive: c.running === true }],
  }))];
}

/**
 * @typedef {object} CapacityView
 * @property {number|null} totalGiB
 * @property {number|null} availableGiB
 * @property {number|null} usedGiB
 * @property {number|null} marginGiB
 * @property {{card:string, measuredGiB:number|null}[]} resident
 */
/**
 * /v1/capacity → 容量视图模型。
 * @param {Record<string, any>|undefined} payload
 * @returns {CapacityView}
 */
export function normalizeCapacity(payload) {
  const mem = payload && typeof payload.memory === 'object' && payload.memory ? payload.memory : {};
  const resident = payload && Array.isArray(payload.resident_models) ? payload.resident_models : [];
  return {
    totalGiB: typeof mem.total_gib === 'number' ? mem.total_gib : null,
    availableGiB: typeof mem.available_gib === 'number' ? mem.available_gib : null,
    usedGiB: typeof mem.used_gib === 'number' ? mem.used_gib : null,
    marginGiB: typeof payload?.safety_margin_gib === 'number' ? payload.safety_margin_gib : null,
    resident: resident
      .filter((r) => r && typeof r === 'object')
      .map((r) => ({
        card: String(r.card ?? ''),
        measuredGiB: typeof r.measured_footprint_gib === 'number' ? r.measured_footprint_gib : null,
      })),
  };
}

/**
 * 换胎完成后选哪行接入条目：卡片身份标记（store_card_id）> 网关+真名 >
 * 别名存量行 > 裸名字（兜底）。同一模型 id 可能多张卡供应（本机实测驭光/
 * sglang 同供 Qwen3.8-27B-NVFP4），裸名字匹配会撞车。
 * @param {Array<Record<string, any>>|undefined} rows - 品悟模型列表。
 * @param {string} card - 目标卡片 id。
 * @param {string|null|undefined} modelId - 卡的真实模型 id。
 * @param {string|null|undefined} gatewayBase - 网关 base（http://127.0.0.1:8000/v1）。
 * @returns {Record<string, any>|null} 命中的行。
 */
export function pickLinkedRowForCard(rows, card, modelId, gatewayBase) {
  if (!Array.isArray(rows)) return null;
  return rows.find((r) => r && r.store_card_id === card)
    || (gatewayBase ? rows.find((r) => r && r.model === modelId && r.base_url === gatewayBase) : null)
    || rows.find((r) => r && r.model === 'megacube-default')
    || rows.find((r) => r && r.model === modelId)
    || null;
}

/**
 * cmsd 托管模型的接入行显示名推导（商店库记录缺位时——AI 对话内下载装的模型
 * 走不到商店的接入链路）。引擎标签只认卡片 id 尾缀这条可靠线索
 * （qwen38-27b-nvfp4-tachyon → 驭光）；认不出就不贴，宁缺毋滥。
 * @param {string} card - 卡片 id。
 * @param {string} modelId - 真实模型 id（/v1/models 报出的 wire id）。
 * @returns {string} 显示名主体（接入行不再加「本地 ·」前缀，名称即引擎/卡片名）。
 */
const CARD_ENGINE_TAGS = { tachyon: '驭光' };
export function cmsdCardRowName(card, modelId) {
  const base = String(modelId || card || '');
  const parts = String(card || '').split('-');
  const tag = CARD_ENGINE_TAGS[parts[parts.length - 1]] || '';
  return tag ? `${base} (${tag})` : base;
}

/**
 * 引擎自报的 model id 能不能直接当接入行显示名。空值、或
 * `/models/target` 这类容器内路径（SGLang 未配 served-model-name 时的
 * 默认 id——真机 mtp 卡实测）都不能当名字，需回退到商店卡片显示名。
 * @param {string|null|undefined} modelId
 * @returns {boolean}
 */
export function modelIdNeedsNameFallback(modelId) {
  const v = String(modelId || '').trim();
  return v === '' || v.startsWith('/');
}

/** Retain confirmed device tasks while an unavailable store makes the aggregate partial. */
export function normalizeTasks(result) {
  const payload = result?.data;
  if (result?.available !== true || !Array.isArray(payload?.tasks)) return { tasks: null, complete: false };
  const tasks = payload.tasks.filter(task => task.source !== 'store'
    || !payload.tasks.some(parent => parent.source === 'cmsd' && parent.store_run_id === task.id));
  return { tasks, complete: payload.store_query?.available === true };
}
