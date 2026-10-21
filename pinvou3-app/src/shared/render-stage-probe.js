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
  // 同一会话持续卡顿时，上报本身（8-80ms/条的同步写）会形成正反馈挤爆共享诊断
  // 队列——一条 stall 记录已经足够定位"卡在哪段"，10 秒内不重复上报。
  var STALL_RECORD_MIN_INTERVAL_MS = 10000;

  var ring = [];
  var currentBySession = Object.create(null);
  var seq = 0;
  var lastStallRecordAt = 0;

  function now() {
    return typeof performance !== "undefined" && performance.now
      ? performance.now()
      : Date.now();
  }

  function finalize(entry) {
    if (entry.finalized) return;
    entry.finalized = true;
    // 武装了但还没触发的帧回调要取消：隐藏窗口里 rAF 永不触发也永不取消，
    // 1 小时能堆 11 万个回调、恢复可见的第一帧全部触发（评审 P2）。
    if (
      entry.rafHandle != null &&
      typeof window !== "undefined" &&
      typeof window.cancelAnimationFrame === "function"
    ) {
      window.cancelAnimationFrame(entry.rafHandle);
    }
    entry.rafHandle = null;
    if (currentBySession[entry.sid] === entry) delete currentBySession[entry.sid];
    var recvToDispatch =
      entry.t_dispatch != null && entry.t_recv != null ? entry.t_dispatch - entry.t_recv : null;
    var dispatchToFrame =
      entry.t_frame != null && entry.t_dispatch != null ? entry.t_frame - entry.t_dispatch : null;
    var stalled =
      (recvToDispatch != null && recvToDispatch > STALL_RECV_TO_DISPATCH_MS) ||
      (dispatchToFrame != null && dispatchToFrame > STALL_DISPATCH_TO_FRAME_MS);
    if (!stalled) return;
    // 隐藏窗口的定时器合并把正常的 32ms 帧间隔节流到 ~1s，纯时差判据会把每个
    // 周期都误判成 stall，~36 秒就把 256 条共享队列全部挤成假记录——恰恰在要
    // 诊断的现场毁掉诊断（评审 P2 实测 5/5 周期误报）。隐藏期间不上报。
    if (typeof document !== "undefined" && document.visibilityState === "hidden") return;
    var t = now();
    if (t - lastStallRecordAt < STALL_RECORD_MIN_INTERVAL_MS) return;
    lastStallRecordAt = t;
    var diagnostics = root.PinvouAuthoritySyncDiagnostics;
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
      entry.rafHandle = null;
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
      var entry = { sid: key, seq: ++seq, t_recv: now(), t_sched: null, t_dispatch: null, t_frame: null, rafHandle: null, finalized: false };
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
        entry.rafHandle = window.requestAnimationFrame(markFrame(entry));
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
