// MegaCube 设备管理 bridge（src/platform/tauri/bridge/device.js）格式化与轮询契约。
// 仿 monitor_bridge_format.test.mjs 的定位：锁 _fmt 的显示口径（GB/MB、
// KB-GB/s、°C/RPM/V/A/W、告警档位），防止单侧漂移。可本地化的词（接口类型、
// 传感器 chip 名）在 bridge 里只给稳定 key + 原文，中文文案由视图 i18n 负责，
// 这里额外锁「bridge 不新增中文字面量」。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(
  path.join(root, 'src', 'platform', 'tauri', 'bridge', 'device.js'),
  'utf8',
);

// 与 memory_bridge_state.test.mjs 同款 vm 装载：device.js 是 classic script，
// 经 window.__PINVOU_TAURI_BRIDGE_FEATURES__ 注册工厂，直接以受控 context 调用。
function loadDeviceFeature() {
  const windowObject = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  let intervalFn = null;
  let intervalMs = 0;
  let startedCount = 0;
  let clearedCount = 0;
  vm.runInNewContext(source, {
    window: windowObject,
    console,
    setTimeout,
    clearTimeout,
    setInterval(fn, ms) { intervalFn = fn; intervalMs = ms; startedCount += 1; return 42; },
    clearInterval(id) { if (id === 42) clearedCount += 1; intervalFn = null; },
  });
  const state = { device: null, deviceError: null, deviceServices: null, deviceServicesError: null, externalModels: null };
  let notifyCount = 0;
  const invokes = [];
  let respond = async () => null;
  const invoke = async (command, args) => {
    invokes.push([command, args]);
    return respond(command, args);
  };
  const factory = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__.device;
  assert.equal(typeof factory, 'function', 'device.js must register a bridge feature factory');
  // 主线 #727 收敛后只有全域 notify 一条通道；发布与否/几次由 notifyCount 钉。
  const api = factory({
    state,
    notify() { notifyCount += 1; },
    invoke,
  });
  return {
    api, state, invokes,
    setRespond(fn) { respond = fn; },
    notifyCount: () => notifyCount,
    interval: () => ({ fn: intervalFn, ms: intervalMs, started: startedCount, cleared: clearedCount }),
  };
}

const fixtureSnapshot = () => ({
  generated_at_ms: 1700000000000,
  os: {
    product_name: 'LinSeer MegaCube',
    pretty_name: 'Ubuntu 24.04.3 LTS',
    kernel: '6.11.0-1019-nvidia',
    arch: 'aarch64',
  },
  sensors: [
    { chip: 'nvme', entries: [{ kind: 'temperature', name: 'Composite', value: 45, critical: 84, max: 74 }] },
    { chip: 'acpitz', entries: [
      { kind: 'temperature', name: 'thermal_zone', value: 79.4, critical: 85, max: null },
      { kind: 'temperature', name: 'critical_zone', value: 86, critical: 85, max: null },
    ] },
    { chip: 'acpi_fan', entries: [{ kind: 'fan', name: 'fan1', value: 1200, critical: null, max: null }] },
    { chip: 'mt7925_wifi', entries: [
      { kind: 'temperature', name: 'wifi_temp', value: 52.3, critical: 90, max: 80 },
      // 峰值 80 不是预警线（十轮评审 P2）：72 距 critical 90 尚远，必须 ok。
      { kind: 'temperature', name: 'wifi_peak_not_threshold', value: 72, critical: 90, max: 80 },
    ] },
    { chip: 'INA3221', entries: [
      { kind: 'voltage', name: 'in0', value: 12.06, critical: null, max: 12.1 },
      { kind: 'voltage', name: 'in1', value: 13.2, critical: 13, max: null },
      // 越过 max 但未到 critical 的电压（无 critical 臂）：非温度类的 max 就
      // 是红线，必须 crit——此前 fixture 没有该形态的正例，删掉 max 臂实现
      // 套件照绿（round-6 review P2 变异存活项）。
      { kind: 'voltage', name: 'in2', value: 12.6, critical: null, max: 12.1 },
      { kind: 'current', name: 'curr1', value: 3.2, critical: null, max: null },
      { kind: 'power', name: 'power1', value: 28.46, critical: null, max: null },
    ] },
  ],
  disks: [
    { name: 'nvme0n1p2', mount_point: '/', fs_type: 'ext4', kind: 'ssd', total_bytes: 1610612736, available_bytes: 805306368, is_removable: false },
    { name: 'sda1', mount_point: '/media/usb', fs_type: 'vfat', kind: 'hdd', total_bytes: 3221225472, available_bytes: 3221225472, is_removable: true },
    { name: null, mount_point: '/snap/core', fs_type: 'squashfs', kind: 'unknown', total_bytes: null, available_bytes: null, is_removable: false },
    // 有总量没可用（损坏/半初始化的盘）：usageKnown 必须 false——曾渲染成
    // 0 宽绿条，读作"空盘"（第四轮评审 P3）。
    { name: 'partial', mount_point: '/mnt/partial', fs_type: 'ext4', kind: 'ssd', total_bytes: 4096, available_bytes: null, is_removable: false },
  ],
  network: [
    { name: 'lo', iface_type: 'loopback', is_up: true, mac: null, rx_bytes: 1610612736, tx_bytes: 1610612736, rx_bytes_per_sec: 2500000, tx_bytes_per_sec: 0 },
    { name: 'wlP1s0', iface_type: 'wifi', is_up: true, mac: 'aa:bb:cc:dd:ee:ff', rx_bytes: 0, tx_bytes: 1048576, rx_bytes_per_sec: 1500000000, tx_bytes_per_sec: -5 },
    { name: 'docker0', iface_type: 'weird', is_up: false, mac: '00:11:22:33:44:55', rx_bytes: null, tx_bytes: null, rx_bytes_per_sec: null, tx_bytes_per_sec: null },
  ],
  processes: {
    top_cpu: [
      { pid: 1235, name: 'build', cpu_pct: 250, memory_bytes: 1024 },
      { pid: 1234, name: 'ollama serve', cpu_pct: 91.6, memory_bytes: 1610612736 },
    ],
    top_memory: [{ pid: 99, name: 'x'.repeat(50), cpu_pct: 0.2, memory_bytes: 1073741824 }],
  },
  gpu_ext: {
    driver_version: '580.02',
    cuda_version: '13.0',
    compute_processes: [{ pid: 7, name: 'python3', used_mib: 1536 }],
  },
});

