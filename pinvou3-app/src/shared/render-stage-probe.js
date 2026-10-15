/**
 * Render pipeline stage probe: event received → scheduled → dispatched → frame.
 *
 * 背景：客户现场的 UI「冻结」只有主线程栈和后端日志，分不清事件投递、JS
 * 调度、React 提交、最终呈现哪一段停了。这里在最热的流式路径上打四个时间
 * 戳，回答「卡在哪段」。设计约束（不可违反）：
 *   - **无内容**：只记 session id、序号和时间差，绝不接触消息文本；
 *   - **有界**：环形缓冲只留最近若干个周期，全量数据不出内存；
 *   - **异常触发上报**：仅当段间隔超过阈值时才推一条记录到
 *     authority-sync 诊断流（该流本身再有界落盘）。
 */
(function (root) {
  "use strict";

  var RING_SIZE = 64;
  var STALL_RECV_TO_DISPATCH_MS = 120;
  var STALL_DISPATCH_TO_FRAME_MS = 120;

  var ring = [];
  var currentBySession = Object.create(null);
  var seq = 0;

  function now() {
    return typeof performance !== "undefined" && performance.now
      ? performance.now()
      : Date.now();
  }

  function finalize(entry) {
    if (entry.finalized) return;
    entry.finalized = true;
    if (currentBySession[entry.sid] === entry) delete currentBySession[entry.sid];
    var recvToDispatch =
      entry.t_dispatch != null && entry.t_recv != null ? entry.t_dispatch - entry.t_recv : null;
    var dispatchToFrame =
      entry.t_frame != null && entry.t_dispatch != null ? entry.t_frame - entry.t_dispatch : null;
    var stalled =
      (recvToDispatch != null && recvToDispatch > STALL_RECV_TO_DISPATCH_MS) ||
      (dispatchToFrame != null && dispatchToFrame > STALL_DISPATCH_TO_FRAME_MS);
    if (!stalled) return;
    var diagnostics = root.AuthoritySyncDiagnostics;
    if (!diagnostics || typeof diagnostics.record !== "function") return;
    var details = { stage_seq: entry.seq };
    if (entry.sid) details.session_id = entry.sid;
    if (recvToDispatch != null) details.recv_to_dispatch_ms = Math.round(recvToDispatch);
    if (dispatchToFrame != null) details.dispatch_to_frame_ms = Math.round(dispatchToFrame);
    try {
      diagnostics.record("render_stage_stall", details);
    } catch (_) {
      /* 诊断失败绝不影响渲染路径 */
    }
  }

  function markFrame(entry) {
    return function () {
      entry.t_frame = now();
      finalize(entry);
    };
  }

  root.RenderStageProbe = {
    /** 事件已到达 JS 监听器。同 session 的上一个未完结周期先按已有时间戳收尾。 */
    begin: function (sid) {
      var key = sid || "";
      var previous = currentBySession[key];
      if (previous) finalize(previous);
      var entry = { sid: key, seq: ++seq, t_recv: now(), t_sched: null, t_dispatch: null, t_frame: null, finalized: false };
      currentBySession[key] = entry;
      ring.push(entry);
      if (ring.length > RING_SIZE) ring.shift();
      return entry;
    },
    /** 合并帧已排上（rAF/32ms 定时器已武装）。 */
    markScheduled: function (sid) {
      var entry = currentBySession[sid || ""];
      if (entry && entry.t_sched == null) entry.t_sched = now();
    },
    /** 可见发布已执行（React 状态已更新）。缺 scheduled 视为同步路径，补齐。 */
    markDispatched: function (sid) {
      var entry = currentBySession[sid || ""];
      if (!entry || entry.finalized) return;
      entry.t_dispatch = now();
      if (entry.t_sched == null) entry.t_sched = entry.t_dispatch;
      if (typeof window !== "undefined" && typeof window.requestAnimationFrame === "function") {
        window.requestAnimationFrame(markFrame(entry));
      } else {
        entry.t_frame = entry.t_dispatch;
        finalize(entry);
      }
    },
    /** 测试专用：取指定 session 当前未完结的活条目引用（模拟耗时用）。
     *  生产代码不得调用——snapshot() 返回的是拷贝，正是为了不暴露内部。 */
    _liveEntry: function (sid) {
      return currentBySession[sid || ""] || null;
    },
    /** 测试与诊断用：最近 N 个周期的段间隔快照（无内容）。 */
    snapshot: function () {
      return ring.map(function (entry) {
        return {
          sid: entry.sid,
          seq: entry.seq,
          recv_to_dispatch_ms:
            entry.t_dispatch != null && entry.t_recv != null ? Math.round(entry.t_dispatch - entry.t_recv) : null,
          dispatch_to_frame_ms:
            entry.t_frame != null && entry.t_dispatch != null ? Math.round(entry.t_frame - entry.t_dispatch) : null,
        };
      });
    },
  };
})(typeof window !== "undefined" ? window : globalThis);
