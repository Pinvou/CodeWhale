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
 * 商店在架卡片。`testMode` 走的是打包在应用内的测试卡片——只有开启了测试商店
 * 并把数据源切过去时才传 true，默认永远是正式商店。
 * @param {boolean} [testMode] - 是否测试模式。
 * @returns {Promise<Array<object>>} 卡片摘要列表。
 */
function fetchStoreCards(testMode = false) {
  return invokeTauri('model_store_cards', { testMode });
}

/**
 * 测试商店的开关状态。测试商店的卡片和脚本打包在应用内、不依赖商店服务器，
 * 只给开发验证脚本本身用，所以它默认是关的、且必须凭口令才能打开。
 * @returns {Promise<{enabled:boolean}>} 当前是否已开启。
 */
function fetchTestStoreState() {
  return invokeTauri('model_store_test_store_state');
}

/**
 * 用口令开启测试商店。
 *
 * 口令不对回的是 `{ok:false}` 而**不是**异常——这条路径上「口令错」是一个正常
 * 结果，不该和「配置写不进去」混成同一个 catch 分支，那样界面只能给出一句含糊的
 * 「失败」。
 * @param {string} password - 口令。
 * @returns {Promise<{ok:boolean}>} ok 为假表示口令不对。
 */
function unlockTestStore(password) {
  return invokeTauri('model_store_unlock_test_store', { password });
}

/**
 * 关闭测试商店。关掉之后连入口都不再提示，重开要再输一次口令。
 * @returns {Promise<{enabled:boolean}>} 关闭后的状态。
 */
function disableTestStore() {
  return invokeTauri('model_store_disable_test_store');
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

/**
 * 发起下载。长操作：立刻返回 run_id，进度靠 `pollRunLog` 读。
 * @param {string} cardId - 卡片 ID。
 * @param {boolean} [testMode] - 是否测试模式。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startDownload(cardId, testMode = false) {
  return invokeTauri('model_store_start_download', { cardId, testMode });
}

/**
 * 发起部署。
 * @param {string} cardId - 卡片 ID。
 * @param {string|null} [profileId] - 部署方案 ID；单方案卡片传 null。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startDeploy(cardId, profileId = null) {
  return invokeTauri('model_store_start_deploy', { cardId, profileId });
}

/**
 * 启用（拉起推理容器）。
 * @param {string} cardId - 卡片 ID。
 * @returns {Promise<{run_id:string}>} 运行句柄。
 */
function startEnable(cardId) {
  return invokeTauri('model_store_start_lifecycle', { cardId, action: 'enable' });
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
  fetchStorageSettings,
  saveStorageSettings,
  fetchStoreCards,
  fetchTestStoreState,
  unlockTestStore,
  disableTestStore,
  fetchLibrary,
  fetchModelDetail,
  fetchApiKeys,
  createApiKey,
  startDownload,
  startDeploy,
  startEnable,
  startStop,
  startUninstall,
  forceRemoveModel,
  pollRunLog,
  cancelRun,
  answerRun,
};
