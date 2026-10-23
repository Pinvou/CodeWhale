/**
 * Render pipeline stage probe: event received → scheduled → dispatched → frame.
 *
 * 背景：客户现场的 UI「冻结」只有主线程栈和后端日志，分不清事件投递、JS
 * 调度、React 提交、最终呈现哪一段停了。这里在最热的流式路径上打四个时间
 * 戳。设计约束（不可违反）：
 *   - **无内容**：只记 session id、序号和时间差，绝不接触消息文本；
 *   - **有界**：环形缓冲只留最近若干个周期，全量数据不出内存；
 *   - **异常触发上报**：仅当段间隔超过阈值时才推一条记录到
 *     authority-sync 诊断流（该流本身再有界落盘）。
 *
 * 实现边界（r2 评审 P2-3/P2-4，正文与这里保持一致，勿超出边界表述）：
 *   - 独立上报的段差只有两段：recv→dispatch、dispatch→frame。t_sched 仅在
 *     同步直发路径补齐（不差量、不独立上报）；React 提交与最终呈现合并在
 *     dispatch→frame 一段里，测不出二者之间的停顿。
 *   - 后台会话（合并帧 publish 时非活跃 sid）的 publish 不打派发标记，
 *     其 recv→dispatch 段当前不量（纯失测、不产假报）；会话激活的真实发布
 *     路径同样未打标记。后台流式卡顿定位靠切回前台后的周期。
 *   - 长任务阻塞期间「未派发即被取代」的周期只有恰处已派发帧未回相位才
 *     补报一条（begin 的 supersede 补报），其余静默收尾。
 */
(function (root) {
  "use strict";

  // desktop-only（接线边界）：钩子仅装于 tauri lane（bridge/chat-events 的
  // onSessionEvent/publish 路径）；web 宿主的 platform/web/bridge.js 有自己的
  // chat:delta 监听但未接探针——web 流式冻结当前不可诊断是未接线，非不能接线。
  var RING_SIZE = 64;
  var STALL_RECV_TO_DISPATCH_MS = 120;
  var STALL_DISPATCH_TO_FRAME_MS = 120;
  // 同一会话持续卡顿时，上报本身（8-80ms/条的同步写）会形成正反馈挤爆共享诊断
  // 队列——一条 stall 记录已经足够定位"卡在哪段"，10 秒内不重复上报。
  // **按会话**节流（评审 P2-3）：全局节流会让一个会话的持续卡顿把另一个会话的
  // 真实 stall 压制 10 秒，制造跨会话诊断盲区。
  var STALL_RECORD_MIN_INTERVAL_MS = 10000;

  var ring = [];
  var currentBySession = Object.create(null);
  var seq = 0;
  var lastStallRecordAt = Object.create(null);

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
    // 先确认诊断模块可用、再消耗节流窗口（评审 P3：诊断缺失/record 抛错时
    // 不该把该会话的 10s 节流窗口白白烧掉）。
    var diagnostics = root.PinvouAuthoritySyncDiagnostics;
    if (!diagnostics || typeof diagnostics.record !== "function") return;
    var t = now();
    var last = lastStallRecordAt[entry.sid] || 0;
    if (t - last < STALL_RECORD_MIN_INTERVAL_MS) return;
    var details = { stage_seq: entry.seq };
    if (entry.sid) details.session_id = entry.sid;
    if (recvToDispatch != null) details.recv_to_dispatch_ms = Math.round(recvToDispatch);
    if (dispatchToFrame != null) details.dispatch_to_frame_ms = Math.round(dispatchToFrame);
    try {
      diagnostics.record("render_stage_stall", details);
      // 成功才烧节流窗口（r2 P3：record 抛错时不消耗该会话的 10s 配额）
      lastStallRecordAt[entry.sid] = t;
    } catch (_) {
      /* 诊断失败绝不影响渲染路径 */
    }
  }

  function markFrame(entry) {
    return function () {
      // 无 cancelAnimationFrame 的老 webview 里，迟到于收尾的补触发回调仍会
      // 进来（句柄没被真正取消）——此时条目已完结，再写 t_frame 只会污染
      // snapshot()（上报不受影响，finalize 幂等），守一道。
      if (entry.finalized) return;
      entry.rafHandle = null;
      entry.t_frame = now();
      finalize(entry);
    };
  }

  // P2-1（隐藏→恢复可见的假 stall）：rAF 在隐藏窗口里被浏览器挂起，恢复可见
  // 后才触发——此刻 finalize 量到的 dispatch_to_frame 横跨整个隐藏时段（实测
  // 5 分钟假 stall）。「边生成边切走窗口」是常见用法。窗口隐藏时主动收尾所有
  // 武装条目（此刻量到的时间差全部落在可见期内，量程诚实；rAF 句柄随 finalize
  // 取消，不会在恢复后补触发）。
  if (typeof document !== "undefined" && typeof document.addEventListener === "function") {
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState !== "hidden") return;
      var entries = Object.keys(currentBySession);
      for (var i = 0; i < entries.length; i++) {
        finalize(currentBySession[entries[i]]);
      }
    });
  }

  root.RenderStageProbe = {
    /** 事件已到达 JS 监听器。同 session 的上一个未完结周期先按已有时间戳收尾。 */
    begin: function (sid) {
      var key = sid || "";
      var previous = currentBySession[key];
      if (previous) {
        // P2-2（流式中 supersede 丢帧段）：连续 delta 流里，下一条 begin 会先于
        // 上一条武装的 rAF 触发——帧等待被中止且不产生任何记录，"冻结在流式中
        // 途"这一最主要场景的最靠近呈现段恰恰测不到。若被中止的等待已超阈值，
        // 把中止时刻记为帧时刻（上报被中止的帧等待；无内容、仍受节流）。
        if (previous.t_dispatch != null && !previous.finalized) {
          var waited = now() - previous.t_dispatch;
          if (waited > STALL_DISPATCH_TO_FRAME_MS) {
            previous.t_frame = now();
          }
        }
        finalize(previous);
      }
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
      // r2 P1-1（平台无关修法）：窗口已隐藏时不武装 rAF——rAF 在隐藏期挂起、
      // 恢复可见后补触发，此刻可见性豁免已放行、量大得离谱的 dispatch→frame
      // 会被当成真 stall 上报（r1 的 visibilitychange 收尾只覆盖转变瞬间已存在
      // 的条目，隐藏期定时器腿的新武装漏网）。不武装、直接同步收尾——隐藏豁
      // 免自动吞掉上报，恢复可见后的下一条 delta 从头开始量。
      if (
        typeof document !== "undefined" &&
        document.visibilityState === "hidden"
      ) {
        entry.t_frame = entry.t_dispatch;
        finalize(entry);
      } else if (typeof window !== "undefined" && typeof window.requestAnimationFrame === "function") {
        // P2-4：隐藏窗口的合并帧走 ~1s 定时器腿会反复 markDispatched——不取消
        // 旧句柄就覆盖，流在隐藏中结束后每次 notify 都再武装一个 rAF，恢复可见
        // 一帧全触发。
        if (entry.rafHandle != null && typeof window.cancelAnimationFrame === "function") {
          window.cancelAnimationFrame(entry.rafHandle);
        }
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
