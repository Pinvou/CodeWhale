// 模型商店的接入层：把 Tauri 命令包装成一组按业务命名的函数，视图层不拼命令名、
// 不处理传输细节。
//
// 这个文件顶替了原来的 `lmm-client.js`。那一版把所有请求经一个 `lmm_request`
// 代理发给 LMM sidecar 的本地 HTTP 管理接口——那是**两个进程**才需要的形态：
// 要 token 鉴权、要 JSON over HTTP、要处理 sidecar 没起来 / 端口被占 / 握手超时。
//
// 模型商店原生做进品悟之后这些统统消失了：同一个进程，直接 invoke。随之去掉的
// 还有「先拉起 sidecar 再操作」这一步——没有东西需要拉起。
//
// 依赖方向 features -> platform，不得反向引用 app/。
import { invokeTauri } from '../../platform/tauri/client.js';
import { LOG_WAIT_SECONDS } from './model-store-logic.js';

/**
 * 这台机器上有没有模型商店。
 *
 * 模型商店是纯 Linux 功能（要 docker 起推理引擎、要 pty 跑卡片脚本、装引擎时要
 * 提权），Windows / macOS 上回 `available: false`，侧栏入口据此整个不渲染。
 * @returns {Promise<{available:boolean, reason:string|null}>} 可用性。
 */
function storeAvailability() {
  return invokeTauri('model_store_availability');
}

/**
 * 商店在架卡片。数据源只有一个：生产商店（地址是后端的编译期常量，不可配）。
 *
 * 这里原来有一个 `testMode` 形参，走的是打包在应用内的 6 张测试卡片，另外还有
 * `fetchTestStoreState` / `unlockTestStore` / `disableTestStore` 三个开关调用。
 * 内置测试商店 2026-09-30 整体拿掉了——内置副本跟生产在架卡片已经分叉，排障时
 * 看错文件是常事。
 * @returns {Promise<Array<object>>} 卡片摘要列表。
 */
function fetchStoreCards() {
  return invokeTauri('model_store_cards');
}

/**
 * 本地模型库。`corrected` 是启动时按实际容器状态纠正过运行标记的模型名。
 * @returns {Promise<{models:Array<object>, corrected:Array<string>}>} 库快照。
 */
function fetchLibrary() {
  return invokeTauri('model_store_library');
}

/**
 * 单个模型的详情（运行时信息、端点、脚本正文等）。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<object>} 模型详情。
 */
function fetchModelDetail(cardId) {
  return invokeTauri('model_store_model_detail', { cardId });
}

/** 已验证存在的模型商店推理引擎；历史数据库残留不会混进列表。 */
function fetchInstalledEngines() {
  return invokeTauri('model_store_installed_engines');
}

/** 本机扫描到的外部权重候选。它们尚未被认定为某张卡片的完整模型。 */
function fetchLocalResources() {
  return invokeTauri('model_store_local_resources');
}

/** 启用模型前的内存准入快照。 */
function fetchMemoryAdmission(cardId) {
  return invokeTauri('model_store_memory_admission', { cardId });
}

/** 删除一个未被模型绑定、且客户端能安全清理的推理引擎。 */
function deleteInstalledEngine(engineName, engineVersion = null) {
  return invokeTauri('model_store_delete_engine', { engineName, engineVersion });
}

/**
 * 某个模型的 API Key 列表。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<Array<object>>} Key 记录。
 */
function fetchApiKeys(cardId) {
  return invokeTauri('model_store_api_keys', { cardId });
}

/**
 * 新建一把 API Key。返回体含明文 `key_value`。
 * @param {string} cardId - 卡片 ID。
 * @param {string|null} [label] - 可选备注。
 * @returns {Promise<object>} 新建的 Key 记录。
 */
function createApiKey(cardId, label = null) {
  return invokeTauri('model_store_create_api_key', { cardId, label });
}

function fetchLanStatus(cardId) { return invokeTauri('model_store_lan_status', { cardId }); }
function setLanSharing(cardId, enabled, address, port = 8090) {
  return invokeTauri('model_store_set_lan_sharing', { cardId, enabled: enabled === true, address, port });
}
function revokeApiKey(keyId) { return invokeTauri('model_store_revoke_api_key', { keyId }); }