const flush = () => new Promise(resolve => setImmediate(resolve));

// ── _fmt 组装与格式化 ─────────────────────────────────────────────
{
  const env = loadDeviceFeature();
  env.setRespond(async command => command === 'get_device_snapshot' ? fixtureSnapshot() : null);
  await env.api.pollDevice();

  assert.equal(env.state.deviceError, null);
  const fmt = env.state.device._fmt;
  assert.deepEqual(
    Object.keys(fmt).sort(),
    ['arch', 'disks', 'gpuExt', 'kernel', 'network', 'osProductName', 'osPrettyName', 'processes', 'sensors', 'updatedAt'].sort(),
    '_fmt must expose the full display contract',
  );
  assert.equal(fmt.osProductName, 'LinSeer MegaCube');
  assert.equal(fmt.osPrettyName, 'Ubuntu 24.04.3 LTS');
  assert.equal(fmt.kernel, '6.11.0-1019-nvidia');
  assert.equal(fmt.arch, 'aarch64');
  assert.equal(fmt.updatedAt, 1700000000000);

  // 字节：1610612736 → '1.5 GB'（GB/MB 标签 = 二进制换算，与 monitor/内嵌卡同口径）；非法值回退 —。
  assert.equal(fmt.disks[0].totalText, '1.5 GB');
  assert.equal(fmt.disks[0].availableText, '768.0 MB');
  // 用量百分比的 50% 路径必须钉死：公式算反（分母换成 available）或封顶丢失
  // 这类显示回归此前全绿通过（变异存活，第三轮评审 P2）。
  assert.equal(fmt.disks[0].usedPct, 50);
  // usageKnown：视图凭它决定画用量条还是"容量未知"占位，不再耦合 em-dash
  // 展示串（第四轮评审 P3）。
  assert.equal(fmt.disks[0].usageKnown, true);
  assert.equal(fmt.disks[1].totalText, '3.0 GB');
  assert.equal(fmt.disks[1].usedPct, 0);
  assert.equal(fmt.disks[2].totalText, '—');
  assert.equal(fmt.disks[2].usedPct, 0);
  assert.equal(fmt.disks[2].usageKnown, false, 'missing total must mark usage unknown');
  // 空盘（available == total）的用量是真实可知的：usageKnown 保持 true。
  assert.equal(fmt.disks[1].usageKnown, true);
  // 有总量没可用：usageKnown=false（曾渲染成 0 宽绿条，读作"空盘"）。
  assert.equal(fmt.disks[3].usageKnown, false, 'total-without-available must be unknown usage');
  assert.equal(fmt.disks[3].usedPct, 0);
  assert.equal(fmt.disks[0].mountPoint, '/');
  assert.equal(fmt.disks[0].isRoot, true);
  assert.equal(fmt.disks[1].isRoot, false);
  assert.equal(fmt.disks[0].fsType, 'ext4');
  assert.equal(fmt.disks[0].kindLabel, 'SSD');
  assert.equal(fmt.disks[1].kindLabel, 'HDD');
  assert.equal(fmt.disks[2].kindLabel, null);

  // 传感器：单位/精度 per kind；chipKey 只给稳定键，原文 name 原样保留。
  const [nvme, acpitz, fan, wifi, power] = fmt.sensors;
  assert.equal(nvme.chipKey, 'nvme');
  assert.equal(nvme.chip, 'nvme');
  assert.equal(nvme.entries[0].name, 'Composite');
  assert.equal(nvme.entries[0].valueText, '45°C');
  assert.equal(nvme.entries[0].level, 'ok');
  assert.equal(nvme.entries[0].critical, 84);
  assert.equal(nvme.entries[0].max, 74);
  // ≥ critical-10 → warn；≥ critical → crit。
  assert.equal(acpitz.chipKey, 'acpitz');
  assert.equal(acpitz.entries[0].valueText, '79°C');
  assert.equal(acpitz.entries[0].level, 'warn');
  assert.equal(acpitz.entries[1].level, 'crit');
  assert.equal(fan.chipKey, 'acpi_fan');
  assert.equal(fan.entries[0].valueText, '1200 RPM');
  assert.equal(fan.entries[0].level, 'ok');
  assert.equal(wifi.chipKey, 'wifi');
  assert.equal(wifi.entries[0].valueText, '52°C');
  // 79.4 四舍五入后是 79，但告警档位必须用原始值判定（≥ critical-10 前 52.3 未达 80）。
  assert.equal(wifi.entries[0].level, 'ok');
  // 历史峰值不是预警线（十轮评审 P2）：max=80 只是 tempN_highest，72 距
  // critical 90 还差 18，旧口径 ≥ max-10 会误报 warn。
  assert.equal(wifi.entries[1].level, 'ok');
  // 未知 chip 保留小写原文做 key，交给视图兜底。
  assert.equal(power.chipKey, 'ina3221');
  assert.equal(power.entries[0].valueText, '12.1 V');
  // ±10 预警带只属于温度：12.06V 贴着 12.1V max 是常态读数，必须保持 ok
  // （旧口径会落进 max-10=2.1 的 warn）；真正越过 critical 才升 crit。
  assert.equal(power.entries[0].level, 'ok');
  assert.equal(power.entries[1].valueText, '13.2 V');
  assert.equal(power.entries[1].level, 'crit');
  // 非温度类越过 max（未到 critical，此臂无 critical）必须 crit（round-6 P2）。
  assert.equal(power.entries[2].valueText, '12.6 V');
  assert.equal(power.entries[2].level, 'crit');
  assert.equal(power.entries[3].valueText, '3.2 A');
  assert.equal(power.entries[3].level, 'ok');
  assert.equal(power.entries[4].valueText, '28.5 W');
  assert.equal(power.entries[4].level, 'ok');

  // 网络：typeKey 白名单校验、累计与速率文本；非法/负速率按 0。
  assert.equal(fmt.network[0].typeKey, 'loopback');
  assert.equal(fmt.network[0].rxTotalText, '1.5 GB');
  assert.equal(fmt.network[0].rxRateText, '2.5 MB/s');
  assert.equal(fmt.network[0].txRateText, '0.0 KB/s');
  assert.equal(fmt.network[1].typeKey, 'wifi');
  assert.equal(fmt.network[1].rxRateText, '1.5 GB/s');
  assert.equal(fmt.network[1].txRateText, '0.0 KB/s');
  assert.equal(fmt.network[1].mac, 'aa:bb:cc:dd:ee:ff');
  assert.equal(fmt.network[2].typeKey, 'other');
  assert.equal(fmt.network[2].isUp, false);
  assert.equal(fmt.network[2].rxTotalText, '—');
  assert.equal(fmt.network[0].mac, null);

  // 进程：CPU 取整百分比 + 内存字节；超长名兜底截断。
  // 多核 >100% 不封顶：封顶会把真实占用藏成 "100%"（fmtPct 只截 0 不封顶）。
  assert.equal(fmt.processes.topCpu[0].pid, 1235);
  assert.equal(fmt.processes.topCpu[0].cpuPctText, '250%');
  assert.equal(fmt.processes.topCpu[1].pid, 1234);
  assert.equal(fmt.processes.topCpu[1].cpuPctText, '92%');
  assert.equal(fmt.processes.topCpu[1].memText, '1.5 GB');
  assert.equal(fmt.processes.topMemory[0].name.length, 41);
  assert.ok(fmt.processes.topMemory[0].name.endsWith('…'));

  // GPU 扩展：驱动/CUDA 透传，显存占用走同一字节口径；无 GPU → null。
  assert.equal(fmt.gpuExt.driverVersion, '580.02');
  assert.equal(fmt.gpuExt.cudaVersion, '13.0');
  assert.equal(fmt.gpuExt.computeProcesses[0].usedText, '1.5 GB');

  env.setRespond(async () => ({ generated_at_ms: 1700000000001, os: {} }));
  await env.api.pollDevice();
  const degraded = env.state.device._fmt;
  assert.equal(degraded.osProductName, null, 'missing os fields must fall back to null (view supplies the name)');
  assert.equal(degraded.kernel, '—');
  assert.equal(degraded.gpuExt, null, 'gpu_ext None must hide the GPU section');
  // device.js 在 vm 域里建数组,原型与本域不同:展开成宿主域副本再比较。
  assert.deepEqual([...degraded.sensors], []);
  assert.deepEqual([...degraded.disks], []);
  assert.deepEqual([...degraded.network], []);
  assert.equal(degraded.processes.topCpu.length, 0);
  assert.equal(degraded.processes.topMemory.length, 0);
}

