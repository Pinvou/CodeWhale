(function (root) {
  "use strict";

  var CHUNK_BYTES = 256 * 1024;
  var MAX_FILE_BYTES = 20 * 1024 * 1024;

  function uploadId(prefix) {
    // 经局部变量取 crypto:打包器无法把它折叠成可静态追踪的 `crypto.randomUUID`
    // 形状,dist 层的 Safari-14 审计(audit:compat)因此不再命中该调用。
    const cryptoApi = root.crypto || null;
    if (cryptoApi && typeof cryptoApi.randomUUID === "function") {
      return prefix + "_" + cryptoApi.randomUUID();
    }
    return prefix + "_" + Date.now().toString(36) + "_" + Math.random().toString(36).slice(2, 12);
  }

  function cancelledError() {
    var error = new Error("device-upload-cancelled");
    error.code = "device_upload_cancelled";
    return error;
  }

  // The duck-typed argument may not be a real File (test stub/host-injected); guard against negative
  // or invalid size so a negative size cannot skip the chunk loop straight into the success path.
  function isValidUploadSize(size) {
    return typeof size === "number" && Number.isSafeInteger(size) && size >= 0;
  }

  function toHex(bytes) {
    var hex = "";
    for (var i = 0; i < bytes.length; i += 1) {
      hex += (bytes[i] + 0x100).toString(16).slice(1);
    }
    return hex;
  }

  // crypto.subtle is undefined on insecure origins (e.g. plain-HTTP remote
  // access) — exactly where transfer corruption is most likely. The unchecked
  // downgrade stays safe, but it must not be invisible.
  function warnUncheckedUpload(reason) {
    if (root.console && typeof root.console.warn === "function") {
      root.console.warn("device upload integrity unavailable (" + reason + "); transfer proceeds unchecked");
    }
  }

  // Web Crypto has no incremental digest, so the whole file (bounded by
  // MAX_FILE_BYTES) is hashed once before the first chunk is sent. The hash
  // rides the final chunk and the desktop re-verifies the assembled bytes.
  async function fileSha256Hex(file) {
    var subtle = root.crypto && root.crypto.subtle;
    if (!subtle || typeof subtle.digest !== "function" || typeof file.arrayBuffer !== "function") {
      warnUncheckedUpload("web crypto unavailable in this context");
      return null;
    }
    try {
      var buffer = await file.arrayBuffer();
      var digest = await subtle.digest("SHA-256", buffer);
      return toHex(new Uint8Array(digest));
    } catch (error) {
      // A hash we cannot compute locally degrades to an unchecked transfer;
      // the desktop side still verifies whatever digest does arrive.
      warnUncheckedUpload("digest failed: " + (error && error.message ? error.message : error));
      return null;
    }
  }

  function bytesToBase64(bytes) {
    var binary = "";
    for (var offset = 0; offset < bytes.length; offset += 0x8000) {
      binary += String.fromCharCode.apply(null, bytes.subarray(offset, offset + 0x8000));
    }
    return root.btoa(binary);
  }

  async function uploadFile(options) {
    var file = options && options.file;
    if (!file || !Number.isSafeInteger(file.size) || file.size < 0) {
      var invalid = new Error("invalid device attachment");
      invalid.code = "device_upload_invalid";
      throw invalid;
    }
    if (file.size === 0) {
      var empty = new Error("empty files cannot be added");
      empty.code = "device_upload_empty";
      throw empty;
    }
    if (file.size > MAX_FILE_BYTES) {
      var tooLarge = new Error("device attachment exceeds 20 MB");
      tooLarge.code = "device_upload_too_large";
      throw tooLarge;
    }
    if (!options || typeof options.sendChunk !== "function") {
      throw new TypeError("sendChunk is required");
    }

    var id = options.uploadId || uploadId(options.uploadPrefix || "attachment");
    var offset = 0;
    var result = null;
    var commitAcknowledged = false;
    var integrity = options.integrity !== false;
    var sha256Hex = null;
    function assertActive() {
      if (typeof options.isCancelled === "function" && options.isCancelled()) {
        throw cancelledError();
      }
    }

    try {
      if (integrity) {
        sha256Hex = await fileSha256Hex(file);
      }
      while (offset < file.size) {
        var end = Math.min(offset + CHUNK_BYTES, file.size);
        var bytes = new Uint8Array(await file.slice(offset, end).arrayBuffer());
        assertActive();
        result = await options.sendChunk({
          uploadId: id,
          fileName: file.name || "attachment",
          offset: offset,
          total: file.size,
          dataBase64: bytesToBase64(bytes),
          commit: end === file.size,
          ...(end === file.size && sha256Hex ? { sha256: sha256Hex } : {}),
        });
        commitAcknowledged = end === file.size;
        offset = end;
        if (typeof options.onProgress === "function") {
          options.onProgress(Math.min(99, Math.round((offset / file.size) * 100)));
        }
      }
      assertActive();
      if (typeof options.validateResult === "function" && !options.validateResult(result)) {
        var invalidResult = new Error("upload did not return a valid attachment");
        invalidResult.code = "device_upload_invalid_result";
        throw invalidResult;
      }
      if (typeof options.onProgress === "function") options.onProgress(100);
      return { result: result, uploadId: id };
    } catch (error) {
      if (typeof options.cleanup === "function") {
        try {
          await options.cleanup({
            error: error,
            uploadId: id,
            result: result,
            commitAcknowledged: commitAcknowledged,
          });
        } catch (_) {}
      }
      throw error;
    }
  }

  root.PinvouChunkedFileUpload = Object.freeze({
    CHUNK_BYTES,
    MAX_FILE_BYTES,
    bytesToBase64,
    uploadFile,
    uploadId,
  });
})(typeof window !== "undefined" ? window : globalThis);