/**
 * 发起下载。长操作：立刻返回 run_id，进度靠 `pollRunLog` 读。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startCardScripts(cardId, rollback = false, confirmed = false) {
  return invokeTauri('model_store_start_card_scripts', { cardId, rollback: rollback === true, confirmed: confirmed === true });
}

function startDownload(cardId) {
  return invokeTauri('model_store_start_download', { cardId });
}

/**
 * 发起部署。
 * @param {string} cardId - 卡片 ID。
 * @param {string|null} [profileId] - 部署方案 ID；单方案卡片传 null。
 * @param {boolean} [memoryConfirmed] - 用户是否明确确认了本次操作的内存风险。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startDeploy(cardId, profileId = null, memoryConfirmed = false) {
  return invokeTauri('model_store_start_deploy', { cardId, profileId, memoryConfirmed: memoryConfirmed === true });
}

/**
 * 启用（拉起推理容器）。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startEnable(cardId, memoryConfirmed = false) {
  return invokeTauri('model_store_start_lifecycle', {
    cardId,
    action: 'enable',
    memoryConfirmed: memoryConfirmed === true,
  });
}

/**
 * 停用。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startStop(cardId) {
  return invokeTauri('model_store_start_lifecycle', { cardId, action: 'stop' });
}

/**
 * 卸载 / 删除。两者都会停服务、移除部署配置，区别在**权重和卡片去留**：
 *
 * - `purgeWeights = false`（**卸载**）：权重留在磁盘上，卡片留在模型库里退回
 *   「已下载」，下次点「部署」直接用，不必重下几十 GB；
 * - `purgeWeights = true`（**删除**）：连权重一起清掉、卡片移出模型库，
 *   **不可撤销**。
 *
 * 缺省是保留——不可逆的那一侧绝不能靠参数缺席命中。
 * @param {string} cardId - 卡片 ID。
 * @param {boolean} purgeWeights - true = 连模型权重一起删除。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startUninstall(cardId, purgeWeights) {
  return invokeTauri('model_store_start_lifecycle', {
    cardId,
    action: 'uninstall',
    purgeWeights: !!purgeWeights,
  });
}

/**
 * 强制从模型库移除：**只删记录，一个字节都不删**。
 *
 * 只在正常卸载反复失败时用。它不可逆（记录和这张卡的 API Key 一起没），而且删完
 * 之后磁盘上的几十 GB 就再也没有界面能管到了——所以要显式 confirm。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<object>} 移除结果，含仍在磁盘上的残留路径。
 */
function forceRemoveModel(cardId) {
  return invokeTauri('model_store_force_remove', { cardId, confirm: true });
}

/**
 * 读一段运行日志。游标 `from` 在调用方手里——断线重连不丢行也不重复，这是整条
 * 长操作链路能做到「界面切走再切回来也不影响」的原因。
 *
 * `wait` 让服务端在没有新行时最多挂 25 秒再返回，所以轮询循环不是忙等。
 * 任务结束后补捞收尾行时传 `wait = 0`：那一次要的是立刻返回，不是再挂 25 秒。
 * @param {string} runId - 运行 ID。
 * @param {number} from - 行游标。
 * @param {number} [wait] - 服务端最多挂多少秒；默认 LOG_WAIT_SECONDS。
 * @returns {Promise<{lines:Array<string>, next:number, done:boolean, ok:boolean, cancelled:boolean, result:unknown, error:unknown}>} 日志分片。
 */
function pollRunLog(runId, from, wait = LOG_WAIT_SECONDS) {
  return invokeTauri('model_store_run_log', { runId, from, wait });
}

/**
 * 取消一次运行。
 * @param {string} runId - 运行 ID。
 * @returns {Promise<{ok:boolean}>} 结果。
 */
function cancelRun(runId) {
  return invokeTauri('model_store_cancel_run', { runId });
}

/**
 * 回答脚本请求的那次输入。
 *
 * `answer` 传 null 表示用户点了取消——服务端会把这次运行整个中断，而不是拿一个
 * 空串继续往下跑（双机部署里那就是拿空的从机 IP 去连）。
 * @param {string} runId - 运行 ID。
 * @param {string|null} answer - 用户输入；null 表示取消。
 * @returns {Promise<{ok:boolean}>} 结果。
 */
function answerRun(runId, answer) {
  return invokeTauri('model_store_answer_run', { runId, answer });
}

/**
 * 在文件管理器里打开模型商店的运行日志目录（`~/.pinvou3/model-store/logs/`）。
 *
 * 只"打开目录"、不"把日志读回前端"：那几 MB 文本走 IPC 再塞进一个浮动任务卡没有
 * 任何意义，而用户真正要做的是把文件发给别人。
 * @returns {Promise<{path:string}>} 目录路径。
 */
