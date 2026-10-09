(function () {
  "use strict";

  var registry = window.__PINVOU_TAURI_BRIDGE_FEATURES__ = window.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["artifact-tracker"] = function (context) {
    var state = context.state;
    var invoke = context.invoke;
    var notify = context.notify;
    var sessionStates = context.sessionStates;
    var isScheduledRunSession = context.isScheduledRunSession;

  // ── 产物跟踪 ─────────────────────────────────────────────────────
  function basename(p) {
    if (!p) return "";
    var parts = String(p).split(/[\\/]/);
    return parts[parts.length - 1] || p;
  }
  function isAbsPath(p) {
    // UNC network-share paths (\\server\\share\...) are absolute too —
    // validate_codex_project_workspace stores them after platform_compat_path,
    // and without this arm the rebase transform returned them unmapped
    // (review #463 round-20 minor 6; Windows network-share workspaces only).
    return (
      typeof p === "string"
        && (p.charAt(0) === "/" || /^[A-Za-z]:[\\/]/.test(p) || /^\\\\[^\\]/.test(p))
    );
  }
  function normalizedPath(p) {
    return String(p || "").replace(/\\/g, "/");
  }
  // Freshness window for the workspace_rebound mark's VIEW heal (the
  // basename-based reconcile arm): generous enough to cover the rebind
  // dialog's retry flow, short enough that an old mark cannot misfire the
  // arm on an unrelated later basename collision. Deliberately does NOT
  // delete the mark on expiry (review #463 round-C Major 2): the save-path
  // transform (rebaseArtifactPathsForRebind) is prefix-exact and shares the
  // mark, and its "whole process lifetime" contract requires the mark to
  // survive while the session's buffer may still hold stale paths — pruning
  // here re-armed the durable revert the transform exists to prevent.
  var REBIND_RECONCILE_WINDOW_MS = 10 * 60 * 1000;
  function sessionRecentlyRebound(sid) {
    var marks = state.reboundSessionIds;
    if (!marks || !sid) return false;
    var mark = marks[sid];
    if (!mark || !mark.at) return false;
    return Date.now() - mark.at <= REBIND_RECONCILE_WINDOW_MS;
  }
  // Strips trailing separators without a regex (the ESLint deny gate flags
  // the previous /\/+$/ form as super-linear).
  function trimTrailingSlashes(p) {
    var s = normalizedPath(p);
    while (s.endsWith("/")) s = s.slice(0, -1);
    return s;
  }
  // Locates the folded prefix match in the ORIGINAL string and returns the
  // code-unit length to cut there, or -1 when no prefix of `original` folds
  // to `foldedKey`. The fold is for the comparison only — the cut must NOT
  // use the folded prefix's length: expanding case mappings (U+0130 "İ"
  // lowercases to "i" + U+0307, two code units for one) make the folded form
  // longer than the original prefix, and slicing the original at the folded
  // length eats a character of the saved suffix (review #463 round-13:
  // "/data/İstanbul/a.txt" used to save as "<to>a.txt"). Lowercasing never
  // shrinks a string, so the scan can stop once the folded prefix outgrows
  // the key.
  function foldedPrefixCutLength(original, foldedKey) {
    for (var cut = 0; cut <= original.length; cut++) {
      var folded = original.slice(0, cut).toLowerCase();
      if (folded === foldedKey) return cut;
      if (folded.length >= foldedKey.length) return -1;
    }
    return -1;
  }
  // Rebases absolute artifact paths along the session's rebind SEGMENT CHAIN
  // while a workspace_rebound mark exists (review #463 round-B Major 1 + the
  // round-D vintage fix): a post-rebind wholesale save of a chat turn's
  // buffer would otherwise durably revert the backend lane's rebase of
  // SavedSession.artifacts[].storage_path. Segments apply in order — an
  // A-era path resolves A→B→C, a buffer re-vintaged from the durable JSON
  // between chained rebinds (B-era) resolves B→C, a C-era path matches
  // nothing. Prefix-exact on the folded normalized form; the suffix keeps
  // the original casing. Relative paths already resolve against the CURRENT
  // workspace and are untouched; marks are memory-only, so a restart starts
  // from the already-rebased JSON with no marks and this is a no-op.
  function rebaseArtifactPathsForRebind(sid, paths) {
    var marks = state.reboundSessionIds;
    var mark = marks && sid ? marks[sid] : null;
    if (!mark || !Array.isArray(mark.chain) || !mark.chain.length || !Array.isArray(paths)) {
      return paths;
    }
    var segments = mark.chain
      .map(function (segment) {
        return {
          // The case fold is unconditional (review #463 round-13): the
          // bridges have no filesystem-case-sensitivity capability to gate
          // it on — adding one would span the Rust capability surface, both
          // lanes' platform wiring and this factory's deps, which is not a
          // small change. On a case-sensitive filesystem two roots differing
          // only in case are indistinguishable HERE while the POSIX backend
          // identity key stays case-sensitive — the fold is this transform's
          // own trade-off (the backend folds unconditionally only on
          // Windows); reworded per round-24 minor 15, capability-gating
          // stays the recorded later option.
          fromKey: trimTrailingSlashes(segment.from).toLowerCase(),
          toKey: trimTrailingSlashes(segment.to),
        };
      })
      .filter(function (segment) { return segment.fromKey; });
    if (!segments.length) return paths;
    return paths.map(function (p) {
      if (typeof p !== "string" || !isAbsPath(p)) return p;
      var norm = normalizedPath(p);
      var mapped = false;
      for (var i = 0; i < segments.length; i++) {
        var cut = foldedPrefixCutLength(norm, segments[i].fromKey);
        if (cut >= 0 && (cut === norm.length || norm.charAt(cut) === "/")) {
          norm = segments[i].toKey + norm.slice(cut);
          mapped = true;
        }
      }
      return mapped ? norm : p;
    });
  }
  // Whether the path still points at a PRE-rebind location on this
  // session's rebind chain (review #463 round-24 minor 3): the reconcile's
  // stale-absolute heal arm is gated on it, so an unrelated live absolute
  // entry whose basename collides cannot be taken over and the wrong path
  // persisted.
  function pathIsRebindStale(sid, path) {
    var marks = state.reboundSessionIds;
    var mark = marks && sid ? marks[sid] : null;
    if (!mark || !Array.isArray(mark.chain) || !mark.chain.length) return false;
    var mapped = rebaseArtifactPathsForRebind(sid, [path]);
    return Array.isArray(mapped) && normalizedPath(mapped[0]) !== normalizedPath(path);
  }
  function noteArtifactChange(path, event, sessionId) {
    if (!path) return;
    state.artifactChange = {
      seq: (state.artifactChange && state.artifactChange.seq || 0) + 1,
      path: path,
      event: event || "modified",
      sessionId: sessionId || "",
      at: Date.now(),
    };
    notify();
  }
  function isSharedMcpArtifactPath(path) {
    return normalizedPath(path).indexOf("/sessions/default/artifacts/") >= 0;
  }
  function artifactBelongsToSession(path, sid) {
    if (!path || !sid) return false;
    if (!isAbsPath(path)) return true;
    if (isSharedMcpArtifactPath(path)) return true;
    var normalized = normalizedPath(path);
    if (normalized.indexOf("/sessions/") >= 0) {
      return normalized.indexOf("/sessions/" + sid + "/workspace/") >= 0 ||
        normalized.indexOf("/sessions/" + sid + "/artifacts/") >= 0;
    }
    return true;
  }
  function filterSessionArtifacts(artifacts, sid) {
    return (Array.isArray(artifacts) ? artifacts : []).filter(function (a) {
      return artifactBelongsToSession(a && a.path, sid);
    });
  }
  // 「成品型」扩展名:write_file 写出这类文件即自动当成品进面板(模型常忘 present_artifact)。
  // 办公文档 + markdown 报告 + 数据表 + 图片 + 打包件都算成品(覆盖 AI 常见产出格式)。
  // 中间/草稿(.txt/.json/.xml 等)刻意不在此列 → 不进面板,避免一堆过程文件污染产物列表;
  // 这类格式若确是成品,靠模型 present_artifact 显式挂出(present 过的不受扩展名门控)。
  var DELIVERABLE_EXTS = new Set([
    "pptx", "ppt", "docx", "doc", "pdf", "html", "htm", "xlsx", "xls",
    "md", "csv", "png", "jpg", "jpeg", "svg", "gif", "webp", "zip",
  ]);
  // tmp/ 是提示词约定的中间文件目录(instructions.md:中间/临时文件一律写 tmp/,
  // 不进产出物列表)。tmp/ 下的文件即使扩展名是成品型(.md/.html 等)也不算自动成品;
  // 确需展示只能靠模型显式 present_artifact(显式 present 不经 isDeliverable 门控)。
  function isTmpPath(path) {
    var segs = normalizedPath(path).split("/");
    for (var i = 0; i < segs.length; i++) {
      if (segs[i] === "tmp") return true;
    }
    return false;
  }
  function isDeliverable(path) {
    if (isTmpPath(path)) return false;
    var ext = (String(path || "").split(".").pop() || "").toLowerCase();
    return DELIVERABLE_EXTS.has(ext);
  }
  function artifactType(path) {
    var ext = (String(path || "").split(".").pop() || "").toLowerCase();
    if (["doc", "docx", "md", "rtf"].indexOf(ext) >= 0) return "document";
    if (["xls", "xlsx", "csv"].indexOf(ext) >= 0) return "spreadsheet";
    if (["ppt", "pptx", "odp"].indexOf(ext) >= 0) return "slides";
    if (ext === "pdf") return "pdf";
    if (["png", "jpg", "jpeg", "gif", "webp", "svg"].indexOf(ext) >= 0) return "image";
    if (["html", "htm"].indexOf(ext) >= 0) return "html";
    if (["js", "jsx", "ts", "tsx", "py", "rs", "go", "java", "c", "cpp", "h", "css"].indexOf(ext) >= 0) return "code";
    return "other";
  }
  function artifactTelemetryId() {
    if (window.crypto && typeof window.crypto.randomUUID === "function") {
      return "artifact_" + window.crypto.randomUUID().replace(/-/g, ""); // safari14-ok: guarded above
    }
    if (window.crypto && typeof window.crypto.getRandomValues === "function") {
      var bytes = new Uint8Array(12);
      window.crypto.getRandomValues(bytes);
      return "artifact_" + Array.from(bytes, function (byte) { return byte.toString(16).padStart(2, "0"); }).join("");
    }
    return "artifact_" + Date.now().toString(36) + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2);
  }
  function recordArtifactCreated(artifact, sid) {
    if (!artifact || !sid) return;
    invoke("track_behavior_event", { request: {
      eventName: "artifact_created", artifactId: artifact.telemetryId,
      artifactType: artifact.telemetryType, sessionId: sid,
    } }).catch(function () {});
  }
  function trackArtifact(path) {
    if (!path) return;
    var bn = basename(path);
    for (var i = 0; i < state.artifacts.length; i++) {
      if (basename(state.artifacts[i].path) === bn) {
        // 已有同名:write_file 跟踪的是相对路径、disk watcher 推的是绝对路径——同一文件
        // 两种 path 会重复。新 path 绝对而旧的相对则用绝对替换(open 可靠),否则忽略重复。
        if (isAbsPath(path) && !isAbsPath(state.artifacts[i].path)) {
          state.artifacts[i] = Object.assign({}, state.artifacts[i], { path: path, basename: bn });
          notify();
        }
        return;
      }
    }
    var artifact = { path: path, basename: bn, telemetryId: artifactTelemetryId(), telemetryType: artifactType(path) };
    state.artifacts.push(artifact);
    recordArtifactCreated(artifact, state.activeSessionId);
    notify();
  }
  function markTurnDirtyArtifact(path) {
    var bn = basename(path);
    if (!bn) return;
    if ((state.turnDirtyArtifacts || []).some(function (p) { return basename(p) === bn; })) return;
    state.turnDirtyArtifacts.push(path);
  }
  function untrackArtifact(path) {
    var before = state.artifacts.length;
    state.artifacts = state.artifacts.filter(function (a) { return a.path !== path; });
    if (state.artifacts.length !== before) notify();
  }
  // Prefer an exact normalized path so an older card is not hidden by a newer
  // same-named artifact from another directory. Fall back to the basename for
  // persisted relative paths that must reconcile with an absolute watcher path.
  function findPresentedArtifact(path) {
    var bn = basename(path);
    if (!bn) return null;
    var normalized = normalizedPath(path);
    var basenameMatch = null;
    for (var i = state.chatItems.length - 1; i >= 0; i--) {
      var it = state.chatItems[i];
      if (it.type !== "artifact_card" || basename(it.path) !== bn) continue;
      if (normalizedPath(it.path) === normalized) return it;
      if (!basenameMatch) basenameMatch = it;
    }
    return basenameMatch;
  }
  // Refresh an existing presentation in place so the card keeps its identity
  // and transcript position. Exact paths win; basename fallback exists only
  // for persisted relative paths that later resolve to an absolute watcher path.
  function updatePresentedArtifact(card) {
    if (!card || !card.path) return null;
    var existing = findPresentedArtifact(card.path);
    if (!existing) return null;
    if (isAbsPath(existing.path) && isAbsPath(card.path) &&
        normalizedPath(existing.path) !== normalizedPath(card.path)) return null;
    var bn = basename(card.path);
    for (var i = state.chatItems.length - 1; i >= 0; i--) {
      var it = state.chatItems[i];
      if (it === existing) break;
      if (it.type === "user") return null;
      if (it.type === "tool" && fileMutationAction(it.name, it.args) &&
          extractArtifactPaths(it.args).some(function (ap) { return basename(ap) === bn; })) break;
    }
    var stableId = existing.id;
    var stableAbsolutePath = isAbsPath(existing.path) && !isAbsPath(card.path)
      ? existing.path
      : null;
    Object.assign(existing, card, { type: "artifact_card" });
    if (stableId !== undefined) existing.id = stableId;
    if (stableAbsolutePath) existing.path = stableAbsolutePath;
    return existing;
  }
  // 切换 session 时对账:扫 workspace 磁盘,把实际存在、但跟踪列表里没有的文件补进来。
  // 修「文件已生成在盘上、却因 app 中途重启/跟踪遗漏而不在产物面板」(以磁盘为准)。
  async function reconcileArtifacts(sid) {
    if (!sid) return;
    if (isScheduledRunSession(sid)) return;
    try {
      var files = await invoke("list_workspace_files", { sessionId: sid });
      if (sid !== state.activeSessionId) return; // 已切走,放弃(避免写错 session)
      var byName = {};
      state.artifacts.forEach(function (a) { byName[basename(a.path)] = a; });
      var added = false;
      files.forEach(function (p) {
        var bn = basename(p);
        var ex = byName[bn];
        // 已 present_artifact 过的成品在 saved.artifacts(ex 命中);扫盘只「新增」成品型文件,
        // 不再把所有过程文件全扫进面板(修「飞书 CLI scratch 全暴露成产物」)。
        if (!ex) {
          if (!isDeliverable(p)) return;
          var na = { path: p, basename: bn }; state.artifacts.push(na); byName[bn] = na; added = true;
        }
        else if (isAbsPath(p) && (!isAbsPath(ex.path) || (normalizedPath(ex.path) !== normalizedPath(p) && sessionRecentlyRebound(sid) && pathIsRebindStale(sid, ex.path)))) {
          // Relative→absolute opens reliably; or stale absolute → live
          // workspace file, matched by basename — ONLY for a session the
          // rebind command just moved (the workspace_rebound mark, review
          // #463 round-10 Major 2) AND only when the stored entry is real
          // stale rebind geometry (its path sits under a chain from-prefix;
          // round-24 minor 3: the mark alone let an unrelated live absolute
          // entry whose basename collides be taken over and the wrong path
          // persisted). After a folder rebind the persisted entry keeps the
          // vanished root, and the relative→absolute escape hatch never
          // fires for an already-absolute stale entry, so the
          // freshness-windowed mark is what lets the reconcile heal it.
          ex.path = p; added = true;
        }
      });
      if (added) {
        notify();
        try { await invoke("save_session_artifacts", { id: sid, paths: rebaseArtifactPathsForRebind(sid, state.artifacts.map(function (a) { return a.path; })) }); } catch (_) {}
      }
    } catch (e) { /* workspace 不存在(新 session)等,忽略 */ }
  }
  function pushArtifactPath(paths, path) {
    if (typeof path !== "string" || !path.trim()) return;
    path = path.trim();
    if (paths.indexOf(path) < 0) paths.push(path);
  }
  function extractPatchHeaderPaths(patch, paths) {
    String(patch || "").split(/\r?\n/).forEach(function (line) {
      var custom = /^\*\*\* (?:Add|Update|Delete) File:\s*(.+?)\s*$/.exec(line);
      if (custom) { pushArtifactPath(paths, custom[1]); return; }
      var unified = /^\+\+\+\s+(?:b\/)?(.+?)\s*$/.exec(line);
      if (unified && unified[1] !== "/dev/null") pushArtifactPath(paths, unified[1]);
    });
  }
  // File.write / File.edit / File.patch 的 args 里提取全部产物路径。
  function extractArtifactPaths(args) {
    if (!args) return [];
    if (typeof args === "string") {
      try { args = JSON.parse(args); } catch (e) { return []; }
    }
    var paths = [];
    pushArtifactPath(paths, args.path || args.file_path || args.filename);
    [args.replace, args.changes].forEach(function (changes) {
      if (!Array.isArray(changes)) return;
      changes.forEach(function (change) {
        if (!change || typeof change !== "object") return;
        pushArtifactPath(paths, change.path || change.file_path || change.filename);
      });
    });
    extractPatchHeaderPaths(args.patch, paths);
    return paths;
  }
  function extractArtifactPath(args) {
    return extractArtifactPaths(args)[0] || null;
  }

  function fileMutationAction(name, args) {
    if (typeof args === "string") {
      try { args = JSON.parse(args); } catch (_) { args = null; }
    }
    if (String(name || "").toLowerCase() === "file") {
      var action = String(args && args.action || "").toLowerCase();
      return action === "write" || action === "edit" || action === "patch" ? action : null;
    }
    if (name === "write" || name === "write_file") return "write";
    if (name === "edit" || name === "edit_file") return "edit";
    return null;
  }


  function isPresentArtifactTool(name) {
    return name === "present_artifact" ||
      (typeof name === "string" && name.endsWith("present_artifact"));
  }

  // 成品卡路径:优先用 server(present_artifact_server.py)解析并验证过的绝对路径 abs_path——
  // 模型常给相对路径,直接拿 args.path 渲染会让卡片 path 是相对,点 Open 报「path must be
  // absolute」,且模型可能重试再 present 一次出双卡。取不到 abs_path 才回退原始 path。
  // 兼容两种结果格式:直接 payload {abs_path} / MCP content 数组 {content:[{text}]} 包一层。
  function parseToolResultPayload(toolResultContent) {
    try {
      var raw = typeof toolResultContent === "string" ? toolResultContent : JSON.stringify(toolResultContent || {});
      var obj = JSON.parse(raw);
      if (obj && obj.content && obj.content[0] && typeof obj.content[0].text === "string") {
        try {
          var inner = JSON.parse(obj.content[0].text);
          if (inner && typeof inner === "object") return inner;
        } catch (_) {}
      }
      return obj;
    } catch (_) {
      return null;
    }
  }
  function artifactPathFromToolOutput(toolResultContent) {
    var obj = parseToolResultPayload(toolResultContent);
    if (!obj || typeof obj !== "object") return null;
    var p = obj.abs_path || obj.path || obj.file_path || obj.local_path;
    return typeof p === "string" && p ? p : null;
  }
  function shouldUseToolOutputAsArtifact(name) {
    if (!name || isPresentArtifactTool(name)) return false;
    // Only MCP-style producer tools should be parsed from result JSON. Shell/read
    // tools often return diagnostic JSON with a `path` field, which is not a
    // newly created artifact.
    return typeof name === "string" && name.indexOf("mcp_") === 0;
  }
  function presentArtifactAbsPath(toolResultContent, fallbackPath) {
    fallbackPath = fallbackPath || "";
    var parsed = artifactPathFromToolOutput(toolResultContent);
    if (parsed) return parsed;
    return fallbackPath;
  }


    return {
      basename: basename,
      isAbsPath: isAbsPath,
      normalizedPath: normalizedPath,
      sessionRecentlyRebound: sessionRecentlyRebound,
      rebaseArtifactPathsForRebind: rebaseArtifactPathsForRebind,
      noteArtifactChange: noteArtifactChange,
      isSharedMcpArtifactPath: isSharedMcpArtifactPath,
      artifactBelongsToSession: artifactBelongsToSession,
      filterSessionArtifacts: filterSessionArtifacts,
      isDeliverable: isDeliverable,
      trackArtifact: trackArtifact,
      markTurnDirtyArtifact: markTurnDirtyArtifact,
      untrackArtifact: untrackArtifact,
      findPresentedArtifact: findPresentedArtifact,
      updatePresentedArtifact: updatePresentedArtifact,
      reconcileArtifacts: reconcileArtifacts,
      extractArtifactPaths: extractArtifactPaths,
      extractArtifactPath: extractArtifactPath,
      fileMutationAction: fileMutationAction,
      isPresentArtifactTool: isPresentArtifactTool,
      parseToolResultPayload: parseToolResultPayload,
      artifactPathFromToolOutput: artifactPathFromToolOutput,
      shouldUseToolOutputAsArtifact: shouldUseToolOutputAsArtifact,
      presentArtifactAbsPath: presentArtifactAbsPath,
    };
  };
})();
