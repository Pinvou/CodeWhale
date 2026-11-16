/**
 * artifacts feature for the Tauri bridge.
 * Registered before bridge.js builds the backwards-compatible facade.
 */
(function (root) {
  "use strict";
  // biome-ignore lint/suspicious/noAssignInExpressions: registry bootstrap of the verbatim payload; splitting statements would diverge from the artifact
  const registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["artifacts"] = function (context) {let pinvouSharedtauriArtifactsCache = null;
function pinvouSharedtauriArtifacts() {
  if (!pinvouSharedtauriArtifactsCache) pinvouSharedtauriArtifactsCache = window.PinvouBridgeShared.create("tauriArtifacts", { invoke, addSystemItem, bt, state, sessionStates, isDeliverable, basename, artifactInfo, dialogOpen, addAttachmentByPath });
  return pinvouSharedtauriArtifactsCache;
}


    var state = context.state;
    var notify = context.notify;
    var invoke = context.invoke;
    var bt = context.bt;
    var addSystemItem = context.addSystemItem;
    var dialogOpen = context.dialogOpen;
    var basename = context.basename;
    var isDeliverable = context.isDeliverable;
    var sessionStates = context.sessionStates;
    var discardManagedAttachment = context.discardManagedAttachment || function () { return Promise.resolve(); };
    var attachIdSeq = 0;
    function telemetryToken(prefix) {
      if (root.crypto && typeof root.crypto.randomUUID === "function") {
        return prefix + root.crypto.randomUUID().replace(/-/g, ""); // safari14-ok: guarded above
      }
      if (root.crypto && typeof root.crypto.getRandomValues === "function") {
        var bytes = new Uint8Array(12);
        root.crypto.getRandomValues(bytes);
        return prefix + Array.from(bytes, function (byte) { return byte.toString(16).padStart(2, "0"); }).join("");
      }
      return prefix + Date.now().toString(36) + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2);
    }
    function fileTypeBucket(name) {
      var ext = (String(name || "").split(".").pop() || "").toLowerCase();
      if (["doc", "docx", "md", "txt", "rtf"].indexOf(ext) >= 0) return "document";
      if (["xls", "xlsx", "csv", "tsv"].indexOf(ext) >= 0) return "spreadsheet";
      if (["ppt", "pptx", "odp"].indexOf(ext) >= 0) return "presentation";
      if (ext === "pdf") return "pdf";
      if (["png", "jpg", "jpeg", "gif", "webp", "svg", "bmp", "heic"].indexOf(ext) >= 0) return "image";
      if (["zip", "7z", "rar", "tar", "gz"].indexOf(ext) >= 0) return "archive";
      if (["js", "jsx", "ts", "tsx", "py", "rs", "go", "java", "c", "cpp", "h", "html", "css", "json", "yaml", "yml"].indexOf(ext) >= 0) return "code";
      return "other";
    }
    function sizeBucket(size) {
      var bytes = Number(size) || 0;
      if (bytes <= 1024 * 1024) return "0_1mib";
      if (bytes <= 10 * 1024 * 1024) return "1_10mib";
      if (bytes <= 100 * 1024 * 1024) return "10_100mib";
      return "100mib_plus";
    }
    function attachmentErrorCategory(error) {
      return root.PinvouAttachmentTelemetry
        ? root.PinvouAttachmentTelemetry.errorCategory(error) : "internal_error";
    }
    function attachmentPhase(att, phase) {
      var attempt = att && att.telemetryAttempt;
      if (attempt && !attempt.finished && root.PinvouAttachmentTelemetry) {
        attempt.step = root.PinvouAttachmentTelemetry.step(phase);
      }
    }
    function beginAttachmentTelemetry(att, name, size, entryPoint, phase) {
      var attempt = {
        attachmentId: telemetryToken("attachment_"),
        operationId: telemetryToken("attachment_op_"),
        fileTypeBucket: fileTypeBucket(name),
        startedAt: Date.now(),
        finished: false,
        step: phase === "ingest" ? "attachment_ingest" : "attachment_validation",
      };
      att.telemetryAttempt = attempt;
      invoke("track_behavior_event", { request: {
        eventName: "attachment_added", attachmentId: attempt.attachmentId,
        operationId: attempt.operationId, attemptIndex: 1,
        fileTypeBucket: attempt.fileTypeBucket, sizeBucket: sizeBucket(size),
        entryPoint: entryPoint,
        stage: "submission", step: attempt.step, inputType: "attachment",
      } }).catch(function () {});
      return attempt;
    }
    function finishAttachmentTelemetry(att, success, errorCategory) {
      var attempt = att && att.telemetryAttempt;
      if (!attempt || attempt.finished) return;
      attempt.finished = true;
      invoke("track_behavior_event", { request: {
        eventName: "attachment_processing_completed", attachmentId: attempt.attachmentId,
        operationId: attempt.operationId, attemptIndex: 1,
        fileTypeBucket: attempt.fileTypeBucket, success: !!success,
        durationMs: Math.max(0, Date.now() - attempt.startedAt),
        errorCategory: success ? undefined : (errorCategory || "internal_error"),
        stage: "submission", step: attempt.step, inputType: "attachment",
      } }).catch(function () {});
    }
  // ── 产物面板 ─────────────────────────────────────────────────────
  function artifactInfo(path) { return invoke("artifact_info", { path: path }); }
  function readArtifactText(path) { return invoke("read_artifact_text", { path: path }); }
  function writeArtifactText(path, content) { return invoke("write_artifact_text", { path: path, content: content }); }
  function readArtifactImageB64(path) { return invoke("read_artifact_image_b64", { path: path }); }
  // pptx 封面缩略图：读 docProps/thumbnail.jpeg → data URL（无则 null）。本地数据、无外链。
  function readArtifactThumbnail(path) { return invoke("read_artifact_thumbnail", { path }).catch(function () { return null; }); }
  function renderArtifactVisual(path) { return invoke("render_artifact_visual", { path }); }
function openContainingFolder(path) { return pinvouSharedtauriArtifacts().openContainingFolder(path); }
function revealSessionFolder(sessionId) { return pinvouSharedtauriArtifacts().revealSessionFolder(sessionId); }
function openScheduledTaskFolder(automationId) { return pinvouSharedtauriArtifacts().openScheduledTaskFolder(automationId); }
  // ACP 消息/产物预览里由用户亲自点击的 HTTP(S) 外链；后端与工具白名单入口分开校验。
  function openUserExternalUrl(url) { return invoke("open_user_external_url", { url }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  // 跨会话产出物索引:磁盘 session JSON 为主,再合并当前内存工作集。
  // 新产物在 chat:done/save_session_artifacts 前也能立刻出现在「产出物」一级入口。
  // #701 T4b 壳化：官方 v2 命令（list_deliverable_index_v2）已随 TB 入主线
  //（src-tauri/src/app/commands/artifacts.rs + lib.rs 注册），KnowledgeView 亦为
  // accept-both 双形状 —— lane 改委托共享核（helpers v2 协商版；桌面 webview
  // 与后端同包发布、version-locked，lane 不传 hasCommand 时按 v2 可用处理）。
  // 共享合并核保留跳过记录，整体扫描失败如实上抛到 Knowledge 视图。
  async function listDeliverableIndex() { return pinvouSharedtauriArtifacts().listDeliverableIndex(); }
  // 外部打开产物：HTML 走 Tauri 独立窗口（绕沙箱），其他走系统应用。
  // sessionId = 卡片携带的产物所属 session。后端 resolve_artifact_path 用它(而非全局
  // active_id)解析相对路径 —— 切回「有 buffer」的会话后端 active 不更新,只有卡片自带
  // session 才解析得准(否则相对路径被拼到错的 workspace 报 not a file)。绝对路径无视它。
  function openArtifactExternal(path, sessionId) {
    var ext = (String(path).split(".").pop() || "").toLowerCase();
    var cmd = (ext === "html" || ext === "htm") ? "open_artifact_window" : "open_in_system";
    return invoke(cmd, { path: path, sessionId: sessionId || null }).then(function (result) {
      trackArtifactExport(path, sessionId);
      return result;
    }).catch(function (e) { addSystemItem(bt("openFailed") + e); return false; });
  }
  function artifactExportFormat(path) {
    var ext = (String(path || "").split(".").pop() || "").toLowerCase();
    if (["docx", "xlsx", "pptx", "pdf", "png", "html"].indexOf(ext) >= 0) return ext;
    if (ext === "htm") return "html";
    if (ext === "md") return "markdown";
    return "other";
  }
  function artifactsForSession(sessionId) {
    var targetSessionId = sessionId || state.activeSessionId;
    if (!targetSessionId) return [];
    if (targetSessionId === state.activeSessionId) return state.artifacts || [];
    return sessionStates[targetSessionId] && sessionStates[targetSessionId].artifacts || [];
  }
  function trackArtifactExport(path, sessionId) {
    var bn = basename(path);
    var candidates = artifactsForSession(sessionId);
    var exactMatches = candidates.filter(function (item) {
      return String(item.path || "") === String(path || "");
    });
    var basenameMatches = exactMatches.length ? [] : candidates.filter(function (item) {
      return basename(item.path) === bn;
    });
    var artifact = exactMatches.length === 1
      ? exactMatches[0]
      : (basenameMatches.length === 1 ? basenameMatches[0] : null);
    if (!artifact || !artifact.telemetryId) return;
    invoke("track_behavior_event", { request: {
      eventName: "artifact_exported", artifactId: artifact.telemetryId,
      exportFormat: artifactExportFormat(path),
    } }).catch(function () {});
  }
  function downloadArtifact(path, sessionId) {
    return openArtifactExternal(path, sessionId);
  }

  // ── 附件 ────────────────────────────────────────────────────────
  async function addAttachmentByPath(path) {
    var id = ++attachIdSeq;
    var att = { id: id, basename: basename(path), status: "parsing", result: null, error: null, cancelled: false };
    state.attachments.push(att); notify();
    try {
      var info = await artifactInfo(path).catch(function () { return null; });
      beginAttachmentTelemetry(att, att.basename, info && info.size, "composer", "ingest");
      var result = await invoke("ingest_file", { path: path });
      if (att.cancelled) { finishAttachmentTelemetry(att, false, "user_cancelled"); return; }
      att.status = "ready"; att.result = result;
      finishAttachmentTelemetry(att, true);
    } catch (e) { att.status = "error"; att.error = String(e); finishAttachmentTelemetry(att, false, attachmentErrorCategory(e)); }
    notify();
  }
  async function addDroppedFileAttachment(file) {
    if (!file) return;
    var id = ++attachIdSeq;
    var att = { id: id, basename: file.name || "attachment", status: "parsing", result: null, error: null, cancelled: false, uploadId: null };
    state.attachments.push(att);
    beginAttachmentTelemetry(att, att.basename, file.size, "composer");
    notify();
    try {
      var uploader = root.PinvouChunkedFileUpload;
      if (!uploader || typeof uploader.uploadFile !== "function") {
        throw Object.assign(new Error("chunked attachment uploader is unavailable"), { code: "attachment_uploader_unavailable" });
      }
      var uploadId = uploader.uploadId("desktop_attach");
      att.uploadId = uploadId;
      var completed = await uploader.uploadFile({
        file: file,
        uploadId: uploadId,
        isCancelled: function () { return att.cancelled; },
        onPhase: function (phase) { attachmentPhase(att, phase); },
        sendChunk: function (chunk) {
          return invoke("ingest_draft_file_chunk", {
            uploadId: chunk.uploadId,
            filename: chunk.fileName,
            offset: chunk.offset,
            total: chunk.total,
            dataBase64: chunk.dataBase64,
            commit: chunk.commit,
            ...(chunk.sha256 ? { sha256: chunk.sha256 } : {}),
          });
        },
        validateResult: function (result) { return Boolean(result && result.basename); },
        cleanup: function (upload) {
          // 后端命令失败时已清理 staging。只有用户取消，或最后一块已确认后
          // 前端校验失败，才允许删除这个 uploadId 对应的已完成目录。
          if (att.cancelled || upload.commitAcknowledged) {
            return invoke("cancel_draft_file_upload", { uploadId: upload.uploadId });
          }
        },
      });
      var result = completed.result;
      Object.defineProperty(result, "__pinvouManagedDraftAttachmentId", {
        configurable: true,
        value: uploadId,
        enumerable: false,
      });
      att.basename = result.basename || att.basename;
      att.status = "ready";
      att.result = result;
      finishAttachmentTelemetry(att, true);
    } catch (e) {
      att.status = "error";
      att.error = e && e.code === "device_upload_empty"
        ? bt("attachEmptyFile")
        : e && e.code === "device_upload_too_large"
          ? "attachment_file_too_large"
          : e && e.code === "device_upload_cancelled"
            ? bt("attachAddCancelled")
            : e && e.code === "device_upload_invalid_result"
              ? bt("attachInvalidResult")
              : String(e);
      finishAttachmentTelemetry(att, false, attachmentErrorCategory(e));
    }
    notify();
  }

function conversationAttachmentArgs(reference) { return pinvouSharedtauriArtifacts().conversationAttachmentArgs(reference); }
  function resolveConversationAttachment(reference) {
    return invoke("resolve_conversation_attachment", conversationAttachmentArgs(reference));
  }
  function openConversationAttachment(reference) {
    return invoke("open_conversation_attachment", conversationAttachmentArgs(reference))
      .catch(function (e) { addSystemItem(bt("openFailed") + e); return false; });
  }
  function revealConversationAttachment(reference) {
    return invoke("reveal_conversation_attachment", conversationAttachmentArgs(reference))
      .catch(function (e) { addSystemItem(bt("openFailed") + e); return false; });
  }

  async function addPasteImage(filename, bytes, formatError) {
    try {
      var path = await invoke("save_paste_image", { filename: filename, bytes: bytes });
      await addAttachmentByPath(path);
    } catch (e) {
      const limitError = typeof formatError === "function" ? formatError(e) : "";
      addSystemItem(limitError || bt("pasteImageFailed") + e);
    }
  }
  // Linux WebKitGTK 的 paste 事件不携带图像数据，由原生层读剪贴板 → 落盘 → 返回 path
  // （图像字节不过 IPC）。无图像返回 null 并静默——与「剪贴板无内容可贴」既有行为一致；
  // 落盘/读剪贴板失败走 addPasteImage 同款报错。
  async function addPasteImageFromClipboard(formatError) {
    try {
      const path = await invoke("paste_clipboard_image");
      if (!path) return null;
      await addAttachmentByPath(path);
      return path;
    } catch (e) {
      const limitError = typeof formatError === "function" ? formatError(e) : "";
      addSystemItem(limitError || bt("pasteImageFailed") + e);
      return null;
    }
  }
  function removeAttachment(id) {
    var removed = state.attachments.find(function (a) { return a.id === id; });
    if (removed) {
      removed.cancelled = true;
      finishAttachmentTelemetry(removed, false, "user_cancelled");
      if (removed.status === "ready" && removed.result) {
        discardManagedAttachment(removed.result);
      }
    }
    state.attachments = state.attachments.filter(function (a) { return a.id !== id; });
    notify();
  }
  // 打开系统文件选择器并摄入为附件
async function pickAndAttach() { return pinvouSharedtauriArtifacts().pickAndAttach(); }
  // 文件选择按钮在桌面仍走原生路径；HTML5 拖放拿不到路径时通过同一域方法
  // 分块写入 sessionless 草稿区，直到实际发送才归属到目标会话。
  async function uploadDeviceFiles(files) {
    var list = Array.prototype.slice.call(files || []).filter(Boolean);
    for (var index = 0; index < list.length; index++) {
      await addDroppedFileAttachment(list[index]);
    }
  }

  async function adoptManagedAttachments(attachments, sessionId) {
    var list = Array.prototype.slice.call(attachments || []);
    for (var index = 0; index < list.length; index++) {
      var attachment = list[index];
      var result = attachment && attachment.result;
      var uploadId = result && result.__pinvouManagedDraftAttachmentId;
      if (!uploadId) continue;
      var adopted = await invoke("adopt_draft_attachment", {
        sessionId: sessionId,
        uploadId: uploadId,
      });
      Object.defineProperty(adopted, "__pinvouManagedAttachmentSessionId", {
        configurable: true,
        value: sessionId,
        enumerable: false,
      });
      attachment.result = adopted;
      attachment.basename = adopted.basename || attachment.basename;
    }
    return list;
  }


    return {
      artifactInfo: artifactInfo,
      readArtifactText: readArtifactText,
      writeArtifactText: writeArtifactText,
      readArtifactImageB64: readArtifactImageB64,
      readArtifactThumbnail: readArtifactThumbnail,
      renderArtifactVisual: renderArtifactVisual,
      openContainingFolder: openContainingFolder,
      revealSessionFolder: revealSessionFolder,
      openScheduledTaskFolder: openScheduledTaskFolder,
      openArtifactExternal: openArtifactExternal,
      downloadArtifact: downloadArtifact,
      listDeliverableIndex: listDeliverableIndex,
      openUserExternalUrl: openUserExternalUrl,
      addAttachmentByPath: addAttachmentByPath,
      addPasteImage: addPasteImage,
      addPasteImageFromClipboard: addPasteImageFromClipboard,
      removeAttachment: removeAttachment,
      pickAndAttach: pickAndAttach,
      uploadDeviceFiles: uploadDeviceFiles,
      adoptManagedAttachments: adoptManagedAttachments,
      resolveConversationAttachment: resolveConversationAttachment,
      openConversationAttachment: openConversationAttachment,
      revealConversationAttachment: revealConversationAttachment
    };
  };
})(window);