// bridge 不新增中文字面量：可本地化词全部住在视图 i18n（设计 §3.1/§8）。
// 中文注释合法（与 monitor.js 一致），故剥离注释后再扫。剥离须先归一换行
// （与 aux_chat_panel_contract 同款）：Windows checkout（autocrlf）把源码读成
// CRLF 后，逐行 `//` 剥离的正则对行尾 \r 失配，中文注释残留会让扫描误红
// （评审环境连续三轮实录的失败点）。
const stripComments = text => text
  .replace(/\r\n?/g, '\n')
  .replace(/\/\*[\s\S]*?\*\//g, comment => comment.replace(/[^\n]/g, ''))
  .split('\n')
  .map(line => line.replace(/\/\/.*$/, ''))
  .join('\n');
assert.doesNotMatch(
  stripComments(source),
  /[\u{4e00}-\u{9fff}]/u,
  'device bridge must stay free of Chinese literals',
);
// 钉死 CRLF 输入与 LF 等价：归一被删掉时本断言先红（而不是等某个 Windows
// 环境的源码扫描）。
{
  const lf = 'const a = 1; // 中文注释\n/* 块注\n释 */\nconst b = 2;\n';
  assert.doesNotMatch(stripComments(lf.replace(/\n/g, '\r\n')), /[\u{4e00}-\u{9fff}]/u);
  assert.equal(stripComments(lf), stripComments(lf.replace(/\n/g, '\r\n')));
}

// ── 显示等价抑制（deviceFmtEqual） ────────────────────────────────
{
  const env = loadDeviceFeature();
  const eq = env.api.deviceFmtEqual;
  const base = { a: 'x', usedPct: 50, nested: { v: 1 }, list: [{ k: 'a', n: 1 }] };
  assert.equal(eq(base, { ...base }), true);
  // 数值容差 0.5（含等号边界）：抖动不触发；超过容差不等于。
  assert.equal(eq(base, { ...base, usedPct: 50.4 }), true);
  assert.equal(eq(base, { ...base, usedPct: 50.5 }), true);
  assert.equal(eq(base, { ...base, usedPct: 51 }), false);
  // updatedAt 是时钟字段：不因前进而打破抑制。
  assert.equal(eq(base, { ...base, updatedAt: 2 }), true);
  assert.equal(eq({ ...base, updatedAt: 1 }, { ...base, updatedAt: 2 }), true);
  assert.equal(eq({ ...base, updatedAt: 1 }, { ...base, a: 'y', updatedAt: 2 }), false);
  // 嵌套对象/数组逐项比较；长度变化必须判定不等。
  assert.equal(eq(base, { ...base, nested: { v: 1.2 } }), true);
  assert.equal(eq(base, { ...base, nested: { v: 2 } }), false);
  assert.equal(eq(base, { ...base, list: [{ k: 'a', n: 1 }, { k: 'b', n: 2 }] }), false);
  assert.equal(eq(base, { ...base, list: [{ k: 'a', n: 2 }] }), false);
  assert.equal(eq(null, base), false);
  assert.equal(eq(base, null), false);
  assert.equal(eq(base, base), true);

  // 轮询抑制：显示等价的快照不覆盖 state.device、不 notify。
  env.setRespond(async () => fixtureSnapshot());
  await env.api.pollDevice();
  const first = env.state.device;
  const notifiesAfterFirst = env.notifyCount();
  assert.equal(notifiesAfterFirst, 1);
  await env.api.pollDevice();
  assert.equal(env.state.device, first, 'display-equal snapshot must not replace state.device');
  assert.equal(env.notifyCount(), notifiesAfterFirst, 'display-equal snapshot must not notify');

  // 真实变化（可用字节变化 → availableText 变）→ 覆盖 + notify。
  const changed = fixtureSnapshot();
  changed.disks[0].available_bytes = 801112064;
  env.setRespond(async () => changed);
  await env.api.pollDevice();
  assert.notEqual(env.state.device, first);
  assert.equal(env.state.device._fmt.disks[0].availableText, '764.0 MB');
  assert.equal(env.notifyCount(), notifiesAfterFirst + 1);

  // 封顶：异常负的 available（损坏的盘报数）不得把占用推过 100。
  const capped = fixtureSnapshot();
  capped.disks[0].available_bytes = -1610612736;
  env.setRespond(async () => capped);
  await env.api.pollDevice();
  assert.equal(env.state.device._fmt.disks[0].usedPct, 100, 'usedPct must cap at 100');

  // null 读数：文本回退 —、档位 ok——绝不能渲染成 0°C（第三轮评审 P2）。
  const nulled = fixtureSnapshot();
  nulled.sensors[0].entries.push({ kind: 'temperature', name: 'null-chan', value: null, critical: null, max: null });
  env.setRespond(async () => nulled);
  await env.api.pollDevice();
  const nullEntry = env.state.device._fmt.sensors[0].entries.at(-1);
  assert.equal(nullEntry.valueText, '—');
  assert.equal(nullEntry.level, 'ok');

  // critical/max 与 valueText 同口径取整后进抑制比较：视图按整数渲染阈值，
  // 12.4→12.6 这类被舍入抹平的抖动不得击穿显示等价抑制（round-5 review P3）。
  const frac = fixtureSnapshot();
  frac.sensors[0].entries.push({ kind: 'temperature', name: 'frac-crit', value: 50, critical: 84.4, max: 74.6 });
  env.setRespond(async () => frac);
  await env.api.pollDevice();
  const fracEntry = env.state.device._fmt.sensors[0].entries.at(-1);
  assert.equal(fracEntry.critical, 84);
  assert.equal(fracEntry.max, 75);
}

// ── 错误轮与 in-flight 守卫 ───────────────────────────────────────
{
  const env = loadDeviceFeature();
  // 先落一拍成功快照再进错误轮：恢复轮的强制 notify 才真正走到 hadDeviceError
  // 臂——首轮 !prevFmt 本来就必 notify，旧顺序（错误轮先行）对"删掉强制臂"的
  // 突变照样放行（第三轮评审 P2）。
  env.setRespond(async () => fixtureSnapshot());
  await env.api.pollDevice();
  assert.equal(env.state.deviceError, null);
  assert.ok(env.state.device._fmt);
  assert.equal(env.notifyCount(), 1);

  env.setRespond(async command => {
    if (command === 'get_device_snapshot') throw new Error('snapshot backend down');
    return null;
  });
  await env.api.pollDevice();
  assert.equal(env.state.deviceError, 'snapshot backend down');
  assert.equal(env.notifyCount(), 2);

  // 同文案失败拍不重复发布（第七轮评审 P3）：后台 2s 轮询在故障期间每拍重入
  // 这里，无条件 notify 会让设备页每 2s 全量重渲染一块不变的横幅。
  await env.api.pollDevice();
  assert.equal(env.state.deviceError, 'snapshot backend down');
  assert.equal(env.notifyCount(), 2, 'identical error round must not re-notify');
  // 错误文案变化仍要发布（横幅内容切换）。
  env.setRespond(async () => { throw new Error('snapshot backend moved'); });
  await env.api.pollDevice();
  assert.equal(env.state.deviceError, 'snapshot backend moved');
  assert.equal(env.notifyCount(), 3);

  // 恢复轮必须强制 notify 一次（横幅切回正常面板），即便显示等价。
  env.setRespond(async () => fixtureSnapshot());
  await env.api.pollDevice();
  assert.equal(env.state.deviceError, null);
  assert.ok(env.state.device._fmt);
  assert.equal(env.notifyCount(), 4, 'recovery round must force a notify even when display-equal');

  // in-flight 守卫：未决轮次期间的重入 pollDevice 直接丢弃。
  let release;
  env.setRespond(() => new Promise(resolve => { release = resolve; }));
  const pending = env.api.pollDevice();
  await env.api.pollDevice();
  release(fixtureSnapshot());
  await pending;
  const snapshotInvokes = env.invokes.filter(([command]) => command === 'get_device_snapshot');
  // 成功轮 + 错误轮 + 同文案轮 + 换文案轮 + 恢复轮 + 本组未决轮 = 6;重入 tick 被吞掉不新增。
  assert.equal(snapshotInvokes.length, 6, 'in-flight poll must swallow reentrant ticks');
}

// ── 服务探测持续失败：连续 3 拍立旗（第六轮 P2 修复的回归钉，第七轮补）──
{
  const env = loadDeviceFeature();
  // 先落一拍成功基线：失败路径才有"旧 chips 保留"的意义。
  env.setRespond(async command => {
    if (command === 'get_device_services') {
      return [{ key: 'ollama', running: true, detail: null }];
    }
    return null;
  });
  await env.api.pollDeviceServices();
  const baselineNotifies = env.notifyCount();

  env.setRespond(async command => {
    if (command === 'get_device_services') throw new Error('probe timeout');
    return null;
  });
  // 第 1、2 拍：有基线且未达阈值 → 静默，旧 chips 原样保留。
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, null);
  assert.equal(env.state.deviceServices.length, 1, 'stale chips must stay visible');
  assert.equal(env.notifyCount(), baselineNotifies);
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, null);
  assert.equal(env.notifyCount(), baselineNotifies);
  // 第 3 拍：立旗一次并发布。
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, 'probe timeout');
  assert.equal(env.notifyCount(), baselineNotifies + 1);
  // 旗已立：同文案的后续失败拍不重复 notify。
  await env.api.pollDeviceServices();
  assert.equal(env.notifyCount(), baselineNotifies + 1);
  // 恢复拍：错误旗→null 翻清必须发布，即便内容与基线完全一致。
  env.setRespond(async command => {
    if (command === 'get_device_services') {
      return [{ key: 'ollama', running: true, detail: null }];
    }
    return null;
  });
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, null);
  assert.equal(env.notifyCount(), baselineNotifies + 2, 'recovery must publish even when content unchanged');
}

