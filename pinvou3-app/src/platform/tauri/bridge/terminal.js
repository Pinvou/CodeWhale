/** Shell polling and terminal output normalization for bridge tool cards. */
(function (root) {
  "use strict";
  // biome-ignore lint/suspicious/noAssignInExpressions: registry bootstrap of the verbatim payload; splitting the statement would diverge from the artifact
  const registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry.terminal = function (context) {let pinvouSharedtauriTerminalCache = null;
function pinvouSharedtauriTerminal() {
  if (!pinvouSharedtauriTerminalCache) pinvouSharedtauriTerminalCache = window.PinvouBridgeShared.create("tauriTerminal", { SHELL_TOOL_NAMES, bt, normalizeTerminalTail, runSyncOnSession, notify, latestShellToolIsWaitObserver, state, addChatItem, shellPollState, invoke });
  return pinvouSharedtauriTerminalCache;
}


    const state = context.state;
    const notify = context.notify;
    const invoke = context.invoke;
    const bt = context.bt;
    const runSyncOnSession = context.runSyncOnSession;
    const addChatItem = context.addChatItem;
    let shellNotifyTimer = null;
    const shellPollState = Object.create(null);
  function updateToolItem(toolId, output, success) {
    for (var i = 0; i < state.chatItems.length; i++) {
      if (state.chatItems[i].type === "tool" && state.chatItems[i].toolId === toolId) {
        state.chatItems[i].output = output;
        state.chatItems[i].success = success;
        state.chatItems[i].state = success ? "done" : "failed";
        delete state.chatItems[i]._terminalParser;
        return state.chatItems[i];
      }
    }
    return null;
  }

  const SHELL_TOOL_NAMES = ["bash", "exec_shell", "exec_shell_wait", "exec_wait", "task_shell_start", "task_shell_wait", "shell", "Bash"];
  const SHELL_WAIT_TOOL_NAMES = ["exec_shell_wait", "exec_wait", "task_shell_wait"];

function isShellExecutionTool(name) { return pinvouSharedtauriTerminal().isShellExecutionTool(name); }

  function latestShellToolIsWaitObserver() {
    for (let i = state.chatItems.length - 1; i >= 0; i--) {
      const item = state.chatItems[i];
      if (item && item.type === "tool" && isShellExecutionTool(item.name)) {
        // Lowercase `bash` is canonical in v0.9.12. Uppercase `Bash` and the
        // dedicated wait names remain here for replayed legacy sessions.
        return SHELL_WAIT_TOOL_NAMES.includes(item.name) ||
          (item.name === "Bash" && item.args != null && item.args.action === "wait");
      }
    }
    return false;
  }

  function mentionsShellTool(text) {
    // 子智能体的工具调用不产生 chat:tool_start（forwarder 只把 Mailbox 的
    // ToolCallStarted 转成 multiagent:agent_progress），只能从进展文本里认出
    // shell 工具并借此调度快照轮询。status 形如 "🔧 bash (step 3)"；
    // 历史子智能体也可能仍报告 exec_shell。
    const raw = String(text || "");
    return SHELL_TOOL_NAMES.some((name) => raw.includes(name));
  }

function utf8Length(text) { return pinvouSharedtauriTerminal().utf8Length(text); }

  // Shell snapshots are a tail view, not an append-only byte stream. Normalize
  // terminal control sequences and state omissions explicitly instead of
  // pretending the visible tail is the complete log.
  function normalizeTerminalTail(text) {
    var value = String(text || "")
      .replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, "")
      .replace(/\x1b\[[0-?]*[ -\/]*[@-~]/g, "");
    var out = [];
    value.split("\n").forEach(function (line) {
      // After splitting on LF, a normal Windows CRLF line still ends in CR.
      // Remove that delimiter first; only an *internal* CR means a terminal
      // progress line overwrote earlier content on the same row.
      var visible = line.endsWith("\r") ? line.slice(0, -1) : line;
      var overwriteAt = visible.lastIndexOf("\r");
      if (overwriteAt >= 0) visible = visible.slice(overwriteAt + 1);
      while (visible.indexOf("\x08") >= 0) {
        visible = visible.replace(/[^\x08]\x08/g, "").replace(/^\x08+/, "");
      }
      out.push(visible);
    });
    return out.join("\n");
  }

function formatShellSnapshot(job) { return pinvouSharedtauriTerminal().formatShellSnapshot(job); }

function shellCommandForItem(item) { return pinvouSharedtauriTerminal().shellCommandForItem(item); }

function shellSnapshotKey(job) { return pinvouSharedtauriTerminal().shellSnapshotKey(job); }

function terminalShellHistoryMatch(item, job) { return pinvouSharedtauriTerminal().terminalShellHistoryMatch(item, job); }

function applyShellSnapshots(sid, jobs) { return pinvouSharedtauriTerminal().applyShellSnapshots(sid, jobs); }

function scheduleShellPoll(sid, immediate) { return pinvouSharedtauriTerminal().scheduleShellPoll(sid, immediate); }

async function runShellPoll(sid) { return pinvouSharedtauriTerminal().runShellPoll(sid); }

  function scheduleShellNotify() {
    if (shellNotifyTimer != null) return;
    shellNotifyTimer = window.setTimeout(function () {
      shellNotifyTimer = null;
      notify();
    }, 50);
  }

  function markBackgroundToolItem(toolId, sessionId, taskId, fallbackOutput) {
    for (var i = 0; i < state.chatItems.length; i++) {
      var item = state.chatItems[i];
      if (item.type !== "tool" || item.toolId !== toolId) continue;
      if (!item.liveOutput && fallbackOutput != null) item.output = fallbackOutput;
      item.success = null;
      item.state = "running";
      item.background = true;
      item.sessionId = sessionId || state.activeSessionId;
      item.taskId = taskId;
      return true;
    }
    return false;
  }

  function finishBackgroundToolItem(toolId, payload) {
    for (var i = 0; i < state.chatItems.length; i++) {
      var item = state.chatItems[i];
      if (item.type !== "tool" || item.toolId !== toolId) continue;
      // chat:shell_task_status can repeat for an already finalized task;
      // re-merging would append the tails a second time, so leave items in a
      // terminal state untouched.
      if (item.state === "done" || item.state === "failed") return true;
      var status = payload.status || "Failed";
      var success = status === "Completed";
      item.success = success;
      item.state = success ? "done" : "failed";
      item.background = false;
      item.shellStatus = status;
      item.exitCode = payload.exit_code;
      item.output = reconcileBackgroundTerminalOutput(item.output, payload);
      delete item._terminalParser;
      return true;
    }
    return false;
  }

  var MAX_PENDING_TERMINAL_SEQUENCE_CHARS = 16 * 1024;
  function rememberPendingTerminalSequence(parserState, input, start) {
    var pending = input.slice(start);
    // A malformed unterminated OSC/DCS sequence must not bypass the live
    // output tail limit and grow renderer memory without bound.
    parserState.pendingAnsi = pending.length <= MAX_PENDING_TERMINAL_SEQUENCE_CHARS ? pending : "";
  }

  function stripTerminalSequences(text, parserState) {
    var input = String((parserState.pendingAnsi || "") + (text || ""));
    parserState.pendingAnsi = "";
    var clean = "";
    for (var i = 0; i < input.length; i++) {
      if (input[i] !== "\x1b") {
        clean += input[i];
        continue;
      }
      if (i + 1 >= input.length) {
        rememberPendingTerminalSequence(parserState, input, i);
        break;
      }

      var kind = input[i + 1];
      if (kind === "[") {
        var csiEnd = i + 2;
        var malformedCsi = false;
        while (csiEnd < input.length) {
          var csiCode = input.charCodeAt(csiEnd);
          if (csiCode >= 0x40 && csiCode <= 0x7e) break;
          if (csiCode < 0x20 || csiCode > 0x3f) {
            malformedCsi = true;
            break;
          }
          csiEnd += 1;
        }
        if (malformedCsi) {
          i += 1;
          continue;
        }
        if (csiEnd >= input.length) {
          rememberPendingTerminalSequence(parserState, input, i);
          break;
        }
        i = csiEnd;
        continue;
      }

      // OSC/DCS/SOS/PM/APC are terminated by ST (ESC \); OSC also accepts BEL.
      if (kind === "]" || kind === "P" || kind === "X" || kind === "^" || kind === "_") {
        var stringEnd = i + 2;
        var terminated = false;
        while (stringEnd < input.length) {
          if (kind === "]" && input[stringEnd] === "\x07") {
            terminated = true;
            break;
          }
          if (input[stringEnd] === "\x1b" && input[stringEnd + 1] === "\\") {
            stringEnd += 1;
            terminated = true;
            break;
          }
          stringEnd += 1;
        }
        if (!terminated) {
          rememberPendingTerminalSequence(parserState, input, i);
          break;
        }
        i = stringEnd;
        continue;
      }

      // Generic two-or-more-byte escape sequence: optional intermediate
      // bytes followed by a final byte.
      var escapeEnd = i + 1;
      while (escapeEnd < input.length) {
        var escapeCode = input.charCodeAt(escapeEnd);
        if (escapeCode < 0x20 || escapeCode > 0x2f) break;
        escapeEnd += 1;
      }
      if (escapeEnd >= input.length) {
        rememberPendingTerminalSequence(parserState, input, i);
        break;
      }
      var finalCode = input.charCodeAt(escapeEnd);
      if (finalCode >= 0x30 && finalCode <= 0x7e) i = escapeEnd;
    }
    return clean;
  }

  function terminalParserState(item, stream) {
    if (!item._terminalParser) {
      Object.defineProperty(item, "_terminalParser", {
        value: {},
        writable: true,
        configurable: true,
      });
    }
    var key = stream === "stderr" ? "stderr" : "stdout";
    if (!item._terminalParser[key]) {
      item._terminalParser[key] = { pendingCR: false, pendingAnsi: "" };
    }
    return item._terminalParser[key];
  }

  // A standalone carriage return resets the current terminal line. WinGet
  // uses this for progress frames, so keep the newest frame instead of
  // appending hundreds of nearly identical lines.
  function mergeTerminalChunk(previous, chunk, parserState, prefix) {
    var output = String(previous == null ? "" : previous);
    var clean = stripTerminalSequences(chunk, parserState);
    var i = 0;
    if (parserState.pendingCR && clean) {
      if (clean[0] === "\n") {
        output += "\n";
        i = 1;
      } else {
        output = output.slice(0, output.lastIndexOf("\n") + 1);
      }
      parserState.pendingCR = false;
    }
    var needsPrefix = !!prefix;
    for (; i < clean.length; i++) {
      var ch = clean[i];
      if (ch === "\r") {
        if (clean[i + 1] === "\n") {
          output += "\n";
          i += 1;
        } else if (i + 1 >= clean.length) {
          parserState.pendingCR = true;
        } else {
          output = output.slice(0, output.lastIndexOf("\n") + 1);
        }
      } else if (ch === "\b") {
        var lineStart = output.lastIndexOf("\n") + 1;
        if (output.length > lineStart) output = output.slice(0, -1);
      } else {
        if (needsPrefix) {
          output += prefix;
          needsPrefix = false;
        }
        output += ch;
      }
    }
    return output;
  }

  function mergeTerminalTail(previous, tail) {
    var output = String(previous == null ? "" : previous);
    var suffix = String(tail == null ? "" : tail);
    if (!suffix) return output;
    if (!output) return suffix;
    if (output.indexOf(suffix) >= 0) return output;

    var maxOverlap = Math.min(output.length, suffix.length);
    for (var overlap = maxOverlap; overlap > 0; overlap--) {
      if (output.slice(-overlap) === suffix.slice(0, overlap)) {
        return output + suffix.slice(overlap);
      }
    }
    return output + (output.endsWith("\n") || suffix.startsWith("\n") ? "" : "\n") + suffix;
  }

  function normalizeTerminalTail(tail, prefix) {
    if (!tail) return "";
    return mergeTerminalChunk(
      "",
      tail,
      { pendingCR: false, pendingAnsi: "" },
      prefix || ""
    );
  }

  // Live and background shell output are display-only; completion replaces
  // the tail with the normal full result. Both paths must share one cap so a
  // verbose process cannot grow renderer memory without bound.
  const MAX_LIVE_OUTPUT_CHARS = 128 * 1024;

  function reconcileBackgroundTerminalOutput(previous, payload) {
    var output = String(previous == null ? "" : previous);
    output = mergeTerminalTail(output, normalizeTerminalTail(payload.stdout_tail, ""));
    output = mergeTerminalTail(output, normalizeTerminalTail(payload.stderr_tail, "[STDERR] "));
    if (output.length > MAX_LIVE_OUTPUT_CHARS) output = "…\n" + output.slice(-MAX_LIVE_OUTPUT_CHARS);
    return output;
  }

  // Live shell output is display-only. The completed tool result remains the
  // authoritative value written to conversation history/model context.
  function appendToolItemOutput(toolId, content, stream) {
    var chunk = typeof content === "string" ? content : String(content == null ? "" : content);
    if (!chunk) return false;
    for (var i = 0; i < state.chatItems.length; i++) {
      var item = state.chatItems[i];
      if (item.type !== "tool" || item.toolId !== toolId) continue;
      var parserState = terminalParserState(item, stream);
      var output = mergeTerminalChunk(
        item.output,
        chunk,
        parserState,
        stream === "stderr" ? "[STDERR] " : ""
      );
      // A verbose long-running process must not grow renderer memory without
      // bound. Completion replaces this tail with the normal full result.
      if (output.length > MAX_LIVE_OUTPUT_CHARS) output = "…\n" + output.slice(-MAX_LIVE_OUTPUT_CHARS);
      item.output = output;
      item.liveOutput = true;
      return true;
    }
    return false;
  }


    return {
      updateToolItem,
      isShellExecutionTool,
      mentionsShellTool,
      utf8Length,
      formatShellSnapshot,
      shellCommandForItem,
      shellSnapshotKey,
      terminalShellHistoryMatch,
      applyShellSnapshots,
      scheduleShellPoll,
      runShellPoll,
      scheduleShellNotify,
      markBackgroundToolItem,
      finishBackgroundToolItem,
      rememberPendingTerminalSequence,
      stripTerminalSequences,
      terminalParserState,
      mergeTerminalChunk,
      mergeTerminalTail,
      normalizeTerminalTail,
      reconcileBackgroundTerminalOutput,
      appendToolItemOutput
    };
  };
})(window);
