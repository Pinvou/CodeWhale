/** Content-free attachment failure facts shared by the desktop and Web hosts. */
(function (root) {
  "use strict";

  var categories = Object.freeze({
    device_upload_invalid: "validation_failed",
    device_upload_empty: "validation_failed",
    device_upload_too_large: "validation_failed",
    attachment_file_too_large: "validation_failed",
    attachment_archive_too_many_entries: "validation_failed",
    attachment_archive_expanded_too_large: "validation_failed",
    attachment_archive_unsafe_entry: "validation_failed",
    web_attachment_digest_invalid: "validation_failed",
    web_attachment_integrity_mismatch: "validation_failed",
    device_upload_invalid_result: "internal_error",
    attachment_uploader_unavailable: "configuration_missing",
    permission_denied: "permission_denied",
    EACCES: "permission_denied",
    EPERM: "permission_denied",
    authentication_failed: "authentication_failed",
    configuration_missing: "configuration_missing",
    unsupported: "unsupported",
    network_unavailable: "network_unavailable",
    desktop_offline: "network_unavailable",
    desktop_capabilities_unavailable: "network_unavailable",
    ECONNRESET: "network_unavailable",
    ECONNREFUSED: "network_unavailable",
    ENETUNREACH: "network_unavailable",
    ENOTFOUND: "network_unavailable",
    ETIMEDOUT: "timeout",
    request_timeout: "timeout",
    web_rpc_timeout: "timeout",
    rpc_timeout: "timeout",
    desktop_capabilities_timeout: "timeout",
  });
  var steps = Object.freeze({
    validation: "attachment_validation",
    read: "attachment_read",
    upload: "attachment_upload",
    commit: "attachment_commit",
    result: "attachment_result",
    ingest: "attachment_ingest",
  });
  function registeredCategory(code) {
    return typeof code === "string" && Object.prototype.hasOwnProperty.call(categories, code)
      ? categories[code] : null;
  }
  function legacyReasonCategory(reason) {
    // Match complete reasons, never a word that could be part of a filename.
    // OS error numbers alone are ambiguous across the supported platforms.
    if (/^(?:EACCES|EPERM)(?::|$)/.test(reason)) return "permission_denied";
    if (/^(?:permission denied|operation not permitted|access is denied[.]?|拒绝访问[。]?|权限不足|アクセスが拒否されました[。]?)(?: \(os error \d+\))?$/i.test(reason)) return "permission_denied";
    if (/^(?:(?:connection |request )?timed out|timeout)(?: \(os error \d+\))?$/i.test(reason)) return "timeout";
    if (/^RPC timed out(?::|$)|^请求超时$/i.test(reason)) return "timeout";
    if (/^(?:connection reset by peer|connection refused|network is unreachable|failed to fetch|networkerror)(?: \(os error \d+\))?$/i.test(reason)) return "network_unavailable";
    if (/^websocket (?:closed|disconnected)(?::|$)/i.test(reason)) return "network_unavailable";
    return "internal_error";
  }
  function legacyCategory(text) {
    if (/^(?:附件文件名(?:无效|不得包含路径)|附件为空|附件完整性校验值无效|附件完整性校验失败，上传内容在传输中损坏)$/.test(text)) return "validation_failed";
    if (/^path must be absolute:|^path must not contain '\.\.' when it cannot be resolved:/.test(text)) return "validation_failed";
    if (/^path [\s\S]+ (?:not under \$HOME|is in system-sensitive area)$/.test(text)) return "permission_denied";
    if (/^path [\s\S]+ crosses sensitive component(?: prefix)? [^\s\\/]+$/.test(text)) return "permission_denied";
    if (/^path [\s\S]+ under sensitive system dir [^\s\\/]+$/.test(text)) return "permission_denied";
    // Native path errors contain user-controlled names. Isolate the last
    // metadata error delimiter before interpreting its OS-provided reason.
    var unreadable = /^path [\s\S]+ is not readable: ([^\r\n]+)$/.exec(text);
    if (unreadable) return legacyReasonCategory(unreadable[1]);
    if (/^path /.test(text)) return "internal_error";
    // These existing wrappers interpolate only std::io::Error, not a path.
    var io = /^(?:(?:清理附件目录|创建附件暂存目录|打开附件暂存文件|写入附件分块|刷新附件分块|读取附件暂存文件|创建会话附件目录|提交会话附件|读取附件引用|创建附件引用目录|读取附件草稿|读取会话附件|删除会话附件)失败：|(?:create attachment upload dir|write uploaded attachment): )([^\r\n]+)$/.exec(text);
    return legacyReasonCategory(io ? io[1] : text);
  }
  function errorCategory(error) {
    try {
      // Only the uploader's typed cancellation is evidence of user intent.
      if (error && error.code === "device_upload_cancelled") return "user_cancelled";
      var explicit = registeredCategory(error && error.code);
      if (explicit) return explicit;
      var text = String(error && error.message ? error.message : error || "");
      if (text.length > 8192) return "internal_error";
      text = text.trim();
      var wire = registeredCategory(text);
      if (wire) return wire;
      // Legacy Tauri filesystem failures are strings. Inspect bounded text
      // locally, but emit only the constants above: never the text or its hash.
      return legacyCategory(text);
    } catch (_) {
      // Diagnostics must not break attachment handling for a malformed error.
      return "internal_error";
    }
  }
  function step(phase) {
    return Object.prototype.hasOwnProperty.call(steps, phase) ? steps[phase] : "attachment_ingest";
  }
  root.PinvouAttachmentTelemetry = Object.freeze({ errorCategory: errorCategory, step: step });
})(typeof window !== "undefined" ? window : globalThis);
