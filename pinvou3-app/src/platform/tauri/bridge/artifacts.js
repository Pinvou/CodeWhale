/**
 * artifacts feature for the Tauri bridge.
 * Registered before bridge.js builds the backwards-compatible facade.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["artifacts"] = function (context) {
    var state = context.state;
    var notify = context.notify;
    var invoke = context.invoke;
    var bt = context.bt;
    var addSystemItem = context.addSystemItem;
    var dialogOpen = context.dialogOpen;
    var basename = context.basename;
    var isDeliverable = context.isDeliverable;
    var isAbsPath = context.isAbsPath;
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
    function beginAttachmentTelemetry(att, name, size, entryPoint) {
      var attempt = {
        attachmentId: telemetryToken("attachment_"),
        operationId: telemetryToken("attachment_op_"),
        fileTypeBucket: fileTypeBucket(name),
        startedAt: Date.now(),
        finished: false,
      };
      att.telemetryAttempt = attempt;
      invoke("track_behavior_event", { request: {
        eventName: "attachment_added", attachmentId: attempt.attachmentId,
        operationId: attempt.operationId, attemptIndex: 1,
        fileTypeBucket: attempt.fileTypeBucket, sizeBucket: sizeBucket(size),
        entryPoint: entryPoint,
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
        errorCategory: success ? undefined : (errorCategory || "validation_failed"),
      } }).catch(function () {});
    }
  // ── 产物面板 ─────────────────────────────────────────────────────
  function artifactInfo(path) { return invoke("artifact_info", { path: path }); }
  function readArtifactText(path) { return invoke("read_artifact_text", { path: path }); }
  function writeArtifactText(path, content) { return invoke("write_artifact_text", { path: path, content: content }); }
  function readArtifactImageB64(path) { return invoke("read_artifact_image_b64", { path: path }); }
  // pptx 封面缩略图：读 docProps/thumbnail.jpeg → data URL（无则 null）。本地数据、无外链。
  function readArtifactThumbnail(path) { return invoke("read_artifact_thumbnail", { path: path }).catch(function () { return null; }); }
  function renderArtifactVisual(path) { return invoke("render_artifact_visual", { path: path }); }
  function openContainingFolder(path) { return invoke("open_containing_folder", { path: path }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  function revealSessionFolder(sessionId) { return invoke("reveal_session_folder", { sessionId: sessionId }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  function openScheduledTaskFolder(automationId) { return invoke("open_scheduled_task_folder", { automationId: automationId }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  function openInSystem(path) { return invoke("open_in_system", { path: path }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  // 仅放白名单 URL (metaso.cn / open.bochaai.com),后端 open_external_url 强制校验。
  function openExternalUrl(url) { return invoke("open_external_url", { url: url }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  // ACP 消息/产物预览里由用户亲自点击的 HTTP(S) 外链；后端与工具白名单入口分开校验。
  function openUserExternalUrl(url) { return invoke("open_user_external_url", { url: url }).catch(function (e) { addSystemItem(bt("openFailed") + e); }); }
  function deliverableCategory(path) {
    var ext = (String(path || "").split(".").pop() || "").toLowerCase();
    if (ext === "html" || ext === "htm" || ext === "mhtml" || ext === "mht") return "web";
    if (ext === "ppt" || ext === "pptx" || ext === "odp" || ext === "dps") return "ppt";
    if (["png", "jpg", "jpeg", "gif", "webp", "svg", "bmp", "heic"].indexOf(ext) >= 0) return "img";
    return "doc";
  }
  function sessionTitleById(sid) {
    var m = state.sessions.find(function (s) { return s.id === sid; });
    return (m && m.title) || "";
  }
  function currentMemoryArtifacts() {
    var rows = [];
    function addFrom(sid, arts) {
      (arts || []).forEach(function (a) {
        var path = a && a.path;
        if (!path || !isDeliverable(path)) return;
        rows.push({ path: path, sessionId: sid || state.activeSessionId, source: sessionTitleById(sid || state.activeSessionId), name: basename(path) });
      });
    }
    addFrom(state.activeSessionId, state.artifacts);
    Object.keys(sessionStates).forEach(function (sid) { addFrom(sid, sessionStates[sid] && sessionStates[sid].artifacts); });
    return rows;
  }
  // 跨会话产出物索引:磁盘 session JSON 为主,再合并当前内存工作集。
  // 新产物在 chat:done/save_session_artifacts 前也能立刻出现在「产出物」一级入口。
  async function listDeliverableIndex() {
    var disk = await invoke("list_deliverable_index").catch(function () { return []; });
    var byPath = {};
    (disk || []).forEach(function (x) { if (x && x.path) byPath[x.path] = x; });
    var mem = currentMemoryArtifacts().filter(function (x) { return x.path && !byPath[x.path]; });
    var hydrated = await Promise.all(mem.map(async function (x) {
      var path = x.path;
      if (!isAbsPath(path) && x.sessionId) {
        try {
          var ws = await invoke("list_workspace_files", { sessionId: x.sessionId });
          var bn = basename(path);
          var resolved = (ws || []).find(function (p) { return basename(p) === bn; });
          if (resolved) path = resolved;
        } catch (_) {}
      }
      var info = null;
      try { info = await artifactInfo(path); } catch (_) {}
      var ext = (String(path).split(".").pop() || "").toLowerCase();
      return {
        name: x.name || basename(path),
        path: path,
        ext: ext,
        category: deliverableCategory(path),
        sessionId: x.sessionId || "",
        source: x.source || sessionTitleById(x.sessionId) || "",
        mtime: info && info.modified ? info.modified : 0,
        size: info && info.size ? info.size : 0,
      };
    }));
    hydrated.forEach(function (x) { if (x && x.path) byPath[x.path] = x; });
    return Object.keys(byPath).map(function (p) { return byPath[p]; }).sort(function (a, b) {
      return (b.mtime || 0) - (a.mtime || 0) || String(a.name || "").localeCompare(String(b.name || ""));
    });
  }
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
      beginAttachmentTelemetry(att, att.basename, info && info.size, "composer");
      var result = await invoke("ingest_file", { path: path });
      if (att.cancelled) { finishAttachmentTelemetry(att, false, "user_cancelled"); return; }
      att.status = "ready"; att.result = result;
      finishAttachmentTelemetry(att, true);
    } catch (e) { att.status = "error"; att.error = String(e); finishAttachmentTelemetry(att, false, "validation_failed"); }
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
        throw new Error("chunked attachment uploader is unavailable");
      }
      var uploadId = uploader.uploadId("desktop_attach");
      att.uploadId = uploadId;
      var completed = await uploader.uploadFile({
        file: file,
        uploadId: uploadId,
        isCancelled: function () { return att.cancelled; },
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
      finishAttachmentTelemetry(att, false, e && e.code === "device_upload_cancelled" ? "user_cancelled" : "validation_failed");
    }
    notify();
  }

  function conversationAttachmentArgs(reference) {
    reference = reference || {};
    return {
      sessionId: reference.sessionId || state.activeSessionId,
      messageIndex: Number(reference.messageIndex),
      attachmentIndex: Number(reference.attachmentIndex),
      basename: String(reference.basename || ""),
      displayText: String(reference.displayText || ""),
    };
  }
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
  function clearAttachments() {
    state.attachments.forEach(function (attachment) {
      attachment.cancelled = true;
      finishAttachmentTelemetry(attachment, false, "user_cancelled");
      if (attachment.status === "ready" && attachment.result) {
        discardManagedAttachment(attachment.result);
      }
    });
    state.attachments = [];
  }
  // 打开系统文件选择器并摄入为附件
  async function pickAndAttach() {
    if (!dialogOpen) { addSystemItem(bt("filePickUnavailable")); return; }
    try {
      var selected = await dialogOpen({ multiple: true });
      if (!selected) return;
      var paths = Array.isArray(selected) ? selected : [selected];
      for (var i = 0; i < paths.length; i++) { await addAttachmentByPath(paths[i]); }
    } catch (e) { addSystemItem(bt("filePickFailed") + e); }
  }
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
      openInSystem: openInSystem,
      openArtifactExternal: openArtifactExternal,
      downloadArtifact: downloadArtifact,
      listDeliverableIndex: listDeliverableIndex,
      openExternalUrl: openExternalUrl,
      openUserExternalUrl: openUserExternalUrl,
      addAttachmentByPath: addAttachmentByPath,
      addPasteImage: addPasteImage,
      addPasteImageFromClipboard: addPasteImageFromClipboard,
      removeAttachment: removeAttachment,
      clearAttachments: clearAttachments,
      pickAndAttach: pickAndAttach,
      uploadDeviceFiles: uploadDeviceFiles,
      adoptManagedAttachments: adoptManagedAttachments,
      resolveConversationAttachment: resolveConversationAttachment,
      openConversationAttachment: openConversationAttachment,
      revealConversationAttachment: revealConversationAttachment
    };
  };
})(window);