// ── sensorChipKey：风扇臂只在整组 fan 时生效（第七轮评审 P3）────────
{
  const env = loadDeviceFeature();
  env.setRespond(async () => ({
    ...fixtureSnapshot(),
    sensors: [
      // 混装芯片（风扇+温度）：后端组内按 kind 字母序 fan 恒排首位——旧口径
      // 拿首条 kind 判断会把整张卡（含温度读数）标成"风扇"。
      { chip: 'nct6798', entries: [
        { kind: 'fan', name: 'fan1', value: 900, critical: null, max: null },
        { kind: 'temperature', name: 'SYSTIN', value: 42, critical: 90, max: null },
      ] },
      // 纯风扇组且 chip 名无提示：kind 臂仍要能命中。
      { chip: 'it8620', entries: [
        { kind: 'fan', name: 'fan2', value: 1200, critical: null, max: null },
      ] },
    ],
  }));
  await env.api.pollDevice();
  const [mixed, fanOnly] = env.state.device._fmt.sensors;
  assert.equal(mixed.chipKey, 'nct6798', 'mixed-kind chip must keep its raw chip key');
  assert.equal(fanOnly.chipKey, 'acpi_fan', 'fan-only chip must map to the fan key');
}

// ── 轮询生命周期 ─────────────────────────────────────────────────
{
  const env = loadDeviceFeature();
  env.setRespond(async () => fixtureSnapshot());
  const before = env.invokes.length;
  env.api.startDevicePolling();
  assert.equal(env.interval().started, 1);
  assert.equal(env.interval().ms, 2000, 'device polling must run on the 2s cadence');
  await flush();
  assert.equal(env.invokes.length, before + 1, 'startDevicePolling must poll immediately');

  const duringStart = env.interval().started;
  env.api.startDevicePolling();
  assert.equal(env.interval().started, duringStart, 'double start must not stack intervals');
  assert.equal(env.invokes.length, before + 1);

  env.api.stopDevicePolling();
  assert.equal(env.interval().cleared, 1);
  env.api.stopDevicePolling();
  assert.equal(env.interval().cleared, 1, 'double stop must stay a no-op');
  env.api.startDevicePolling();
  await flush();
  assert.equal(env.invokes.length, before + 2, 'restart must resume immediate polling');
  env.api.stopDevicePolling();
}

