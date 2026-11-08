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
 * 实现边界（r2 评审 P2-3/P2-4，r5 勘误，正文与这里保持一致）：
 *   - 独立上报的段差只有两段：recv→dispatch、dispatch→frame。t_sched 仅在
 *     同步直发路径补齐（不差量、不独立上报）；React 提交与最终呈现合并在
 *     dispatch→frame 一段里，测不出二者之间的停顿。
 *   - 后台会话的段量是混合竞速（r5 P2-2 勘误）：publish 对流属 sid 打
 *     markDispatched，但抑制态 notify 也可能提前盖章——后台段的 recv→dispatch
 *     既非纯失测也非纯可靠，是两者竞速的产物。抑制态直发钩子已加门控
 *     （!isNotifySuppressed），publish 侧仍无条件。
 *   - 长任务阻塞期间「未派发即被取代」的周期只有恰处已派发帧未回相位才
 *     补报一条（begin 的 supersede 补报），其余静默收尾。
 *   - 防补盖伪差（r4）：派发标记带 5s 新鲜度上界——后台会话存活数分钟的
 *     未派发条目被任意 notify() 补盖时不盖章、静默收尾（P2-A）；begin 发生
 *     在隐藏期的条目一律不上报——其时差含浏览器对隐藏定时器的节流（P2-B）。
 *   - →hidden 转变收尾的条目**可以上报**（r5 P2-1）：测量全在可见期，
 *     transitionFinalized 标记绕过隐藏豁免；武装条目按 supersede 先例补章
 *     t_frame。用户的自然冻结反应就是切走窗口——这些恰是最有价值的记录。
 *   - >5s 的真实派发冻结被 P2-A 上界静默吞——5s 新鲜度上界的已知取舍。
 *   - 合并竞速噪音（r7 P2-2 + r8 P1）：跨会话 publish 的 cancel-all 会杀掉
 *     其它会话已武装的合并帧，而被置换条目的内容其实已随该次发布上屏。
 *     活跃会话的滞留条目若不补章，会被 180ms 渲染腿补成假 stall 并烧掉该
 *     会话的 10s 节流配额（r8 P1：publish 现当场对被置换的活跃条目补
 *     markDispatched）；后台会话的同类条目打 throttledOrigin 只进 snapshot
 *     （r7 P2-2）。可见会话平日无此噪音的机理：32ms 合并发布腿先盖章，
 *     渲染腿落到已 finalize 的条目上是 no-op——不是"180ms 低于阈值"
 *     （180>120，原句是算术错误）；后台 ~1000ms 条目也只在跨会话竞速杀
 *     pend 时出现，独流后台会话会被自己的 32ms 帧在 ~16ms 盖章。
 *   - 先章有效分支对等待超阈值的条目补章 t_frame（r7 P2-3）：冻结中排队的
 *     终态 notify 解冻后先于 rAF 执行，不补章则该条 dispatch→frame 为 null、
 *     真实冻结零记录。t_dispatch 首章有效，归因不颠倒。等待**未**超阈值的
 *     条目不补章也不收尾（r12 P2-3 帧腿保留）：刚武装的 rAF 帧腿照常落章
 *     量终态提交 paint，finalize 只发生在补章/帧回调/visibility/begin 收口。
 *   - markThrottled 只打标不收尾，滞留条目由 begin 入口的扫尾按 5s 新鲜度
 *     上界有界收口（r12 P2-4）：先标后章（r7 P2-2）语义原样保留，等不来
 *     章的未派发条目在任何后续活动后 5s 内被移出 currentBySession。
 */
