/**
 * MegaCube 管理面（cmsd）只读查询的前端封装。
 * 命令层在 src-tauri/src/app/commands/cms.rs；能力门控由命令侧完成
 * （socket 缺失/平台不支持 → available:false），前端不报错、不打扰。
 */
import { invokeTauri } from '../../platform/tauri/client.js';

/**
 * @typedef {object} CmsQueryResult
 * @property {boolean} [detected] - Known local installation.
 * @property {boolean} available - 管理能力是否存在（false 时 reason 给出机器可读原因）。
 * @property {string} [reason] - 不可用原因（socket_missing / connect_failed / unsupported_platform…）。
 * @property {Record<string, any>} [data] - cmsd 的 JSON 应答原文（/v1/status、/v1/models 等）。
 */

/** @returns {Promise<CmsQueryResult>} 管理面状态概览。 */
export function fetchCmsStatus() {
  return invokeTauri('cms_status');
}

/** @returns {Promise<CmsQueryResult>} 已登记模型清单（含健康/重启计数/降级原因）。 */
export function fetchCmsModels() {
  return invokeTauri('cms_models');
}

/** @returns {Promise<CmsQueryResult>} 引擎注册表（含野生收编实例）。 */
export function fetchCmsEngines() {
  return invokeTauri('cms_engines');
}

/** @returns {Promise<CmsQueryResult>} 内存容量与常驻模型实测占用。 */
export function fetchCmsCapacity() {
  return invokeTauri('cms_capacity');
}

/** @returns {Promise<CmsQueryResult>} 任务列表（维护窗口只消费 switch 换胎任务；启停任务由发起方自行呈现）。 */
export function fetchCmsTasks() {
  return invokeTauri('cms_tasks');
}

/** A known accepted task can outlive its entry in the recent task index. */
export function fetchCmsTask(taskId) {
  return invokeTauri('cms_task', { taskId });
}

/** @param {string} card @param {string} action @returns {Promise<Record<string, any>>} */
export function actOnCmsModel(card, action) { return invokeTauri('cms_model_action', { card, action }); }
/** @param {string} taskId @returns {Promise<Record<string, any>>} */
export function cancelCmsTask(taskId) { return invokeTauri('cms_cancel_task', { taskId }); }

/** @param {boolean} [inspectRunning] @returns {Promise<Record<string, any>>} */
export function fetchCmsDeviceStatus(inspectRunning = false) {
  return invokeTauri('cms_device_status', { inspectRunning });
}

/** Task-observer probe only; the observer retries after outages and this result grants no control authority. */
export async function probeCmsAvailable() {
  try {
    const result = await fetchCmsStatus();
    return !!result?.available || !!result?.detected;
  } catch (error) {
    console.warn('[megacube] 能力探测失败，入口不渲染：', error);
    return false;
  }
}