// ── 服务探测 / 外部模型 / 故障排查提示词 ─────────────────────────
{
  const env = loadDeviceFeature();
  env.setRespond(async command => {
    if (command === 'get_device_services') {
      return [{ key: 'ollama', running: true, detail: null }, { key: 'docker', running: false, detail: 'command not found' }];
    }
    return null;
  });
  await env.api.pollDeviceServices();
  assert.deepEqual(env.state.deviceServices, [
    { key: 'ollama', running: true, detail: null },
    { key: 'docker', running: false, detail: 'command not found' },
  ]);
  assert.equal(env.notifyCount(), 1);

  // 内容完全没变 → 不重复下发（与快照的显示等价抑制同一目标）。
  await env.api.pollDeviceServices();
  assert.equal(env.notifyCount(), 1);

  // 键序翻转也不得击穿抑制：structuralEqual 取代 JSON.stringify（第四轮 P3，
  // map 形字段换序列化序时旧口径会白白重渲染）。
  env.setRespond(async () => [
    { detail: null, running: true, key: 'ollama' },
    { detail: 'command not found', running: false, key: 'docker' },
  ]);
  await env.api.pollDeviceServices();
  assert.equal(env.notifyCount(), 1, 'key-order flip must stay suppressed');

  // 探测失败保留上一份结果：瞬时抖动不渲染成「全部停止」；且失败轮零变化不 notify。
  env.setRespond(async command => {
    if (command === 'get_device_services') throw new Error('probe timeout');
    return null;
  });
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServices.length, 2, 'failed probe must keep the last known services');
  // 已有结果的失败轮：保留旧值且不立错误旗（瞬时抖动 ≠ 探测已坏）。
  assert.equal(env.state.deviceServicesError, null);
  assert.equal(env.notifyCount(), 1);

  // in-flight 守卫：未决轮次期间的重入直接丢弃（10s 定时 + 手动刷新可并发，
  // docker 探测最坏 3s+，乱序回填会把旧探测结果倒灌回来）。
  let releaseServices;
  env.setRespond(command => command === 'get_device_services'
    ? new Promise(resolve => { releaseServices = resolve; })
    : null);
  const pendingServices = env.api.pollDeviceServices();
  await env.api.pollDeviceServices();
  releaseServices([
    { key: 'ollama', running: true, detail: null },
    { key: 'docker', running: false, detail: 'command not found' },
  ]);
  await pendingServices;
  const serviceInvokes = env.invokes.filter(([command]) => command === 'get_device_services');
  // 成功轮 + 内容等价轮 + 键序等价轮 + 失败轮 + 未决轮 = 5；重入 tick 被吞掉不新增。
  assert.equal(serviceInvokes.length, 5, 'in-flight services poll must swallow reentrant ticks');
  // 决算结果与保留值内容相同 → 不 notify。
  assert.equal(env.notifyCount(), 1);
}

