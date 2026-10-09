// MegaCube 设备管理 bridge（src/platform/tauri/bridge/device.js）格式化与轮询契约。
// 仿 monitor_bridge_format.test.mjs 的定位：锁 _fmt 的显示口径（GiB/MiB、
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
  const state = { device: null, deviceError: null, deviceServices: null, externalModels: null };
  let notifyCount = 0;
  const invokes = [];
  let respond = async () => null;
  const invoke = async (command, args) => {
    invokes.push([command, args]);
    return respond(command, args);
  };
  const factory = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__.device;
  assert.equal(typeof factory, 'function', 'device.js must register a bridge feature factory');
  const api = factory({ state, notify() { notifyCount += 1; }, invoke });
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
      { kind: 'temperature', name: 'wifi_max_warn', value: 72, critical: 90, max: 80 },
    ] },
    { chip: 'INA3221', entries: [
      { kind: 'voltage', name: 'in0', value: 12.06, critical: null, max: null },
      { kind: 'current', name: 'curr1', value: 3.2, critical: null, max: null },
      { kind: 'power', name: 'power1', value: 28.46, critical: null, max: null },
    ] },
  ],
  disks: [
    { name: 'nvme0n1p2', mount_point: '/', fs_type: 'ext4', kind: 'ssd', total_bytes: 1610612736, available_bytes: 805306368, is_removable: false },
    { name: 'sda1', mount_point: '/media/usb', fs_type: 'vfat', kind: 'hdd', total_bytes: 3221225472, available_bytes: 3221225472, is_removable: true },
    { name: null, mount_point: '/snap/core', fs_type: 'squashfs', kind: 'unknown', total_bytes: null, available_bytes: null, is_removable: false },
  ],
  network: [
    { name: 'lo', iface_type: 'loopback', is_up: true, mac: null, rx_bytes: 1610612736, tx_bytes: 1610612736, rx_rate_bps: 2500000, tx_rate_bps: 0 },
    { name: 'wlP1s0', iface_type: 'wifi', is_up: true, mac: 'aa:bb:cc:dd:ee:ff', rx_bytes: 0, tx_bytes: 1048576, rx_rate_bps: 1500000000, tx_rate_bps: -5 },
    { name: 'docker0', iface_type: 'weird', is_up: false, mac: '00:11:22:33:44:55', rx_bytes: null, tx_bytes: null, rx_rate_bps: null, tx_rate_bps: null },
  ],
  processes: {
    top_cpu: [{ pid: 1234, name: 'ollama serve', cpu_pct: 91.6, memory_bytes: 1610612736 }],
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

  // 字节：1610612736 → '1.5 GiB'；768 MiB 以下保持 MiB；非法值回退 —。
  assert.equal(fmt.disks[0].totalText, '1.5 GiB');
  assert.equal(fmt.disks[0].availableText, '768.0 MiB');
  assert.equal(fmt.disks[1].totalText, '3.0 GiB');
  assert.equal(fmt.disks[1].usedPct, 0);
  assert.equal(fmt.disks[2].totalText, '—');
  assert.equal(fmt.disks[2].usedPct, 0);
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
  assert.equal(wifi.chipKey, 'mt7925');
  assert.equal(wifi.entries[0].valueText, '52°C');
  // 79.4 四舍五入后是 79，但告警档位必须用原始值判定（≥ max-10 前 52.3 未达 70）。
  assert.equal(wifi.entries[0].level, 'ok');
  // ≥ max-10 → warn（max 口径）。
  assert.equal(wifi.entries[1].level, 'warn');
  // 未知 chip 保留小写原文做 key，交给视图兜底。
  assert.equal(power.chipKey, 'ina3221');
  assert.equal(power.entries[0].valueText, '12.1 V');
  assert.equal(power.entries[1].valueText, '3.2 A');
  assert.equal(power.entries[2].valueText, '28.5 W');

  // 网络：typeKey 白名单校验、累计与速率文本；非法/负速率按 0。
  assert.equal(fmt.network[0].typeKey, 'loopback');
  assert.equal(fmt.network[0].rxTotalText, '1.5 GiB');
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
  assert.equal(fmt.processes.topCpu[0].pid, 1234);
  assert.equal(fmt.processes.topCpu[0].cpuPctText, '92%');
  assert.equal(fmt.processes.topCpu[0].memText, '1.5 GiB');
  assert.equal(fmt.processes.topMemory[0].name.length, 41);
  assert.ok(fmt.processes.topMemory[0].name.endsWith('…'));

  // GPU 扩展：驱动/CUDA 透传，显存占用走同一字节口径；无 GPU → null。
  assert.equal(fmt.gpuExt.driverVersion, '580.02');
  assert.equal(fmt.gpuExt.cudaVersion, '13.0');
  assert.equal(fmt.gpuExt.computeProcesses[0].usedText, '1.5 GiB');

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
// 中文注释合法（与 monitor.js 一致），故剥离注释后再扫。
const stripComments = text => text
  .replace(/\/\*[\s\S]*?\*\//g, comment => comment.replace(/[^\n]/g, ''))
  .split('\n')
  .map(line => line.replace(/\/\/.*$/, ''))
  .join('\n');
assert.doesNotMatch(
  stripComments(source),
  /[\u{4e00}-\u{9fff}]/u,
  'device bridge must stay free of Chinese literals',
);

// ── 显示等价抑制（deviceFmtEqual） ────────────────────────────────
{
  const env = loadDeviceFeature();
  const eq = env.api.deviceFmtEqual;
  const base = { a: 'x', usedPct: 50, nested: { v: 1 }, list: [{ k: 'a', n: 1 }] };
  assert.equal(eq(base, { ...base }), true);
  // 数值容差 0.5：抖动不触发；超过容差不等于。
  assert.equal(eq(base, { ...base, usedPct: 50.4 }), true);
  assert.equal(eq(base, { ...base, usedPct: 51 }), false);
  // updatedAt 是时钟字段：不因前进而打破抑制。
  assert.equal(eq(base, { ...base, updatedAt: base.updatedAt === undefined ? 2 : 1 }), true);
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
  assert.equal(env.state.device._fmt.disks[0].availableText, '764.0 MiB');
  assert.equal(env.notifyCount(), notifiesAfterFirst + 1);
}

// ── 错误轮与 in-flight 守卫 ───────────────────────────────────────
{
  const env = loadDeviceFeature();
  env.setRespond(async command => {
    if (command === 'get_device_snapshot') throw new Error('snapshot backend down');
    return null;
  });
  await env.api.pollDevice();
  assert.equal(env.state.device, null, 'failed first round must not fabricate a snapshot');
  assert.equal(env.state.deviceError, 'snapshot backend down');
  assert.equal(env.notifyCount(), 1);

  // 恢复轮必须强制 notify 一次（横幅切回正常面板），即便显示等价。
  env.setRespond(async () => fixtureSnapshot());
  await env.api.pollDevice();
  assert.equal(env.state.deviceError, null);
  assert.ok(env.state.device._fmt);
  assert.equal(env.notifyCount(), 2);

  // in-flight 守卫：未决轮次期间的重入 pollDevice 直接丢弃。
  let release;
  env.setRespond(() => new Promise(resolve => { release = resolve; }));
  const pending = env.api.pollDevice();
  await env.api.pollDevice();
  release(fixtureSnapshot());
  await pending;
  const snapshotInvokes = env.invokes.filter(([command]) => command === 'get_device_snapshot');
  // 本 env 已有错误轮 + 恢复轮两次,加上本组未决轮一次 = 3;重入 tick 被吞掉不新增。
  assert.equal(snapshotInvokes.length, 3, 'in-flight poll must swallow reentrant ticks');
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

  // 探测失败保留上一份结果：瞬时抖动不渲染成「全部停止」。
  env.setRespond(async command => {
    if (command === 'get_device_services') throw new Error('probe timeout');
    return null;
  });
  await env.api.pollDeviceServices();
  assert.equal(env.state.deviceServices.length, 2, 'failed probe must keep the last known services');
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

  // 移除同理：透传守卫参数 + 回读注册表。
  const outcome = { removed_registry: true, removed_weights: true, ollama_deleted: false, warnings: [] };
  env.setRespond(async command => command === 'device_remove_external_model' ? outcome : modelsView);
  const removed = await env.api.removeExternalModel({ id: 'm-9', deleteWeights: true, confirmName: 'qwen3-30b' });
  assert.equal(removed, outcome);
  assert.deepEqual(
    env.invokes.filter(([command]) => command === 'device_remove_external_model'),
    [['device_remove_external_model', { id: 'm-9', deleteWeights: true, confirmName: 'qwen3-30b' }]],
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

console.log('device bridge _fmt formatting, display-equality suppression and polling contract passed');
