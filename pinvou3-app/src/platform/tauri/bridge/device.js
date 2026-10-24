/**
 * device feature for the Tauri bridge (MegaCube 设备管理).
 * Registered before bridge.js builds the backwards-compatible facade.
 */
(function (root) {
  "use strict";
  var registry = root.__PINVOU_TAURI_BRIDGE_FEATURES__ = root.__PINVOU_TAURI_BRIDGE_FEATURES__ || {};
  registry["device"] = function (context) {
    var state = context.state;
    var notify = context.notify;
    var invoke = context.invoke;
    var deviceIntervalId = null;
    var devicePollInFlight = false;
    var deviceServicesInFlight = false;

  // ── Device snapshot ──────────────────────────────────────────────

  // 字节人性化：≥1 GiB 用 GiB、其余 MiB（均 1 位小数），空值回退 "—"。
  // 单位后缀一律 ASCII；可本地化的词（SSD/风扇/接口类型等）由视图层负责，
  // 与 monitor 同口径（bridge 不放中文文案）。GB/MB 标签 = 二进制换算——与
  // shared/format-utils 的 fmtMiB 及内嵌运行状态卡同一**口径**，同一屏不再
  // 出现 "1.5 GiB" 与 "7.2 GB" 两套标签（round-5 review P3）。**不复用**
  // fmtMiB（round-21 review P3 复核结论）：它的 MB 臂直接输出原值
  // （`mib + " MB"`，非整 MiB 会带出多位小数、整 MiB 丢掉 ".0"），与本文件的
  // 一位小数口径不同——同口径不等于可互换，device_bridge_format 的字面量
  // 断言钉住的就是这里的一位小数显示契约。
  function fmtBytes(bytes) {
    if (bytes == null || typeof bytes !== "number" || !isFinite(bytes)) return "—";
    if (bytes >= 1024 * 1024 * 1024) return (bytes / (1024 * 1024 * 1024)).toFixed(1) + " GB";
    return (bytes / (1024 * 1024)).toFixed(1) + " MB";
  }

  // 速率人性化：十进制 KB/s / MB/s / GB/s（1 位小数），负值/非法值按 0 处理。
  // 入参是后端的 *_bytes_per_sec（字节/秒，字段名不用 "bps" 正是为避免按 bit
  // 误读）。刻意与 fmtBytes 的二进制口径不同（速率用 SI 十进制是网速惯例，
  // monitor 的 fmtRate 同款）：同一网卡卡上两行的 "GB" 基数不同是有意的取舍，
  // 单位标签区分在 "/s" 后缀（round-21 review P3 记录在案）。
  function fmtRate(bytesPerSec) {
    var v = typeof bytesPerSec === "number" && isFinite(bytesPerSec) && bytesPerSec > 0 ? bytesPerSec : 0;
    if (v >= 1e9) return (v / 1e9).toFixed(1) + " GB/s";
    if (v >= 1e6) return (v / 1e6).toFixed(1) + " MB/s";
    return (v / 1e3).toFixed(1) + " KB/s";
  }

  // 百分比：向下截 0、不封顶取整——多核机器上单进程 CPU% 可超 100，封顶会把
  // 真实占用藏成 "100%"；需要 0-100 封顶的场合（磁盘占用条宽）由调用方自行截。
  function fmtPct(pct) {
    var v = typeof pct === "number" && isFinite(pct) ? pct : 0;
    return Math.round(Math.max(0, v));
  }

  // 传感器 chip 名规约成视图侧本地化映射键（zh/en/ja 文案在视图的 i18n，
  // bridge 只给稳定 key + 原始 label）：nvme→固态硬盘、mt79*/wifi→无线网卡（键名
  // 用通用的 "wifi"，不借具体型号——第二个 wifi 芯片接入时不会再误导，五轮评审 P3）、
  // *fan*→风扇、acpitz/thermal→主板温度；未知 chip 保留小写原文，
  // 视图兜底直接展示原文。kind 参数仅在**整组都是风扇通道**时传 "fan"：
  // 后端组内按 kind 字母序排（fan 恒在最前），拿首条 kind 判断会让混装
  // 芯片（nct6798 的风扇+电压、acpitz 的风扇+温度）整卡被标成"风扇"
  // （第七轮评审 P3）。
  function sensorChipKey(chip, kind) {
    var c = String(chip || "").toLowerCase();
    if (c.indexOf("nvme") !== -1) return "nvme";
    if (c.indexOf("mt79") !== -1 || c.indexOf("wifi") !== -1 || c.indexOf("wireless") !== -1) return "wifi";
    if (c.indexOf("fan") !== -1) return "acpi_fan";
    if (kind === "fan") return "acpi_fan";
    if (c.indexOf("acpitz") !== -1 || c.indexOf("thermal") !== -1 || c.indexOf("acpi") !== -1) return "acpitz";
    return c || "other";
  }

  // 传感器数值文本：按 kind 定单位与精度（温度/转速取整，电压/电流/功率 1 位小数）。
  function sensorValueText(kind, value) {
    var v = typeof value === "number" && isFinite(value) ? value : null;
    if (v == null) return "—";
    if (kind === "temperature") return Math.round(v) + "°C";
    if (kind === "fan") return Math.round(v) + " RPM";
    if (kind === "voltage") return v.toFixed(1) + " V";
    if (kind === "current") return v.toFixed(1) + " A";
    if (kind === "power") return v.toFixed(1) + " W";
    return v.toFixed(1);
  }

  // 告警档位：≥critical → crit；温度量另有 critical-10 预警带。max 对温度是
  // **历史峰值**（hwmon tempN_highest，后端只在严格高于当前读数时才给），不是
  // 阈值——拿峰值当预警线，当前 45°C/峰值 46°C/critical 100°C 会恒 warn，升到
  // 46°C 后后端省略相等峰值反而"恢复"ok（十轮评审 P2）。峰值只用于展示，档位
  // 只看真正的安全阈值 critical。电压/电流/功率贴着 max 是常态读数（如 INA3221
  // 的 12.06V vs 12.1V max），套预警带会常年误报 warn；这些量只有真正越过
  // critical（或 max）才算 crit，其余一律 ok。
  function sensorLevel(kind, value, critical, max) {
    var v = typeof value === "number" && isFinite(value) ? value : null;
    if (v == null) return "ok";
    var hasCritical = typeof critical === "number" && isFinite(critical);
    var hasMax = typeof max === "number" && isFinite(max);
    if (hasCritical && v >= critical) return "crit";
    if (kind !== "temperature") {
      if (hasMax && v >= max) return "crit";
      return "ok";
    }
    if (hasCritical && v >= critical - 10) return "warn";
    return "ok";
  }

  function fmtSensors(groups) {
    return (Array.isArray(groups) ? groups : []).map(function (group) {
      var g = group || {};
      var entries = (Array.isArray(g.entries) ? g.entries : []).map(function (entry) {
        var e = entry || {};
        return {
          kind: e.kind || "",
          name: e.name ? String(e.name) : "—",
          valueText: sensorValueText(e.kind, e.value),
          // critical/max 与 valueText 同口径取整后再进抑制比较：视图按整数渲染
          // 阈值，12.4→12.6 这类被舍入抹平的抖动不该击穿显示等价抑制
          // （round-5 review P3）。level 判定仍吃原始值。
          critical:
            typeof e.critical === "number" && isFinite(e.critical) ? Math.round(e.critical) : null,
          max: typeof e.max === "number" && isFinite(e.max) ? Math.round(e.max) : null,
          level: sensorLevel(e.kind, e.value, e.critical, e.max),
        };
      });
      // chipKey 的风扇臂只在整组都是风扇通道时生效（见 sensorChipKey 注释）。
      var fanOnly =
        entries.length > 0 &&
        entries.every(function (entry) {
          return entry.kind === "fan";
        });
      return {
        chip: g.chip ? String(g.chip) : "—",
        chipKey: sensorChipKey(g.chip, fanOnly ? "fan" : null),
        entries: entries,
      };
    });
  }

  function fmtDisks(disks) {
    return (Array.isArray(disks) ? disks : []).map(function (disk) {
      var d = disk || {};
      var total = typeof d.total_bytes === "number" && isFinite(d.total_bytes) ? d.total_bytes : null;
      var available = typeof d.available_bytes === "number" && isFinite(d.available_bytes) ? d.available_bytes : null;
      // 用量是否真实可知（总/可用都到位且总量为正）：视图凭这个布尔决定渲染
      // 用量条还是"容量未知"占位——不再耦合 em-dash 展示字符串（有总量没可用
      // 的盘曾因此渲染成 0 宽绿条，读作"空盘"）。usedPct 在不可知时保持 0，
      // 只是永远不会被渲染（第四轮评审 P3）。
      var usageKnown = total != null && total > 0 && available != null;
      return {
        mountPoint: d.mount_point ? String(d.mount_point) : "—",
        fsType: d.fs_type ? String(d.fs_type) : "—",
        kindLabel: d.kind === "ssd" ? "SSD" : (d.kind === "hdd" ? "HDD" : null),
        totalText: fmtBytes(total),
        availableText: fmtBytes(available),
        usageKnown: usageKnown,
        usedPct: usageKnown ? Math.min(100, fmtPct((total - available) / total * 100)) : 0,
        isRoot: d.mount_point === "/",
      };
    });
  }

  var NET_TYPE_KEYS = ["ethernet", "wifi", "loopback", "bridge", "virtual", "other"];
  function fmtNetwork(ifaces) {
    return (Array.isArray(ifaces) ? ifaces : []).map(function (iface) {
      var n = iface || {};
      return {
        name: n.name ? String(n.name) : "—",
        typeKey: NET_TYPE_KEYS.indexOf(n.iface_type) !== -1 ? n.iface_type : "other",
        isUp: !!n.is_up,
        mac: n.mac ? String(n.mac) : null,
        rxTotalText: fmtBytes(n.rx_bytes),
        txTotalText: fmtBytes(n.tx_bytes),
        rxRateText: fmtRate(n.rx_bytes_per_sec),
        txRateText: fmtRate(n.tx_bytes_per_sec),
      };
    });
  }

  // 进程名后端已截断 40 字符；这里兜底再截一次，超长补省略号。
  function truncateName(name) {
    var n = name ? String(name) : "—";
    return n.length > 40 ? n.slice(0, 40) + "…" : n;
  }

  function fmtProcs(procs) {
    return (Array.isArray(procs) ? procs : []).map(function (proc) {
      var p = proc || {};
      return {
        pid: typeof p.pid === "number" && isFinite(p.pid) ? p.pid : 0,
        name: truncateName(p.name),
        cpuPctText: fmtPct(p.cpu_pct) + "%",
        memText: fmtBytes(p.memory_bytes),
      };
    });
  }

  function fmtGpuExt(gpu) {
    if (!gpu) return null;
    // 后端契约（十六轮评审）：compute_processes 为 null = 查询失败（不可知），
    // 数组 = 查询成功（可为空 = 真的零进程）。null 必须原样保留，视图靠它
    // 区分“没有计算进程”（隐藏区块）与“查询不可用”（显式提示）。
    var procs = Array.isArray(gpu.compute_processes) ? gpu.compute_processes : null;
    return {
      driverVersion: gpu.driver_version ? String(gpu.driver_version) : null,
      cudaVersion: gpu.cuda_version ? String(gpu.cuda_version) : null,
      computeProcesses: Array.isArray(procs)
        ? procs.map(function (proc) {
            var p = proc || {};
            return {
              pid: typeof p.pid === "number" && isFinite(p.pid) ? p.pid : 0,
              name: truncateName(p.name),
              usedText: fmtBytes(typeof p.used_mib === "number" && isFinite(p.used_mib) ? p.used_mib * 1024 * 1024 : null),
            };
          })
        : null,
    };
  }

  // DeviceView only consumes state.device._fmt, so deep equality means display
  // equivalence. The poll runs every 2 seconds; when a snapshot is
  // display-equivalent, skip the assignment + notify to avoid a full App
  // re-render while the device page is open. Numeric leftovers (disk usedPct
  // etc.) jitter naturally, so comparisons allow a 0.5 tolerance (prefer one
  // extra notify over ever getting stuck); most values are strings after the
  // _fmt formatting above and compare exactly. updatedAt is the snapshot's
  // generation clock: like monitor's page clock it must not break suppression
  // on its own — the view renders that frozen timestamp as a plain "updated
  // at HH:MM:SS" label (no ticking clock on the device page).
  function deviceFmtEqual(prev, next) {
    if (prev === next) return true;
    if (!prev || !next) return false;
    var numEq = function (a, b) {
      if (a === b) return true;
      return typeof a === "number" && typeof b === "number"
        && Number.isFinite(a) && Number.isFinite(b)
        && Math.abs(a - b) <= 0.5;
    };
    var valueEq = function (a, b) {
      if (a && b && typeof a === "object" && typeof b === "object") {
        if (Array.isArray(a) || Array.isArray(b)) {
          if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
          for (var i = 0; i < a.length; i++) {
            if (!valueEq(a[i], b[i])) return false;
          }
          return true;
        }
        var keys = Object.keys(b);
        if (keys.length !== Object.keys(a).length) return false;
        for (var j = 0; j < keys.length; j++) {
          if (!valueEq(a[keys[j]], b[keys[j]])) return false;
        }
        return true;
      }
      return numEq(a, b);
    };
    // updatedAt 是时钟字段:出现/缺失都不算差异(与下方循环的 skip 对应)。
    var countKeys = function (obj) {
      var total = 0;
      var keys = Object.keys(obj);
      for (var i = 0; i < keys.length; i++) {
        if (keys[i] !== "updatedAt") total += 1;
      }
      return total;
    };
    var topKeys = Object.keys(next);
    if (countKeys(next) !== countKeys(prev)) return false;
    for (var k = 0; k < topKeys.length; k++) {
      var key = topKeys[k];
      if (key === "updatedAt") continue;
      if (!valueEq(prev[key], next[key])) return false;
    }
    return true;
  }

  // Structural deep-equality for the services/external-models suppression
  // baselines. JSON.stringify was order-sensitive: a map-shaped field flipping
  // serialization order would defeat suppression (extra notifies, never wrong
  // data). Primitive comparison is strict — these payloads carry no jittering
  // numerics (the snapshot path keeps its 0.5 tolerance in deviceFmtEqual).
  function structuralEqual(a, b) {
    if (a === b) return true;
    if (!a || !b || typeof a !== "object" || typeof b !== "object") return false;
    if (Array.isArray(a) !== Array.isArray(b)) return false;
    if (Array.isArray(a)) {
      if (a.length !== b.length) return false;
      for (var i = 0; i < a.length; i++) {
        if (!structuralEqual(a[i], b[i])) return false;
      }
      return true;
    }
    var keys = Object.keys(a);
    if (keys.length !== Object.keys(b).length) return false;
    for (var j = 0; j < keys.length; j++) {
      if (!Object.prototype.hasOwnProperty.call(b, keys[j])) return false;
      if (!structuralEqual(a[keys[j]], b[keys[j]])) return false;
    }
    return true;
  }

  // 快照轮询世代号：pollDevice 过 in-flight 守卫后捕获 seq，invoke 决算后先对
  // 世代再动 state/notify，finally 只在最新世代清 in-flight 旗。否则
  // startDevicePolling 复位卡死旗（round-8）后，慢而未死的旧拍会在新拍在飞时
  // 再清一次旗（双发），旧快照/旧错误也可能晚于新拍落地——旧错误会在恢复后
  // 重新点亮横幅。startDevicePolling 重启时 bump 一次，让任何在飞旧拍整体
  // 作废：不能再发布、不能清旗、也不会压制新拍（round-9 review P2）。
  var devicePollSeq = 0;

  // eslint-disable-next-line sonarjs/cognitive-complexity -- legacy bridge; refactor tracked separately
  async function pollDevice() {
    if (devicePollInFlight) return;
    devicePollInFlight = true;
    var seq = ++devicePollSeq;
    try {
      var snap = await invoke("get_device_snapshot");
      // 世代已前进（重启过）→ 本拍整体作废：不发布、不动 state；下面的
      // finally 也不清 in-flight 旗（旗只归最新世代所有）。
      if (seq !== devicePollSeq) return;
      // The previous round errored → this round must notify once so the
      // "read failed" banner switches back to the normal panel.
      var hadDeviceError = !!state.deviceError;
      state.deviceError = null;
      var os = snap.os || {};
      snap._fmt = {
        osProductName: os.product_name ? String(os.product_name) : null,
        osPrettyName: os.pretty_name ? String(os.pretty_name) : null,
        kernel: os.kernel ? String(os.kernel) : "—",
        arch: os.arch ? String(os.arch) : "—",
        sensors: fmtSensors(snap.sensors),
        disks: fmtDisks(snap.disks),
        network: fmtNetwork(snap.network),
        processes: {
          topCpu: fmtProcs(snap.processes && snap.processes.top_cpu),
          topMemory: fmtProcs(snap.processes && snap.processes.top_memory),
        },
        gpuExt: fmtGpuExt(snap.gpu_ext),
        updatedAt: typeof snap.generated_at_ms === "number" && isFinite(snap.generated_at_ms)
          ? snap.generated_at_ms : Date.now(),
      };
      // Display-equivalent snapshots neither overwrite state.device nor
      // notify (must send on the first frame or after an errored round).
      var prevFmt = state.device && state.device._fmt;
      if (hadDeviceError || !prevFmt || !deviceFmtEqual(prevFmt, snap._fmt)) {
        state.device = snap;
        // 主线 #727 收敛为单快照发布,精确域通道已不存在;真正的限频是上面的
        // 结构比较——无变化不下发,变了才走全域 notify。
        notify();
      }
    } catch (e) {
      // 旧世代的错误晚到不得再动横幅：恢复拍可能早已落地，旧错误重新点亮
      // 就是横幅回归。
      if (seq !== devicePollSeq) return;
      // 同文案的失败拍不重复发布：后台 2s 轮询在故障期间每拍都会走到这里，
      // 无条件 notify 会让设备页每 2s 全量重渲染一块不变的横幅（第七轮评审
      // P3；services 通道同款守卫见下）。恢复拍由 success 分支的 hadDeviceError
      // 强制发布，横幅照常清除。
      var message = e && e.message ? e.message : String(e || "device poll failed");
      if (state.deviceError !== message) {
        state.deviceError = message;
        console.warn("[pinvou3][device] poll failed", e);
        notify();
      }
    } finally {
      if (seq === devicePollSeq) {
        devicePollInFlight = false;
      }
    }
  }

  function startDevicePolling() {
    if (deviceIntervalId) return;
    // A backend panic can leave devicePollInFlight stuck: the invoke promise
    // never settles, pollDevice's finally never runs, and every later tick
    // would no-op — the page freezes on stale data with no error banner.
    // Restart paths (mount/retry) are the recovery point, so clear the flag
    // here (round-8 review P3). 复位同时 bump 两个通道的世代号作双保险：作废
    // 的主机制是重启后新拍自己的 ++seq（见 pollDevice/pollDeviceServices），
    // 这里的显式 bump 只是把"复活瞬间的任何旧拍"也一并作废——单一机制已经
    // 足够（删掉这两行测试照绿，round-21 review P3），保留只为纵深，别把
    // 不变量归因到这一行。
    devicePollInFlight = false;
    devicePollSeq += 1;
    deviceServicesInFlight = false;
    deviceServicesPollSeq += 1;
    pollDevice();
    deviceIntervalId = setInterval(pollDevice, 2000);
  }
  function stopDevicePolling() {
    if (deviceIntervalId) {
      clearInterval(deviceIntervalId);
      deviceIntervalId = null;
    }
  }

  // ── Model services (slow path) ───────────────────────────────────
  // 服务探测独立于 2s 快照：视图按 10s 慢轮询或手动刷新时调用。
  // 有基线后的持续失败阈值：连续 SERVICES_STALE_FAILURES 拍（约 30s）仍失败才
  // 立错误旗。瞬时抖动（守护进程重启中的一两拍）不该把面板打坏，但探测真死
  // 也不能让过时的 running 状态无限期冒充事实（第六轮评审 P2）。
  var SERVICES_STALE_FAILURES = 3;
  var deviceServicesFailures = 0;
  // services 探测世代号：与快照通道同一纪律（round-9 review P3）。stale 世代
  // 的失败不得推进连续失败计数（否则阈值提前打满、错误旗早立），也不得替
  // 最新世代清 in-flight 旗；成功拍同样先对世代——stale 成功不得把计数清零。
  var deviceServicesPollSeq = 0;
  async function pollDeviceServices() {
    // 与快照轮询同一 in-flight 纪律：10s 定时 + 手动刷新可并发，docker 探测最坏
    // 3s+，上一拍未返回时本拍直接跳过，避免响应乱序把旧探测结果倒灌回来。
    if (deviceServicesInFlight) return;
    deviceServicesInFlight = true;
    var seq = ++deviceServicesPollSeq;
    try {
      var services = await invoke("get_device_services");
      // 世代已前进 → 本拍作废：不动 state、不动计数（成功清零也是计数变更）。
      if (seq !== deviceServicesPollSeq) return;
      var next = Array.isArray(services) ? services : [];
      deviceServicesFailures = 0;
      // 内容没变不下发（与快照的显示等价抑制同一目标）。
      // 首拍必须落地（state.deviceServices 还是 null）：基线用 null 短路，否则
      // "null || []" 会把首个空结果吞掉，视图的 loading 永远不退（第三轮评审 P2）。
      // 错误旗→null 的翻清也必须发布：否则恢复拍若内容恰好没变，常驻错误行
      // 会一直挂在页面上（第六轮评审 P2 附带修复）。
      var hadError = state.deviceServicesError !== null;
      state.deviceServicesError = null;
      var changed = hadError;
      if (!state.deviceServices || !structuralEqual(next, state.deviceServices)) {
        state.deviceServices = next;
        changed = true;
      }
      if (changed) notify();
    } catch (e) {
      // stale 世代的失败：不推进计数、不立旗、不留日志——它已不属于当下。
      if (seq !== deviceServicesPollSeq) return;
      // 探测失败保留上一份结果：瞬时抖动不该被渲染成「全部停止」。首拍就失败
      // 必须立即可见——null 悬着只会让视图的"探测中"无限驻留；有基线后连续
      // SERVICES_STALE_FAILURES 拍失败也要把错误旗立起来（旧 chips 保留显示，
      // 视图同时给出"状态可能过期"提示），探测死了不再静默冒充在线。
      deviceServicesFailures += 1;
      var surfaced =
        !state.deviceServices || deviceServicesFailures >= SERVICES_STALE_FAILURES;
      if (surfaced) {
        var message = e && e.message ? e.message : String(e || "services probe failed");
        if (state.deviceServicesError !== message) {
          // Same-message guard doubles as log throttling (matches the snapshot
          // channel above): a down daemon must not re-log every 10s tick
          // (round-21 review P3).
          console.warn("[pinvou3][device] services poll failed", e);
          state.deviceServicesError = message;
          notify();
        }
      }
    } finally {
      if (seq === deviceServicesPollSeq) {
        deviceServicesInFlight = false;
      }
    }
  }

  // ── External models ──────────────────────────────────────────────
  // 回读世代号：调用入口捕获 seq，invoke 决算后世代已前进（有更新的回读发起）
  // 则整包丢弃——不发布、不动 state，仅为调用方兼容返回当前
  // state.externalModels。register/remove 的内部回读在操作 invoke 决算之后才
  // 发起，seq 严格大于操作前发起的任何回读：操作前的慢回读无论多晚到都会被
  // 丢弃——被删的行不会复活、新登记的行不会消失；后发起的调用永远持更高
  // seq、永远胜出，其回读在时间上必然不早于被丢弃者（round-9 review P1）。
  var externalModelsFetchSeq = 0;
  async function fetchExternalModels() {
    var seq = ++externalModelsFetchSeq;
    var view = await invoke("get_external_models");
    if (seq !== externalModelsFetchSeq) {
      return state.externalModels;
    }
    // 无变化不下发（登记/移除后的回读常常逐字节一致，不值得一次全域重渲染）。
    // 结构比较而非 JSON.stringify：键序翻转不再击穿抑制（第四轮评审 P3）。
    var next = view || null;
    if (!structuralEqual(next, state.externalModels || null)) {
      state.externalModels = next;
      notify();
    }
    return state.externalModels;
  }

  // 注册/移除成功后回读注册表，保证 state.externalModels 与磁盘一致。
  async function registerExternalModel(payload) {
    var record = await invoke("device_register_external_model", payload || {});
    await fetchExternalModels();
    return record || null;
  }
  async function removeExternalModel(payload) {
    var outcome = await invoke("device_remove_external_model", payload || {});
    await fetchExternalModels();
    return outcome || null;
  }

  // ── AI doctor prompt ─────────────────────────────────────────────
  // 提示词模板与组装都在后端（features/device/doctor.rs）；bridge 只透传
  // kind 与自定义问题，返回拼好的完整提示词文本。
  function buildDoctorPrompt(kind, question) {
    return invoke("device_doctor_prompt", {
      kind: kind,
      question: question == null ? null : question,
    });
  }

    return {
      startDevicePolling: startDevicePolling,
      stopDevicePolling: stopDevicePolling,
      pollDevice: pollDevice,
      pollDeviceServices: pollDeviceServices,
      fetchExternalModels: fetchExternalModels,
      registerExternalModel: registerExternalModel,
      removeExternalModel: removeExternalModel,
      buildDoctorPrompt: buildDoctorPrompt,
      deviceFmtEqual: deviceFmtEqual
    };
  };
})(window);