// ── 服务探测首拍：空结果与失败必须可见 ──────────────────────────
{
  const env = loadDeviceFeature();
  // 首拍即空数组也必须落地：抑制基线若是 "null || []"，首个空结果会被吞掉、
  // 视图的 loading 永远不退（第三轮评审 P2）。
  env.setRespond(async command => (command === 'get_device_services' ? [] : null));
  await env.api.pollDeviceServices();
  assert.deepEqual(env.state.deviceServices, [], 'first empty probe must land (no eternal loading)');
  assert.equal(env.notifyCount(), 1);
  // 已有结果（含空数组）后的失败轮保持静默：瞬时抖动 ≠ 探测已坏。
  env.setRespond(async command => {
    if (command === 'get_device_services') throw new Error('later probe down');
    return null;
  });
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, null);
  assert.equal(env.notifyCount(), 1);
}

// ── 服务探测首拍即失败：deviceServicesError 必须可见 ────────────
{
  const env = loadDeviceFeature();
  env.setRespond(async command => {
    if (command === 'get_device_services') throw new Error('first probe down');
    return null;
  });
  await env.api.pollDeviceServices();
  // null 悬着只会让视图的"探测中"无限驻留；首拍失败必须立旗。
  assert.equal(env.state.deviceServices, null);
  assert.equal(env.state.deviceServicesError, 'first probe down');
  assert.equal(env.notifyCount(), 1);

  // 恢复轮清掉错误旗并落地结果。
  env.setRespond(async command => (command === 'get_device_services' ? [] : null));
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, null);
  assert.deepEqual(env.state.deviceServices, []);
  assert.equal(env.notifyCount(), 2);
}

{
  const env = loadDeviceFeature();
  const modelsView = {
    models: [{ id: 'm-1', name: 'qwen3-30b', kind: 'ollama', registered: true, source: 'registry' }],
    warnings: [],
  };
  env.setRespond(async command => command === 'get_external_models' ? modelsView : null);
  const fetched = await env.api.fetchExternalModels();
  assert.equal(fetched, modelsView);
  assert.equal(env.state.externalModels, modelsView);
  assert.equal(env.notifyCount(), 1);

  // 注册成功后必须回读注册表刷新 externalModels。
  const record = { id: 'm-9', name: 'qwen3-30b' };
  env.setRespond(async (command, args) => {
    if (command === 'device_register_external_model') return record;
    if (command === 'get_external_models') return modelsView;
    return null;
  });
  const registered = await env.api.registerExternalModel({ name: 'qwen3-30b', kind: 'ollama' });
  assert.equal(registered, record);
  const registerInvokes = env.invokes.filter(([command]) => command.startsWith('device_register'));
  assert.deepEqual(registerInvokes, [['device_register_external_model', { name: 'qwen3-30b', kind: 'ollama' }]]);
  assert.ok(env.invokes.some(([command]) => command === 'get_external_models'), 'register must refetch external models');

  // 移除同理：透传守卫参数 + 回读注册表（回读此前只在注释里声称，断言缺席——
  // 删掉回读调用全部测试照绿，UI 会停留在已移除的旧行上；第四轮评审补钉）。
  const outcome = { removed_registry: true, removed_weights: true, ollama_deleted: false, warnings: [] };
  env.setRespond(async command => command === 'device_remove_external_model' ? outcome : modelsView);
  const refetchesBeforeRemove = env.invokes.filter(([command]) => command === 'get_external_models').length;
  const removed = await env.api.removeExternalModel({ id: 'm-9', deleteWeights: true, confirmName: 'qwen3-30b' });
  assert.equal(removed, outcome);
  assert.deepEqual(
    env.invokes.filter(([command]) => command === 'device_remove_external_model'),
    [['device_remove_external_model', { id: 'm-9', deleteWeights: true, confirmName: 'qwen3-30b' }]],
  );
  assert.equal(
    env.invokes.filter(([command]) => command === 'get_external_models').length,
    refetchesBeforeRemove + 1,
    'remove must refetch the external models registry',
  );

  // 故障排查提示词：kind/question 透传后端组装，返回拼好的文本；缺省问题置 null。
  env.setRespond(async (command, args) => command === 'device_doctor_prompt' ? `提示词:${args.kind}` : null);
  const prompt = await env.api.buildDoctorPrompt('custom', '  风扇为什么一直响  ');
  assert.equal(prompt, '提示词:custom');
  // args 对象在 vm 域里创建,原型与本域不同:逐字段比较而不是 deepEqual。
  const customInvoke = env.invokes.filter(([command]) => command === 'device_doctor_prompt').at(-1);
  assert.equal(customInvoke[0], 'device_doctor_prompt');
  assert.equal(customInvoke[1].kind, 'custom');
  assert.equal(customInvoke[1].question, '  风扇为什么一直响  ');
  await env.api.buildDoctorPrompt('packages');
  const packagesInvoke = env.invokes.filter(([command]) => command === 'device_doctor_prompt').at(-1);
  assert.equal(packagesInvoke[0], 'device_doctor_prompt');
  assert.equal(packagesInvoke[1].kind, 'packages');
  assert.equal(packagesInvoke[1].question, null);
}

