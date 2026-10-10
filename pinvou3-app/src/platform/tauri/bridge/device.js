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
    var notifyDomains = context.notifyDomains || notify;
    var invoke = context.invoke;
    var deviceIntervalId = null;
    var devicePollInFlight = false;
    var deviceServicesInFlight = false;

  // ── Device snapshot ──────────────────────────────────────────────

  // 字节人性化：≥1 GiB 用 GiB、其余 MiB（均 1 位小数），空值回退 "—"。
  // 单位后缀一律 ASCII；可本地化的词（SSD/风扇/接口类型等）由视图层负责，
  // 与 monitor 同口径（bridge 不放中文文案）。
  function fmtBytes(bytes) {
    if (bytes == null || typeof bytes !== "number" || !isFinite(bytes)) return "—";
    if (bytes >= 1024 * 1024 * 1024) return (bytes / (1024 * 1024 * 1024)).toFixed(1) + " GiB";
    return (bytes / (1024 * 1024)).toFixed(1) + " MiB";
  }

  // 速率人性化：十进制 KB/s / MB/s / GB/s（1 位小数），负值/非法值按 0 处理。
  // 入参是后端的 *_bytes_per_sec（字节/秒，字段名不用 "bps" 正是为避免按 bit 误读）。
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
  // bridge 只给稳定 key + 原始 label）：nvme→固态硬盘、mt79*/wifi→无线网卡、
  // *fan*/fan 通道→风扇、acpitz/thermal→主板温度；未知 chip 保留小写原文，
  // 视图兜底直接展示原文。
  function sensorChipKey(chip, kind) {
    var c = String(chip || "").toLowerCase();
    if (c.indexOf("nvme") !== -1) return "nvme";
    if (c.indexOf("mt79") !== -1 || c.indexOf("wifi") !== -1 || c.indexOf("wireless") !== -1) return "mt7925";
    if (c.indexOf("fan") !== -1 || kind === "fan") return "acpi_fan";
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

  // 告警档位：≥critical → crit；温度量另有 ±10 预警带（≥max-10 或 ≥critical-10
  // → warn）。±10 带只对温度有意义：电压/电流/功率贴着 max 是常态读数
  // （如 INA3221 的 12.06V vs 12.1V max），套预警带会常年误报 warn；这些量只有
  // 真正越过 critical（或 max）才算 crit，其余一律 ok。
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
    var nearMax = hasMax && v >= max - 10;
    var nearCrit = hasCritical && v >= critical - 10;
    if (nearMax || nearCrit) return "warn";
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
          critical: typeof e.critical === "number" && isFinite(e.critical) ? e.critical : null,
          max: typeof e.max === "number" && isFinite(e.max) ? e.max : null,
          level: sensorLevel(e.kind, e.value, e.critical, e.max),
        };
      });
      return {
        chip: g.chip ? String(g.chip) : "—",
        chipKey: sensorChipKey(g.chip, g.entries && g.entries[0] && g.entries[0].kind),
        entries: entries,
      };
    });
  }

  function fmtDisks(disks) {
    return (Array.isArray(disks) ? disks : []).map(function (disk) {
      var d = disk || {};
      var total = typeof d.total_bytes === "number" && isFinite(d.total_bytes) ? d.total_bytes : null;
      var available = typeof d.available_bytes === "number" && isFinite(d.available_bytes) ? d.available_bytes : null;
      return {
        mountPoint: d.mount_point ? String(d.mount_point) : "—",
        fsType: d.fs_type ? String(d.fs_type) : "—",
        kindLabel: d.kind === "ssd" ? "SSD" : (d.kind === "hdd" ? "HDD" : null),
        totalText: fmtBytes(total),
        availableText: fmtBytes(available),
        // 缺任一实测值不给百分比（宁缺勿把未知渲染成「已满/已空」）。
        usedPct: total != null && total > 0 && available != null
          ? Math.min(100, fmtPct((total - available) / total * 100)) : 0,
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
    var procs = Array.isArray(gpu.compute_processes) ? gpu.compute_processes : [];
    return {
      driverVersion: gpu.driver_version ? String(gpu.driver_version) : null,
      cudaVersion: gpu.cuda_version ? String(gpu.cuda_version) : null,
      computeProcesses: procs.map(function (proc) {
        var p = proc || {};
        return {
          pid: typeof p.pid === "number" && isFinite(p.pid) ? p.pid : 0,
          name: truncateName(p.name),
          usedText: fmtBytes(typeof p.used_mib === "number" && isFinite(p.used_mib) ? p.used_mib * 1024 * 1024 : null),
        };
      }),
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

  // eslint-disable-next-line sonarjs/cognitive-complexity -- legacy bridge; refactor tracked separately
  async function pollDevice() {
    if (devicePollInFlight) return;
    devicePollInFlight = true;
    try {
      var snap = await invoke("get_device_snapshot");
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
        // 精确域发布(与 monitor 同款):快照速率文本几乎每拍都变,全域 notify 会让
        // 设备页打开期间每 2s 触发一次全 21 域的结构化重投影(含整个 chat 树)。
        notifyDomains(["device"]);
      }
    } catch (e) {
      state.deviceError = e && e.message ? e.message : String(e || "device poll failed");
      console.warn("[pinvou3][device] poll failed", e);
      notifyDomains(["device"]);
    } finally {
      devicePollInFlight = false;
    }
  }

  function startDevicePolling() {
    if (deviceIntervalId) return;
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
  async function pollDeviceServices() {
    // 与快照轮询同一 in-flight 纪律：10s 定时 + 手动刷新可并发，docker 探测最坏
    // 3s+，上一拍未返回时本拍直接跳过，避免响应乱序把旧探测结果倒灌回来。
    if (deviceServicesInFlight) return;
    deviceServicesInFlight = true;
    try {
      var services = await invoke("get_device_services");
      var next = Array.isArray(services) ? services : [];
      // 内容没变不下发（与快照的显示等价抑制同一目标）；失败路径零变化也不 notify。
      // 首拍必须落地（state.deviceServices 还是 null）：基线用 null 短路，否则
      // "null || []" 会把首个空结果吞掉，视图的 loading 永远不退（第三轮评审 P2）。
      state.deviceServicesError = null;
      if (!state.deviceServices || JSON.stringify(next) !== JSON.stringify(state.deviceServices)) {
        state.deviceServices = next;
        notifyDomains(["device"]);
      }
    } catch (e) {
      // 探测失败保留上一份结果：瞬时抖动不该被渲染成「全部停止」。但首拍就失败
      // 必须可见——null 悬着只会让视图的"探测中"无限驻留，用户无从知道探测已坏。
      console.warn("[pinvou3][device] services poll failed", e);
      if (!state.deviceServices) {
        var message = e && e.message ? e.message : String(e || "services probe failed");
        if (state.deviceServicesError !== message) {
          state.deviceServicesError = message;
          notifyDomains(["device"]);
        }
      }
    } finally {
      deviceServicesInFlight = false;
    }
  }

  // ── External models ──────────────────────────────────────────────
  async function fetchExternalModels() {
    var view = await invoke("get_external_models");
    // 无变化不下发（登记/移除后的回读常常逐字节一致，不值得一次全域重渲染）。
    var next = view || null;
    if (JSON.stringify(next) !== JSON.stringify(state.externalModels || null)) {
      state.externalModels = next;
      notifyDomains(["device"]);
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