function revealRunLogsDir() {
  return invokeTauri('model_store_reveal_run_logs_dir');
}

/**
 * 导出一次运行的日志，生成一份**可外发**的副本。
 *
 * 跟落盘那份的区别是脱敏强度：落盘那份刻意留着路径和内网地址（不留就没法诊断），
 * 导出那份会把它们一起抹掉——它是要离开这台机器的那一份。
 * @param {string} runId - 运行 ID。
 * @returns {Promise<{path:string}>} 导出文件路径。
 */
function exportRunLog(runId) {
  return invokeTauri('model_store_export_run_log', { runId });
}

/**
 * 还在跑的长操作。给了 `cardId` 就只问那一张卡。
 *
 * 进入模型商店时用它把任务卡接回去。任务态在前端随视图销毁（模型商店是条件挂载
 * 的），而一次下载要跑几十分钟到十几小时——没有这条路，切走再回来就等于把这次
 * 运行丢了：看不到进度、点不了取消、脚本请求输入也没人答（600 秒后被判死），
 * 再点一次下载还会撞上后端的卡片互斥，被渲染成一张说不清原因的失败卡。
 * @param {string|null} [cardId] - 只看这张卡；不给就是全部。
 * @returns {Promise<{runs:Array<{card_id:string, run_id:string, kind:string, label:string}>}>} 活跃运行。
 */
function fetchActiveRuns(cardId = null) {
  return invokeTauri('model_store_active_runs', { cardId });
}

/**
 * 取走启动清理的报告（**取一次就没了**）。
 *
 * 应用启动时会收掉上次没收干净的脚本进程。不取的话这份报告永远烂在内存里——
 * 而它要告诉用户的正是那起事故：上次强杀之后残留的下载进程占着文件锁，把下一次
 * 下载堵了 16 小时且没有任何提示。收掉了要说一声，收不掉（root 身份）更要说。
 * @returns {Promise<{killed:string[], skipped_root:string[]}>} 收掉的与收不掉的。
 */
function takeStartupCleanup() {
  return invokeTauri('model_store_take_startup_cleanup');
}

/**
 * 权重目录、用户补充的扫描目录，以及**实际会扫的完整目录列表**。
 *
 * `scan_dirs` 是只读的，含内置位置（当前权重目录、`~/models`、下载/桌面）和当前
 * 挂着的 U 盘；`import_scan_dirs` 才是用户能编辑的那份补充。两者分开是因为用户
 * 真正要问的是「下载前到底找了哪些地方」，只给可编辑的那份答不了这个问题。
 * @returns {Promise<{models_dir:string, import_scan_dirs:string[], scan_dirs:string[], free_bytes:number|null}>} 当前存储设置。
 */
function fetchStorageSettings() {
  return invokeTauri('model_store_storage_settings');
}

/**
 * 改权重目录 / 改补充扫描目录。传 null 的那项不动。
 *
 * **不会搬运已下载的权重**：几十上百 GB 的移动可能跨盘、可能中途断电，那得是
 * 一件有独立进度和失败处理的事，不能藏在一次保存设置里悄悄发生。
 * @param {{modelsDir?:string|null, importScanDirs?:string[]|null}} patch - 要改的项。
 * @returns {Promise<object>} 保存后的设置，形状同 fetchStorageSettings。
 */
function saveStorageSettings({ modelsDir = null, importScanDirs = null } = {}) {
  return invokeTauri('model_store_set_storage_settings', { modelsDir, importScanDirs });
}

export {
  storeAvailability,
  takeStartupCleanup,
  fetchStorageSettings,
  saveStorageSettings,
  fetchStoreCards,
  fetchLibrary,
  fetchModelDetail,
  fetchInstalledEngines,
  fetchLocalResources,
  fetchMemoryAdmission,
  deleteInstalledEngine,
  fetchApiKeys,
  createApiKey,
  fetchLanStatus,
  setLanSharing,
  revokeApiKey,
  startDownload,
  startCardScripts,
  startDeploy,
  startEnable,
  startStop,
  startUninstall,
  forceRemoveModel,
  pollRunLog,
  cancelRun,
  answerRun,
  revealRunLogsDir,
  exportRunLog,
  fetchActiveRuns,
};