// ── 外部模型回读 epoch：后发先至，迟到的旧回读不得倒灌（round-9 P1）──
{
  const env = loadDeviceFeature();
  const staleList = { models: [{ id: 'm-old', name: 'stale-model', kind: 'ollama', registered: true, source: 'registry' }], warnings: [] };
  const freshList = { models: [{ id: 'm-new', name: 'fresh-model', kind: 'ollama', registered: true, source: 'registry' }], warnings: [] };
  let releasePre;
  let fetchCount = 0;
  env.setRespond(command => {
    if (command !== 'get_external_models') return null;
    fetchCount += 1;
    if (fetchCount === 1) return new Promise(resolve => { releasePre = () => resolve(staleList); });
    return freshList;
  });
  // fetch A（先发，挂起）→ fetch B（后发，立即带回新列表）：B 必须先落地。
  const preFetch = env.api.fetchExternalModels();
  await env.api.fetchExternalModels();
  assert.equal(env.state.externalModels, freshList, 'the later-started fetch must win');
  assert.equal(env.notifyCount(), 1, 'only the winning fetch may publish');
  // A 晚到（携带操作前的旧列表）：整包丢弃——不改 state、不 notify，
  // 但仍返回当前 state.externalModels（调用方兼容）。
  releasePre();
  const staleReturn = await preFetch;
  assert.equal(env.state.externalModels, freshList, 'stale fetch must not overwrite the fresh registry');
  assert.equal(staleReturn, freshList, 'stale fetch must return current state for caller compatibility');
  assert.equal(env.notifyCount(), 1, 'stale fetch must not publish');
}

// ── remove 内部回读与操作前回读的竞态：被删行不得复活（round-9 P1）──
{
  const env = loadDeviceFeature();
  const preRemoveList = { models: [{ id: 'm-9', name: 'qwen3-30b', kind: 'ollama', registered: true, source: 'registry' }], warnings: [] };
  const postRemoveList = { models: [], warnings: [] };
  const outcome = { removed_registry: true, removed_weights: true, ollama_deleted: false, warnings: [] };
  let releasePreRemove;
  let fetchCount = 0;
  env.setRespond(command => {
    if (command === 'device_remove_external_model') return outcome;
    if (command !== 'get_external_models') return null;
    fetchCount += 1;
    if (fetchCount === 1) return new Promise(resolve => { releasePreRemove = () => resolve(preRemoveList); });
    return postRemoveList;
  });
  // 操作前发起的回读先挂起；remove 的内部回读在操作 invoke 决算后发起，
  // seq 必然更高 → 立即落地移除后列表；晚到的操作前列表必须被丢弃。
  const preRemoveFetch = env.api.fetchExternalModels();
  const removed = await env.api.removeExternalModel({ id: 'm-9', deleteWeights: true, confirmName: 'qwen3-30b' });
  assert.equal(removed, outcome);
  assert.equal(env.state.externalModels, postRemoveList, 'remove refetch must publish the post-remove registry');
  releasePreRemove();
  assert.equal(await preRemoveFetch, postRemoveList, 'stale pre-remove fetch must be discarded');
  assert.equal(env.state.externalModels, postRemoveList, 'removed row must not resurrect');
  assert.equal(env.notifyCount(), 1, 'only the post-remove refetch may publish');
}

// ── 快照轮询世代：startDevicePolling 重启作废在飞旧拍（round-9 P2）──
{
  const env = loadDeviceFeature();
  const deferred = [];
  env.setRespond(command => command === 'get_device_snapshot'
    ? new Promise((resolve, reject) => { deferred.push({ resolve, reject }); })
    : null);
  env.api.startDevicePolling();               // P1 未决（立即拍 + 定时器）
  assert.equal(env.interval().started, 1);
  env.api.stopDevicePolling();
  env.api.startDevicePolling();               // 重启：复位卡死旗 + bump 世代 → P2
  assert.equal(env.interval().started, 2, 'restart must re-establish the interval');
  // 旧拍带着快照晚到：不得发布、不得 notify。
  deferred[0].resolve(fixtureSnapshot());
  await flush();
  assert.equal(env.state.device, null, 'stale poll must not publish its snapshot');
  assert.equal(env.state.deviceError, null, 'stale poll must not touch the banner state');
  assert.equal(env.notifyCount(), 0, 'stale poll must not notify');
  // 旧拍的 finally 不得清 in-flight 旗：新拍未决期间的重入 tick 必须被吞。
  const invokesBeforeTick = env.invokes.length;
  await env.interval().fn();
  assert.equal(env.invokes.length, invokesBeforeTick, 'in-flight flag must stay owned by the fresh poll');
  // 新拍正常发布。
  deferred[1].resolve(fixtureSnapshot());
  await flush();
  assert.ok(env.state.device && env.state.device._fmt, 'fresh poll must publish');
  assert.equal(env.notifyCount(), 1);
  // 错误路径同理：慢旧拍的错误晚到，不得在恢复后重新点亮横幅。
  // 在拍未决：fire-and-forget，await 会挂住测试（轮询 promise 要到决算才 settle）。
  void env.interval().fn();                   // P3 未决
  env.api.stopDevicePolling();
  env.api.startDevicePolling();               // P4；bump → P3 作废
  deferred[2].reject(new Error('stale poll error'));
  await flush();
  assert.equal(env.state.deviceError, null, 'stale error must not re-light the banner');
  assert.equal(env.notifyCount(), 1, 'stale error must not notify');
  deferred[3].resolve(fixtureSnapshot());
  await flush();
  assert.equal(env.state.deviceError, null);
  assert.equal(env.notifyCount(), 1, 'display-equal fresh round stays suppressed');
  env.api.stopDevicePolling();
}