(function (root) {
  "use strict";

  // desktop-only（接线边界）：钩子仅装于 tauri lane（bridge/chat-events 的
  // onSessionEvent/publish 路径）；web 宿主的 platform/web/bridge.js 有自己的
  // chat:delta 监听但未接探针——web 流式冻结当前不可诊断是未接线，非不能接线。
  var RING_SIZE = 64;
  var STALL_RECV_TO_DISPATCH_MS = 120;
  var STALL_DISPATCH_TO_FRAME_MS = 120;
  // 派发标记的新鲜度上界（r4 P2-A）：后台会话的未派发条目可存活数分钟，
  // 任意事件路径的 notify() 补盖 t_dispatch 会把它变成分钟级假 stall。
  // 正常合并节奏是 rAF/32ms、隐藏期 ~1s，5s 上界足够宽不吞真实卡顿。
  var STALE_DISPATCH_LIMIT_MS = 5000;
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
    // 周期都误判成 stall。隐藏期间不上报——**例外**（r5 P2-1）：转变收尾的
    // 条目打有 transitionFinalized 标记，其测量全部落在可见期，放行上报。
    if (
      typeof document !== "undefined" &&
      document.visibilityState === "hidden" &&
      !entry.transitionFinalized
    ) {
      return;
    }
    // r4 P2-B：begin 发生在隐藏期的条目，其 recv→dispatch 间隔含浏览器对隐藏
    // 窗口定时器的节流（标准节流 ~1s 桶）——恢复可见后定时器腿落地会测得 ~1s
    // 的假 stall，且几乎每次「流式中切走再切回」都产一条。探针分不清「应用
    // 卡了」和「浏览器节流」，隐藏期起源的条目一律不上报（snapshot 仍可见，
    // 只是不进诊断流）。
    if (entry.hiddenOrigin) return;
    // r7 P2-2：后台预览腿的合并节流被当成 stall 上报过（后台会话生成停顿
    // ≥1s 时每会话每 10s 一条 ~1032ms 记录，受控时钟实测）——那 ~1s 是应用
    // 自己 BACKGROUND_STREAM_RENDER_THROTTLE_MS 的节流，不是主线程冻结，
    // 节律性假信号会支配诊断流。打标条目不上报（snapshot 仍可见）。
    if (entry.throttledOrigin) return;
    // 先确认诊断模块可用、再消耗节流窗口（评审 P3：诊断缺失/record 抛错时
    // 不该把该会话的 10s 节流窗口白白烧掉）。
    var diagnostics = root.PinvouAuthoritySyncDiagnostics;
    if (!diagnostics || typeof diagnostics.record !== "function") return;
    var t = now();
    // r6 P2-A：never-recorded 用 -Infinity 而非 0——performance.now() 纪元
    // 是页面加载时刻，|| 0 把"从未上报"当"在第 0ms 上报过"，启动后前 10s
    // 的 stall 全部静默丢弃（启动恰是冻结高发窗口）。
    var last = lastStallRecordAt[entry.sid];
    if (last === undefined) last = -Infinity;
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
  // 后才触发——此刻 finalize 量到的 dispatch_to_frame 横跨整个隐藏时段。
  // 「边生成边切走窗口」是常见用法。窗口隐藏时主动收尾所有武装条目。
  //
  // r5 P2-1：转变收尾运行时 visibilityState 已是 hidden，finalize 内的上报
  // 时刻隐藏豁免必然先行拦截——测量全部落在可见期却被静默丢弃，恰是最有
  // 价值的冻结记录（用户对冻结的自然反应就是切走窗口）。修法：转变收尾的
  // 条目按 supersede 先例补章 t_frame（等待超阈值时）后，绕过隐藏豁免上报。
  if (typeof document !== "undefined" && typeof document.addEventListener === "function") {
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState !== "hidden") return;
      var entries = Object.keys(currentBySession);
      for (var i = 0; i < entries.length; i++) {
        var entry = currentBySession[entries[i]];
        if (!entry || entry.finalized) continue;
        // 武装条目（已派发、帧未回）：按 supersede 先例——等待已超阈值则
        // 把此刻记为帧时刻（测量诚实地覆盖到切走瞬间），否则不补章。
        if (entry.t_dispatch != null) {
          var waited = now() - entry.t_dispatch;
          if (waited > STALL_DISPATCH_TO_FRAME_MS) {
            entry.t_frame = now();
          }
        }
        // 标记为转变收尾——finalize 的隐藏豁免对此条目放行（测量全在可见期）
        entry.transitionFinalized = true;
        finalize(entry);
      }
    });
  }

  // r12 P2-4：滞留打标条目的有界收口。原实现只打标不收尾——条目滞留到该
  // sid 下次 begin 或窗口隐藏，currentBySession 对环形淘汰免疫（_liveEntry
  // 仍取到同引用），后台终态/被弃会话的未派发条目无界滞留。此处按派发章
  // 同款的 5s 新鲜度上界扫尾：**先标后章**（r7 P2-2）不受影响——后台预览腿
  // 打标后 ~16-32ms 内的合法补章远早于上界；只有等不来章的滞留条目被收走。
  // 挂在 begin（探针最高频入口）上摊销，任何会话的后续活动都会清其它 sid
  // 的滞留条目。
  function sweepStaleThrottled() {
    var keys = Object.keys(currentBySession);
    for (var i = 0; i < keys.length; i++) {
      var entry = currentBySession[keys[i]];
      if (
        entry && !entry.finalized &&
        entry.throttledOrigin && entry.t_dispatch == null &&
        entry.t_recv != null && now() - entry.t_recv > STALE_DISPATCH_LIMIT_MS
      ) {
        finalize(entry);
      }
    }
  }

  root.RenderStageProbe = {
    /** 事件已到达 JS 监听器。同 session 的上一个未完结周期先按已有时间戳收尾。 */
    begin: function (sid) {
      sweepStaleThrottled();
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
      var beganHidden =
        typeof document !== "undefined" && document.visibilityState === "hidden";
      var entry = { sid: key, seq: ++seq, t_recv: now(), t_sched: null, t_dispatch: null, t_frame: null, rafHandle: null, hiddenOrigin: beganHidden, finalized: false };
      currentBySession[key] = entry;
      ring.push(entry);
      if (ring.length > RING_SIZE) ring.shift();
      return entry;
    },
    /** 合并帧已排上（rAF/32ms 定时器已武装）。t_sched 兼作派发新鲜度的参考点。 */
    markScheduled: function (sid) {
      var entry = currentBySession[sid || ""];
      if (entry && entry.t_sched == null) entry.t_sched = now();
    },
    /** r7 P2-2：这条目目的 recv→dispatch 间隔含**应用自己的**合并节流（后台
     * 会话 1s 预览腿），不是主线程冻结——打标后 finalize 不上报（snapshot
     * 保留，段差仍可查）。调用方是 chat-events 的后台渲染腿。
     * r9 P2：只对**未盖章**的条目打标。已盖章（publish 已跑、React 状态已
     * 更新）的条目处于「等下一帧」段——它的 dispatch→frame 间隔量的是真实
     * 渲染/冻结，无条件打标会把真实冻结吞掉（受控时钟三几何亲证：流中切走/
     * 冻结跨帧等待/自然多会话流——最常态的「看着 A、B 在生成」场景正中）。
     * 这一行守卫对 r7-P2-2 的抑制语义（未盖章条目）零影响。 */
    markThrottled: function (sid) {
      var entry = currentBySession[sid || ""];
      if (entry && !entry.finalized && entry.t_dispatch == null) entry.throttledOrigin = true;
    },
    /** 可见发布已执行（React 状态已更新）。缺 scheduled 视为同步路径，补齐。 */
    markDispatched: function (sid) {
      var entry = currentBySession[sid || ""];
      if (!entry || entry.finalized) return;
      // r6 P2-B：先章有效——主线程冻结期间排队的终态事件（chat:done 等
      // ~30 个 handler 的 notify）在解冻后先于迟到的 rAF 执行，无条件覆盖
      // 会把冻结从 dispatch→frame 段搬到 recv→dispatch 段（分段归因颠倒）。
      // 直发路径两盖章方互斥，先章有效不影响。
      if (entry.t_dispatch != null) {
        // 已有派发章：只更新 t_sched（如有），不动 t_dispatch
        if (entry.t_sched == null) entry.t_sched = entry.t_dispatch;
        // r7 P2-3：先章有效分支的补章——终态 notify 在冻结中排队、解冻后先于
        // 迟到的 rAF 执行时，帧还没回来。不补章的话这条 dispatch→frame 就是
        // null，真实冻结零记录（受控时钟实测 recorded:[]）。与 begin-supersede/
        // 转变收尾同一先例：等待超阈值才补章；补 t_frame 不动 t_dispatch，
        // 分段归因不颠倒（recv→dispatch 保持在首章量级）。
        if (entry.t_frame == null && now() - entry.t_dispatch > STALL_DISPATCH_TO_FRAME_MS) {
          entry.t_frame = now();
          finalize(entry);
          return;
        }
        // r12 P2-3 帧腿保留：等待未超阈值（正常合并节奏 ~16ms 间隙恰在此带）
        // 时**不补章也不收尾**——此刻 rAF 帧腿刚武装，finalize 会取消它，
        // 终态提交 paint 的真实呈现时刻被系统性失测（每条终态都走这臂）。
        // 让已武装的帧回调自己落章：markFrame 量到 t_frame 后 finalize。
        // 兜底不变：窗口隐藏由 visibilitychange 收尾、supersede 由 begin 收尾。
        return;
      }
      // r4 P2-A（陈旧条目新鲜度上界）：后台会话的最后一条 delta 停在「未派发」
      // 状态可存活数分钟（合并帧 publish 只标记活跃 sid、chat:done 不调 notify
      // 收尾它），期间任意事件路径的 notify() 都会无条件补盖 t_dispatch——
      // recv→dispatch 当场变成分钟级假 stall。派发标记只对新鲜条目有效：距
      // 调度参考点（t_sched，直发路径为 t_recv）超过上界的条目静默收尾（不
      // 盖章 → 段差为 null → 不上报）。
      var freshRef = entry.t_sched != null ? entry.t_sched : entry.t_recv;
      if (freshRef != null && now() - freshRef > STALE_DISPATCH_LIMIT_MS) {
        finalize(entry);
        return;
      }
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
        // r8 P2-3：r6 先章有效后，rafHandle 非空蕴含 t_dispatch 非空、条目已在
        // 上面的先章分支 return——「覆盖前取消旧句柄」的分支不可达（变异亲证：
        // 删除后全套件绿），随防御性代码删除。
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
