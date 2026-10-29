/**
 * updater feature for the Tauri bridge.
 * Registered before bridge.js builds the backwards-compatible facade.
 */
(function (root) {
  // biome-ignore lint/suspicious/noRedundantUseStrict: verbatim classic-script artifact; strict mode is part of the payload
  "use strict";
  // biome-ignore lint/suspicious/noAssignInExpressions: registry bootstrap of the verbatim payload; splitting statements would diverge from the artifact
  const registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["updater"] = function (context) {let pinvouSharedtauriUpdaterCache = null;
function pinvouSharedtauriUpdater() {
  if (!pinvouSharedtauriUpdaterCache) pinvouSharedtauriUpdaterCache = window.PinvouBridgeShared.create("tauriUpdater", { state, notify, invoke });
  return pinvouSharedtauriUpdaterCache;
}


  const state = context.state;
  const notify = context.notify;
  const invoke = context.invoke;
  const listen = context.listen;
  let progressTimer = null;
  let windowsDownload = false;
  function clearProgressTimer() {
    if (progressTimer !== null) root.clearTimeout(progressTimer);
    progressTimer = null;
  }
  listen("update:progress", function (event) {
    if (!windowsDownload || !state.updateDownloading || state.updateCancelling || state.updateProgress >= 100) return;
    const payload = event && event.payload || {};
    const total = Number(payload.total), downloaded = Number(payload.downloaded);
    if (!Number.isFinite(total) || total <= 0 || !Number.isFinite(downloaded) || downloaded < 0) return;
    const progress = Math.min(99, Math.floor(downloaded / total * 100));
    if (progress <= state.updateProgress) return;
    state.updateProgress = progress;
    if (progressTimer === null) progressTimer = root.setTimeout(function () {
      progressTimer = null;
      notify();
    }, 200);
  });
  // ── 应用内升级 ───────────────────────────────────────────────────
  // Chain: check_for_update (compares the server's latest.json) → download_update (streamed download + sha256)
  // → update:progress → install_update → restart_app.
  async function loadAppVersion() {
    try {
      state.appVersion = await invoke("get_app_version");
    } catch { /* leaving the version empty on read failure is fine */ }
  }
  // 启动静默检查: 失败全吞(网络差/更新源挂了不打扰用户)。结果不管新旧都存——
  // available 驱动红点,current_version 给设置页显示当前版本用。
  async function checkForUpdateSilently() {
    try {
      const info = await invoke("check_for_update");
      if (info && info.current_version) state.appVersion = info.current_version;
      if (info) { state.updateInfo = info; notify(); }
    } catch { /* 静默 */ }
  }
  // 设置页手动检查: 错误和「已是最新」都要反馈。
async function checkForUpdate() { return pinvouSharedtauriUpdater().checkForUpdate(); }
  // Windows 使用清单中的 NSIS/MSI 安装器；Linux/macOS 安装后由前端重启。
  async function downloadAndInstallUpdate() {
    if (!state.updateInfo || !state.updateInfo.available || state.updateDownloading) return false;
    // 入口捕获发起时的更新信息：下载/安装期间静默检查可能替换 updateInfo，
    // download/install 参数须仍指向发起时的版本。
    const info = state.updateInfo;
    const shouldRestartAfterInstall = info.platform !== "windows";
    let installed = false;
    state.updateDownloading = true; state.updateCancelling = false;
    windowsDownload = info.platform === "windows";
    clearProgressTimer();
    state.updateProgress = 0; state.updateError = null; notify();
    try {
      const downloadResult = await invoke("download_update", { info: state.updateInfo });
      clearProgressTimer();
      state.updateProgress = 100; notify();
      state.updateInfo = info; // 复原：install 用发起时的版本元数据，不随静默检查漂移
      if (downloadResult && typeof downloadResult === "object" && downloadResult.installer_path) {
        await invoke("install_update", { installerPath: downloadResult.installer_path, info: state.updateInfo });
      } else {
        await invoke("install_update", { debPath: downloadResult, info: state.updateInfo });
      }
      state.updateReady = true;
      installed = true;
    } catch (e) {
      // 用户主动取消下载时后端返回「已取消下载」,当正常处理不弹错误
      if (state.updateCancelling) state.updateProgress = 0;
      else state.updateError = String(e);
    }
    clearProgressTimer();
    windowsDownload = false;
    state.updateDownloading = false; state.updateCancelling = false; notify();
    if (installed && shouldRestartAfterInstall) restartApp();
    return installed;
  }
  // 取消进行中的下载: 置前端标志 + 通知后端中断下载循环。仅下载阶段有效;
  // 已进入 install(pkexec/apt)则无效(系统接管,装一半不能停)。
  function cancelUpdate() {
    if (!state.updateDownloading || state.updateCancelling) return;
    clearProgressTimer();
    state.updateCancelling = true; notify();
    invoke("cancel_download").catch(function () { /* 忽略,下载循环超时也会退 */ });
  }
  function restartApp() {
    invoke("restart_app").catch(function () { /* restart 成功不会返回 */ });
  }
  function reportPendingUpdateResult() {
    invoke("report_pending_update_result").catch(function () { /* 静默重试,不阻塞启动 */ });
  }

    return {
      loadAppVersion,
      checkForUpdateSilently,
      checkForUpdate,
      downloadAndInstallUpdate,
      cancelUpdate,
      restartApp,
      reportPendingUpdateResult
    };
  };
})(window);