// ── services 探测世代：stale 失败不推进连续失败计数、不清在飞旗（round-9 P3）──
{
  const env = loadDeviceFeature();
  const baseline = [{ key: 'ollama', running: true, detail: null }];
  let servicesCall = 0;
  let rejectHeld;
  let rejectFresh;
  env.setRespond(command => {
    if (command === 'get_device_snapshot') return fixtureSnapshot();
    if (command !== 'get_device_services') return null;
    servicesCall += 1;
    if (servicesCall === 2) return new Promise((resolve, reject) => { rejectHeld = reject; });
    if (servicesCall === 3) return new Promise((resolve, reject) => { rejectFresh = reject; });
    if (servicesCall === 1 || servicesCall >= 6) return baseline;
    throw new Error('probe timeout');
  });
  await env.api.pollDeviceServices();          // call1：基线成功
  const held = env.api.pollDeviceServices();   // call2：未决（将被作废的旧世代）
  env.api.startDevicePolling();                // bump services 世代 + 复位卡死旗
  const fresh = env.api.pollDeviceServices();  // call3：新世代，未决
  await flush();                               // 等重启立即拍的快照发布落定，notify 基线此后只含 services 事件
  const notifiesAfterRestart = env.notifyCount();
  // 旧世代的失败晚到：静默吞掉——不立旗、不动基线。
  rejectHeld(new Error('late probe boom'));
  await held;
  assert.equal(env.state.deviceServicesError, null, 'stale failure must not raise the flag');
  assert.equal(env.state.deviceServices.length, 1, 'stale failure must keep the baseline visible');
  assert.equal(env.notifyCount(), notifiesAfterRestart, 'stale failure must not publish');
  // stale 的 finally 不得替新世代清旗：fresh 未决期间重入必须被吞。
  const servicesInvokesBefore = env.invokes.filter(([command]) => command === 'get_device_services').length;
  await env.api.pollDeviceServices();
  assert.equal(
    env.invokes.filter(([command]) => command === 'get_device_services').length,
    servicesInvokesBefore,
    'stale finally must not clear the in-flight flag owned by the fresh generation',
  );
  // 新世代失败 #1：有基线、未达阈值 → 静默。stale 失败不得推进计数——否则
  // 这里已是第 2 拍、下一拍即提前立旗（3 拍节奏被破坏）。
  rejectFresh(new Error('probe timeout'));
  await fresh;
  assert.equal(env.state.deviceServicesError, null);
  await env.api.pollDeviceServices();          // 失败 #2 → 仍静默
  assert.equal(env.state.deviceServicesError, null);
  assert.equal(env.notifyCount(), notifiesAfterRestart);
  await env.api.pollDeviceServices();          // 失败 #3 → 立旗一次并发布
  assert.equal(env.state.deviceServicesError, 'probe timeout');
  assert.equal(env.notifyCount(), notifiesAfterRestart + 1);
  // 恢复拍：旗翻清必须发布，即便内容与基线一致。
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServicesError, null);
  assert.deepEqual(env.state.deviceServices, baseline);
  assert.equal(env.notifyCount(), notifiesAfterRestart + 2, 'recovery must publish even when content unchanged');
  env.api.stopDevicePolling();
}

console.log('device bridge _fmt formatting, display-equality suppression and polling contract passed');

// ── 温度档位：历史峰值只用于展示，升/降温序列不翻转（十轮评审 P2）──
{
  const env = loadDeviceFeature();
  const sensors = (value, max) => [
    {
      chip: 'coretemp',
      entries: [{ kind: 'temperature', name: 'Package', value, critical: 100, max }],
    },
  ];
  const levelAt = async (value, max) => {
    env.state.device = null;
    env.setRespond(async command =>
      command === 'get_device_snapshot'
        ? { ...fixtureSnapshot(), sensors: sensors(value, max) }
        : null
    );
    await env.api.pollDevice();
    return env.state.device._fmt.sensors[0].entries[0].level;
  };
  // 升温：45°C（峰值 46，critical 100）→ ok，不得因贴近峰值误报 warn；
  // 46°C：后端对相等峰值省略 max，档位不得反而变化；95°C → warn；100°C → crit。
  assert.equal(await levelAt(45, 46), 'ok');
  assert.equal(await levelAt(46, null), 'ok');
  assert.equal(await levelAt(95, 96), 'warn');
  assert.equal(await levelAt(100, 100), 'crit');
  // 降温：峰值仍挂着 46，回落到 45 依旧是 ok（无 critical 的芯片不伪造档位）。
  assert.equal(await levelAt(45, 46), 'ok');
  // 后端没给 critical 时温度只有 ok 一档——没有真阈值就不造预警。
  const noCrit = await (async () => {
    env.state.device = null;
    env.setRespond(async command =>
      command === 'get_device_snapshot'
        ? {
            ...fixtureSnapshot(),
            sensors: [
              { chip: 'coretemp', entries: [{ kind: 'temperature', name: 'Package', value: 99, critical: null, max: 100 }] },
            ],
          }
        : null
    );
    await env.api.pollDevice();
    return env.state.device._fmt.sensors[0].entries[0].level;
  })();
  assert.equal(noCrit, 'ok');
}
