#!/usr/bin/env node
/**
 * MegaCube 设备管理页 UI smoke：加载真实 Vite dist，mock TauriBridge，点击真实 React UI。
 * 平台桩：get_platform_capabilities 返回 megacubeDevice:true + showMegacubeSite:false，
 * 侧栏原「运行状态」入口换成「设备管理」。覆盖：
 * - 设备管理页渲染出全部区块（概览/传感器/存储/网络/进程/模型管理/故障排查）
 * - get_device_snapshot fixture 驱动磁盘行与传感器 chip 真实渲染（含系统盘徽标）
 * - 模型商店行呈锁定态（由模型商店管理 + 禁用），外部模型行显示类别/状态徽标
 * - 外部模型「移除」的两段式确认：先登记（ollama 未登记行）再走守卫移除命令
 * - 故障排查快捷卡：device_doctor_prompt → 新建普通会话（create_session）→ chat 携带提示词
 * - 商店库/外部模型读取失败面：常驻错误行 + 前往商店/重试恢复（第五轮评审）
 * 依赖:puppeteer-core + 系统 chromium（或 env CHROME 指定）。缺依赖 → exit 2（SKIP 行）。
 */
const fs = require('fs');
const os = require('os');
const path = require('path');
const assert = require('assert');
const { startUiTestServer } = require('./ui_test_server');

function loadPuppeteer() {
  try { return require('puppeteer-core'); } catch (_) { /* fall through */ }
  const npx = path.join(os.homedir(), '.npm', '_npx');
  if (fs.existsSync(npx)) {
    for (const directory of fs.readdirSync(npx)) {
      const candidate = path.join(npx, directory, 'node_modules', 'puppeteer-core');
      if (fs.existsSync(candidate)) {
        try { return require(candidate); } catch (_) { /* try next */ }
      }
    }
  }
  console.error('SKIP: 找不到 puppeteer-core');
  process.exit(2);
}

const puppeteer = loadPuppeteer();
const CHROME = process.env.CHROME || [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/snap/bin/chromium',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
].find(candidate => fs.existsSync(candidate));
if (!CHROME) {
  console.error('SKIP: 未找到 chromium/chrome，可用 env CHROME=/path/to/chrome 指定');
  process.exit(2);
}
const PROFILE = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-device-mgr-'));

// 设备快照 fixture：与 tests/device_bridge_format.test.mjs 的 fixtureSnapshot 同一
// 形状（bridge/device.js 的 _fmt 契约已在那边锁定，这里只负责驱动真实渲染）。
function deviceSnapshotFixture() {
  return {
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
    ],
    disks: [
      { name: 'nvme0n1p2', mount_point: '/', fs_type: 'ext4', kind: 'ssd', total_bytes: 322122547200, available_bytes: 161061273600, is_removable: false },
      { name: 'sda1', mount_point: '/media/usb', fs_type: 'vfat', kind: 'hdd', total_bytes: 32212254720, available_bytes: 32212254720, is_removable: true },
    ],
    network: [
      { name: 'lo', iface_type: 'loopback', is_up: true, mac: null, rx_bytes: 1610612736, tx_bytes: 1610612736, rx_bytes_per_sec: 2500000, tx_bytes_per_sec: 0 },
      { name: 'wlP1s0', iface_type: 'wifi', is_up: true, mac: 'aa:bb:cc:dd:ee:ff', rx_bytes: 104857600, tx_bytes: 1048576, rx_bytes_per_sec: 15000000, tx_bytes_per_sec: 500000 },
    ],
    processes: {
      top_cpu: [{ pid: 1234, name: 'ollama serve', cpu_pct: 91.6, memory_bytes: 1610612736 }],
      top_memory: [{ pid: 99, name: 'python3 trainer.py', cpu_pct: 0.2, memory_bytes: 1073741824 }],
    },
    gpu_ext: {
      driver_version: '580.02',
      cuda_version: '13.0',
      compute_processes: [{ pid: 7, name: 'python3', used_mib: 1536 }],
    },
  };
}

// 内嵌运行状态页的内存快照只取用 ram 两个数字：头部「内存」健康 chip 的期望值
// 由这两个常量推导（51/129 ≈ 40%），桩与断言共用同一来源，不发明第二份数字。
const MONITOR_RAM_TOTAL_KIB = 129 * 1024 * 1024;
const MONITOR_RAM_USED_KIB = 51 * 1024 * 1024;

// 从 deviceSnapshotFixture 推导各渲染位的期望文本（与 bridge/device.js 的
// _fmt 同一口径：fmtBytes 1024 进制 GB/MB 一位小数、fmtRate 十进制 KB/s/MB/s、
// 温度取整、进程 CPU% 截 0 取整）。步骤 ④-b 的断言读这些「值」而不只数个数——
// 只数个数曾放过头部最高温 chip 渲染 undefined°C（九轮评审 P1）。
function expectedSnapshotValues(fixture) {
  const fmtBytes = bytes => (bytes >= 1024 * 1024 * 1024)
    ? (bytes / (1024 * 1024 * 1024)).toFixed(1) + ' GB'
    : (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  const fmtRate = v => (v >= 1e6 ? (v / 1e6).toFixed(1) + ' MB/s' : (v / 1e3).toFixed(1) + ' KB/s');
  const temperatures = fixture.sensors
    .flatMap(group => group.entries)
    .filter(entry => entry.kind === 'temperature')
    .map(entry => Math.round(entry.value));
  const root = fixture.disks.find(disk => disk.mount_point === '/');
  const wifi = fixture.network.find(iface => iface.iface_type === 'wifi');
  const topCpu = fixture.processes.top_cpu[0];
  const topMem = fixture.processes.top_memory[0];
  return {
    diskPct: Math.round((root.total_bytes - root.available_bytes) / root.total_bytes * 100) + '%',
    diskAvailable: fmtBytes(root.available_bytes),
    diskTotal: fmtBytes(root.total_bytes),
    ramPct: Math.round(MONITOR_RAM_USED_KIB / MONITOR_RAM_TOTAL_KIB * 100) + '%',
    maxTemp: Math.max(...temperatures) + '°C',
    driver: fixture.gpu_ext.driver_version,
    cuda: fixture.gpu_ext.cuda_version,
    netRxTotal: fmtBytes(wifi.rx_bytes),
    netTxTotal: fmtBytes(wifi.tx_bytes),
    netRxRate: fmtRate(wifi.rx_bytes_per_sec),
    netTxRate: fmtRate(wifi.tx_bytes_per_sec),
    procName: topCpu.name,
    procCpuPct: Math.round(topCpu.cpu_pct) + '%',
    procMemText: fmtBytes(topMem.memory_bytes),
  };
}

// 步骤 ④-b 断言用的期望值：全部从 fixture 推导（见 expectedSnapshotValues）。
const EXPECTED = expectedSnapshotValues(deviceSnapshotFixture());

function externalModelsFixture() {
  return {
    models: [
      // base_url/model 都留 null：Ollama 在线条目真实形态（后端两者恒 null），
      // ⑤-b 的回环回退常量与 name 回退只有在这种行上才真正走到——fixture 原先
      // 填了与回退常量相同的值，回退实现删掉套件也照绿（第七轮评审 P3 base_url，
      // 第八轮评审 model）。
      { id: 'm-ollama-1', name: 'qwen3:30b', kind: 'ollama', base_url: null, model: null, weights_path: null, note: '', status: 'available', registered: false, source: 'ollama' },
      { id: 'm-reg-1', name: 'my-endpoint', kind: 'openai_endpoint', base_url: 'http://127.0.0.1:9999/v1', model: 'my-model', weights_path: '/home/u/models/my-endpoint', note: '', status: 'unreachable', registered: true, source: 'registry' },
      { id: 'm-reg-2', name: 'lan-endpoint', kind: 'openai_endpoint', base_url: 'http://192.168.1.50:8000/v1', model: 'lan-model', weights_path: null, note: '', status: 'available', registered: true, source: 'registry' },
      // 商店 ollama 卡片在 daemon 里的真实形态：商店 model_name 永无冒号、daemon
      // 恒带 `:tag`——这行必须被 managedByStore 的去 tag 臂识别为商店管理并禁用
      // 移除，否则会被当成可移除的外部模型（第八轮评审）。
      { id: 'ollama-yuguang14b:latest', name: 'yuguang14b:latest', kind: 'ollama', base_url: null, model: null, weights_path: null, note: '', status: 'available', registered: false, source: 'ollama' },
    ],
    warnings: [],
  };
}

function injectSource() {
  return `(function () {
    var handlers = Object.create(null);
    var calls = [];
    var settings = {
      theme: 'liquid-light',
      color_scheme: 'light',
      language: 'zh-Hans',
      memory_enabled: true,
      notifications: { enabled: true, task_completed: true },
      pet: { enabled: false },
      search: { provider: 'bing', enabled_providers: ['bing'], api_key: null, credentials: {} },
      advanced: {},
    };
    var models = [
      {
        id: 'cloud-deepseek',
        name: 'DeepSeek',
        preset: 'deepseek',
        model: 'deepseek-v4-flash',
        base_url: 'https://api.deepseek.com',
        provider_kind: 'official_api',
        has_secret: true,
        credential_state: 'configured',
      },
    ];
    var activeModelId = 'cloud-deepseek';
    var removeOutcome = { removed_registry: true, removed_weights: false, ollama_deleted: true, warnings: [] };
    // 平台能力桩收敛为可变量：末尾的非 MegaCube 负控要把 megacubeDevice 翻成
    // false 后整页重载，断言入口与页面回到主线形态（十二轮评审 P2）。覆盖值走
    // sessionStorage——injectSource 每次导航都会重新执行，闭包变量扛不住 reload。
    var platformCaps = {
      os: 'linux',
      megacubeDevice: sessionStorage.getItem('__testMegacubeDevice') !== '0',
      showMegacubeSite: false,
      showSuperPermissionSettings: false,
      usesBundledDependencyInstaller: true,
      taskCompletionNotificationsDefault: true,
      voiceShortcutNative: false,
      browserNativeDisplay: false,
      codexAcpSupported: false,
      localVllmSupported: false,
      teamRelaySupported: true,
    };
    window.__testPlatformCaps = platformCaps;
    window.__testSetMegacubeDevice = function (value) {
      sessionStorage.setItem('__testMegacubeDevice', value ? '1' : '0');
    };
    function record(cmd, args) { calls.push({ cmd: cmd, args: args || null }); }
    function emit(name, payload) {
      return Promise.all((handlers[name] || []).slice().map(function (handler) {
        return handler({ payload: payload || {} });
      }));
    }
    function invoke(cmd, args) {
      record(cmd, args);
      switch (cmd) {
        case 'get_platform_capabilities': return Promise.resolve(Object.assign({}, platformCaps));
        case 'get_settings': return Promise.resolve(settings);
        case 'get_effective_model_config': return Promise.resolve({ model: 'deepseek-v4-flash', base_url: 'https://api.deepseek.com', preset: 'deepseek', api_key_set: true });
        case 'list_models': return Promise.resolve({ models: models.slice(), active_model_id: activeModelId });
        case 'save_model':
          var saved = Object.assign({}, args.model, { has_secret: !!args.model.api_key, credential_state: 'configured' });
          models = models.filter(function (m) { return m.id !== saved.id; }).concat(saved);
          return Promise.resolve(null);
        case 'set_active_model': activeModelId = args.id; return Promise.resolve(null);
        case 'list_sessions': return Promise.resolve([]);
        case 'create_session':
          return Promise.resolve({ id: 's-doctor', title: '新对话', created_at: new Date().toISOString(), updated_at: new Date().toISOString(), message_count: 0 });
        case 'load_session': return Promise.resolve({ metadata: { id: (args && args.id) || 's-doctor' }, messages: [] });
        case 'get_session_persona_events': return Promise.resolve([]);
        case 'get_session_pinvou_reviews': return Promise.resolve([]);
        case 'get_mode_state': return Promise.resolve({ mode: 'yolo', plan_phase: 'none' });
        case 'get_active_persona': return Promise.resolve(null);
        case 'list_personas': return Promise.resolve([]);
        case 'get_backend_status': return Promise.resolve({ online: true, ok: true, status: 'online' });
        case 'get_app_version': return Promise.resolve('0.10.6');
        case 'check_for_update': return Promise.resolve({ available: false, current_version: '0.10.6', latest_version: '0.10.6', notes: '', platform: 'linux' });
        case 'find_resumable_run': return Promise.resolve(null);
        case 'list_scheduled_tasks': return Promise.resolve([]);
        case 'list_scheduled_task_recent_runs': return Promise.resolve([]);
        case 'check_dependencies': return Promise.resolve([]);
        case 'list_workspace_files': return Promise.resolve([]);
        case 'get_behavior_telemetry_status': return Promise.resolve({
          configured: true, enabled: false, user_enabled: null, collection_period_id: 'period_test',
          withdrawal_pending: false, deletion_pending: false, retention_days: 35, pending_event_count: 0, dropped_event_count: 0,
        });
        case 'get_selected_pet': return Promise.resolve('lingling');
        case 'get_memory_overview': return Promise.resolve(null);
        case 'chat':
          if (window.__DEVICE_MGR_TEST__.failChatSend) return Promise.reject(new Error('send down (smoke)'));
          return Promise.resolve(null);
        // 内嵌运行状态页每秒轮询；给一份最小快照，避免错误轮刷屏（内存 chip 也随 bs.monitor._fmt 生效）。
        case 'get_monitor_snapshot': return Promise.resolve({
          cpu: { name: 'NVIDIA GB10', total_usage_pct: 12 },
          ram: { total_kib: ${MONITOR_RAM_TOTAL_KIB}, used_kib: ${MONITOR_RAM_USED_KIB}, swap_total_kib: 0, swap_used_kib: 0 },
          gpu: null, vllm: null, self_perf: null,
          app: { pinvou3_version: '0.10.6', deepseek_tui_version: '0.0.0', session_uptime_secs: 60 },
        });
        // ── MegaCube 设备管理命令 ──
        case 'get_device_snapshot':
          if (window.__DEVICE_MGR_TEST__.failSnapshot) return Promise.reject(new Error('snapshot down (smoke)'));
          var smokeSnapshot = ${JSON.stringify(deviceSnapshotFixture())};
          // ④-c 场景开关：compute_processes null = 查询失败（round-16 契约）。
          if (window.__DEVICE_MGR_TEST__.nullGpuCompute) smokeSnapshot.gpu_ext.compute_processes = null;
          return Promise.resolve(smokeSnapshot);
        case 'get_device_services':
          if (window.__DEVICE_MGR_TEST__.failServices) return Promise.reject(new Error('services down (smoke)'));
          return Promise.resolve([{ key: 'ollama', running: false, detail: 'probe timeout' }, { key: 'docker', running: true, detail: null }]);
        case 'get_external_models':
          if (window.__DEVICE_MGR_TEST__.failExternalModels) return Promise.reject(new Error('external models down (smoke)'));
          if (window.__DEVICE_MGR_TEST__.emptyExternalModels) return Promise.resolve({ models: [], warnings: [] });
          return Promise.resolve(${JSON.stringify(externalModelsFixture())});
        case 'model_store_availability': return Promise.resolve({ available: true, reason: null });
        case 'model_store_library':
          if (window.__DEVICE_MGR_TEST__.failStoreLibrary) return Promise.reject(new Error('store library down (smoke)'));
          return Promise.resolve({
          models: [
            {
              card_id: 'card-1', card_name: '驭光 14B', running: true, status: 'deployed',
              endpoint_url: 'http://127.0.0.1:30001/v1', model_name: 'yuguang14b', endpoint_model: 'Yuguang-14B',
              size_bytes: 8589934592,
            },
          ],
          corrected: [],
        });
        case 'device_doctor_prompt':
          return Promise.resolve('DEVICE_DOCTOR_MARKER::' + (args && args.kind) + (args && args.question ? '::' + args.question : ''));
        case 'device_register_external_model':
          return Promise.resolve({ id: 'm-registered-new', name: (args && args.name) || '', kind: (args && args.kind) || 'ollama' });
        case 'device_remove_external_model': return Promise.resolve(removeOutcome);
        default: return Promise.resolve(null);
      }
    }
    window.__DEVICE_MGR_TEST__ = {
      calls: calls,
      callsOf: function (cmd) { return calls.filter(function (item) { return item.cmd === cmd; }); },
      emit: emit,
      // 场景开关：读取失败横幅/重试与发送失败路径此前零覆盖（第四轮评审）。
      nullGpuCompute: false,
      failServices: false,
      setRemoveOutcome: function (next) { removeOutcome = next; },
    };
    window.__TAURI__ = {
      core: { invoke: invoke },
      event: {
        listen: function (name, handler) {
          (handlers[name] || (handlers[name] = [])).push(handler);
          return Promise.resolve(function () {
            var index = handlers[name].indexOf(handler);
            if (index >= 0) handlers[name].splice(index, 1);
          });
        },
        emit: emit,
      },
      window: {
        getCurrentWindow: function () {
          return {
            minimize: function () {},
            maximize: function () {},
            close: function () {},
            toggleMaximize: function () {},
            isMaximized: function () { return Promise.resolve(false); },
            onResized: function () { return Promise.resolve(function () {}); },
            startDragging: function () {},
          };
        },
      },
      dialog: { open: function () { return Promise.resolve(null); } },
    };
  })();`;
}

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const callsOf = (page, cmd) => page.evaluate(command =>
  window.__DEVICE_MGR_TEST__.calls.filter(call => call.cmd === command), cmd);

async function clickNavByLabel(page, label) {
  const ok = await page.evaluate(text => {
    // 侧栏收起时 NavItem 只剩图标，label 落在 title 属性上（与 settings_ui_smoke
    // 的 button[title="设置"] 同一判据）；展开时才是 span 文本。
    const titled = document.querySelector(`[title="${text}"]`);
    if (titled) { titled.click(); return true; }
    const spans = [...document.querySelectorAll('span')].filter(node => (node.textContent || '').trim() === text);
    const span = spans.find(node => node.parentElement && String(node.parentElement.className || '').includes('cursor-pointer'));
    const target = span ? span.parentElement : null;
    if (!target) return false;
    target.scrollIntoView({ block: 'center' });
    target.click();
    return true;
  }, label);
  if (!ok) throw new Error('找不到导航项: ' + label);
}

async function clickTestId(page, testId) {
  // evaluate 内直接 click：puppeteer 的 page.click 按几何中心命中，滚动到顶部的
  // 元素中心会被页内 sticky 分区导航盖住（真实用户滚一点就不会）。
  const ok = await page.evaluate(id => {
    const el = document.querySelector(`[data-testid="${id}"]`);
    if (!el) return false;
    el.scrollIntoView({ block: 'center' });
    el.click();
    return true;
  }, testId);
  if (!ok) throw new Error('找不到测试元素: ' + testId);
}

(async () => {
  const { url } = await startUiTestServer();
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: 'new',
    args: ['--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check'],
    userDataDir: PROFILE,
  });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error') errors.push('console:' + message.text()); });
  await page.evaluateOnNewDocument(injectSource());
  await page.setViewport({ width: 1440, height: 1000 });
  await page.goto(url, { waitUntil: 'networkidle0' });
  await page.waitForFunction(() => window.TauriBridge && document.querySelector('[data-testid="app-root"]'), { timeout: 20000 });
  await sleep(1000);

  const results = [];
  const rec = (name, pass, detail = '') => {
    results.push({ name, pass });
    console.log(`${pass ? '✅' : '❌'} ${name}${detail ? '  ' + detail : ''}`);
  };

  // ① MegaCube 平台：侧栏原「运行状态」入口显示为「设备管理」。
  const navLabel = await page.evaluate(() =>
    !!document.querySelector('[title="设备管理"]')
    || [...document.querySelectorAll('span')].some(node => (node.textContent || '').trim() === '设备管理'));
  rec('① megacubeDevice 平台侧栏入口为设备管理', navLabel);

  // ② 点击入口 → 设备管理页挂载（渲染开关复用 currentView==='monitor'）。
  await clickNavByLabel(page, '设备管理');
  await page.waitForFunction(() => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'monitor', { timeout: 8000 });
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-view"]'), { timeout: 8000 });
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-doctor"]'), { timeout: 8000 });
  rec('② 设备管理页挂载且轮询已发起', (await callsOf(page, 'get_device_snapshot')).length > 0);

  // ③ 区块齐全：概览（内嵌运行状态）/传感器/存储/网络/进程/模型管理/故障排查。
  const sections = await page.evaluate(() => Object.fromEntries([
    'device-manager-overview', 'device-manager-sensors', 'device-manager-storage',
    'device-manager-network', 'device-manager-processes', 'device-manager-models', 'device-manager-doctor',
  ].map(id => [id, !!document.querySelector(`[data-testid="${id}"]`)])));
  rec('③ 七个区块全部渲染', Object.values(sections).every(Boolean), JSON.stringify(sections));

  // ④ 快照 fixture 驱动真实渲染：头部设备名、系统盘徽标、传感器 chip、健康 chips。
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-disk-row"]'), { timeout: 8000 });
  const headerState = await page.evaluate(() => {
    const text = document.querySelector('[data-testid="device-manager-view"]')?.innerText || '';
    return {
      deviceName: text.includes('LinSeer MegaCube'),
      osLine: text.includes('Ubuntu 24.04.3 LTS') && text.includes('6.11.0-1019-nvidia') && text.includes('aarch64'),
      rootBadge: !!document.querySelector('[data-testid="device-manager-disk-root-badge"]'),
      diskRows: document.querySelectorAll('[data-testid="device-manager-disk-row"]').length,
      sensorChips: document.querySelectorAll('[data-testid="device-manager-sensor-chip"]').length,
      nvmeChip: text.includes('固态硬盘') && text.includes('45°C'),
      critAdvice: text.includes('已达临界温度'),
      healthChips: document.querySelectorAll('[data-testid="device-manager-health-chip"]').length,
      embeddedMonitor: text.includes('pinvou3-app'),
      netRows: document.querySelectorAll('[data-testid="device-manager-net-row"]').length,
      noErrorBanner: !document.querySelector('[data-testid="device-manager-error"]'),
    };
  });
  // 健康 chips 精确为 5（盘/内存/最高温/驱动/CUDA）：fixture 齐备时 GPU 双 chip
  // 掉一个也算回归（此前 >=3 放行，第四轮评审 P3）。
  await page.waitForFunction(() => document.querySelectorAll('[data-testid="device-manager-health-chip"]').length === 5, { timeout: 8000 });
  const procGpuRendered = await page.evaluate(() => !!document.querySelector('[data-testid="device-manager-proc-gpu"]'));
  rec('④ 快照驱动头部/磁盘/传感器/内嵌概览渲染',
    headerState.deviceName && headerState.osLine && headerState.rootBadge
    && headerState.diskRows === 2 && headerState.sensorChips === 3 && headerState.nvmeChip
    && headerState.critAdvice && headerState.healthChips === 5 && procGpuRendered && headerState.embeddedMonitor
    && headerState.netRows === 2 && headerState.noErrorBanner,
    JSON.stringify({ ...headerState, procGpuRendered }));

  // ④-b fixture 真值断言：健康 chips 的 label+value、网络行的速率/累计、进程行
  // 的 CPU%/内存、磁盘行的容量文本——期望值全部从 fixture 推导（EXPECTED）。
  // 此前的断言只数元素个数、从不读值，头部最高温 chip 渲染成 undefined°C 也
  // 全绿逃逸（九轮评审 P1）；这里同时要求整页不出现 "undefined"。
  const renderedValues = await page.evaluate(expected => {
    const normalize = node => (node?.innerText || '').replace(/\s+/g, ' ').trim();
    const chips = [...document.querySelectorAll('[data-testid="device-manager-health-chip"]')].map(normalize);
    const wifiRow = [...document.querySelectorAll('[data-testid="device-manager-net-row"]')]
      .find(row => (row.innerText || '').includes('wlP1s0'));
    return {
      viewText: document.querySelector('[data-testid="device-manager-view"]')?.innerText || '',
      chips,
      wifiRow: wifiRow ? normalize(wifiRow) : null,
      cpuRow: normalize(document.querySelector('[data-testid="device-manager-proc-cpu"]')),
      memRow: normalize(document.querySelector('[data-testid="device-manager-proc-mem"]')),
      rootDiskRow: normalize(document.querySelector('[data-testid="device-manager-disk-row"]')),
    };
  }, EXPECTED);
  // chip 断言按 label→value 配对（文案为 zh-Hans，与 fixture 同源推导的数值配对），
  // 单独断言 "45°C" 之类无法防止 chip 串位/值缺失。
  const expectedChipPairs = [
    ['系统盘', EXPECTED.diskPct],
    ['内存', EXPECTED.ramPct],
    ['最高温度', EXPECTED.maxTemp],
    ['驱动', EXPECTED.driver],
    ['CUDA', EXPECTED.cuda],
  ];
  const chipValuesOk = expectedChipPairs.every(([label, value]) =>
    renderedValues.chips.some(chip => chip.includes(label) && chip.includes(value)));
  rec('④-b 健康 chips/网络/进程/磁盘渲染 fixture 推导的真值且无 undefined',
    !renderedValues.viewText.includes('undefined')
    && chipValuesOk
    && !!renderedValues.wifiRow
    && renderedValues.wifiRow.includes(EXPECTED.netRxTotal) && renderedValues.wifiRow.includes(EXPECTED.netRxRate)
    && renderedValues.wifiRow.includes(EXPECTED.netTxTotal) && renderedValues.wifiRow.includes(EXPECTED.netTxRate)
    && renderedValues.cpuRow.includes(EXPECTED.procName) && renderedValues.cpuRow.includes(EXPECTED.procCpuPct)
    && renderedValues.memRow.includes(EXPECTED.procMemText)
    && renderedValues.rootDiskRow.includes(`${EXPECTED.diskAvailable} / ${EXPECTED.diskTotal}`),
    JSON.stringify({ expected: EXPECTED, rendered: renderedValues }));

  // ④-c GPU 计算进程查询失败（compute_processes null）：bridge 契约 null =
  // 查询不可知，视图必须出“查询不可用”提示而不是静默隐藏整块（round-16
  // 收口的披露项）；恢复数组后提示消失、进程列表回归。
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.nullGpuCompute = true; });
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-proc-gpu-unavailable"]')
    && !document.querySelector('[data-testid="device-manager-proc-gpu"]'), { timeout: 8000 });
  const gpuUnavailableShown = await page.evaluate(() =>
    (document.querySelector('[data-testid="device-manager-proc-gpu-unavailable"]')?.innerText || ''));
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.nullGpuCompute = false; });
  await page.waitForFunction(() => !document.querySelector('[data-testid="device-manager-proc-gpu-unavailable"]')
    && !!document.querySelector('[data-testid="device-manager-proc-gpu"]'), { timeout: 8000 });
  // 文案钉（round-19 P3）：此前只断 innerText 非空，空占位 div 也绿。
  rec('④-c GPU 计算进程查询失败出“查询不可用”提示、恢复后列表回归',
    gpuUnavailableShown.includes('查询暂不可用'), JSON.stringify({ gpuUnavailableShown }));

  // ⑤ 模型管理：商店行锁定（由模型商店管理 + 禁用按钮 + 前往模型商店），
  //    外部模型行显示类别/登记/状态徽标与接入按钮。
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-store-row"]'), { timeout: 8000 });
  const modelSection = await page.evaluate(() => {
    const text = document.querySelector('[data-testid="device-manager-models"]')?.innerText || '';
    const managedButton = document.querySelector('[data-testid="device-manager-store-managed"]');
    const externalRows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
    return {
      storeName: text.includes('驭光 14B') && text.includes('运行中'),
      endpointShown: text.includes('http://127.0.0.1:30001/v1'),
      managedLocked: !!managedButton && managedButton.disabled && (managedButton.innerText || '').includes('由模型商店管理'),
      gotoStore: !!document.querySelector('[data-testid="device-manager-goto-store"]'),
      externalCount: externalRows.length,
      ollamaRow: (externalRows[0]?.innerText || '').includes('qwen3:30b')
        && (externalRows[0]?.innerText || '').includes('Ollama')
        && (externalRows[0]?.innerText || '').includes('可用')
        && !!externalRows[0]?.querySelector('[data-testid="device-manager-link"]'),
      endpointRow: (externalRows[1]?.innerText || '').includes('my-endpoint')
        && (externalRows[1]?.innerText || '').includes('不可达')
        && (externalRows[1]?.innerText || '').includes('已登记'),
      // 探测不可达的行不允许接入：接入会顺手把它设为默认模型（评审 P2）。
      unreachableLinkDisabled: (() => {
        const link = externalRows[1]?.querySelector('[data-testid="device-manager-link"]');
        return !!link && link.disabled;
      })(),
      // 服务探测 chips 真渲染（stub 返回停止的 ollama + 运行的 docker）。
      // 运行态用读屏 aria-label 断言而非实现细节 CSS 类（round-19 P3）。
      serviceChips: (() => {
        const chips = [...document.querySelectorAll('[data-testid="device-manager-service-chip"]')];
        return chips.length === 2
          && (chips[0].innerText || '').includes('Ollama')
          && (chips[1].innerText || '').includes('Docker')
          && chips[1].querySelector('[aria-label="运行中"]') != null;
      })(),
      // 远端端点行：接入被禁用并带说明（回环尺子，第三轮评审 P1）。
      remoteLinkBlocked: (() => {
        const remote = externalRows[2];
        const button = remote?.querySelector('[data-testid="device-manager-link-remote"]');
        return !!button && button.disabled;
      })(),
      // 商店 ollama 卡片的 daemon 形态（name:latest）被 managedByStore 的去 tag
      // 臂识别为商店管理：移除禁用 + managed 提示（第八轮评审）。
      storeTaggedRowManaged: (() => {
        const tagged = externalRows[3];
        const remove = tagged?.querySelector('[data-testid="device-manager-remove"]');
        return !!remove && remove.disabled && (remove.title || '').includes('由模型商店管理');
      })(),
    };
  });
  rec('⑤ 商店行锁定态与外部模型行徽标齐全', Object.values(modelSection).every(Boolean), JSON.stringify(modelSection));

  // ⑤-b 接入品悟 happy path（页面的主动作，此前零覆盖）：未登记 Ollama 行用
  // 回环回退地址 save_model，随后把新模型设为新建会话默认（set_active_model）。
  await page.evaluate(() => {
    const row = document.querySelector('[data-testid="device-manager-external-row"]');
    row?.querySelector('[data-testid="device-manager-link"]')?.click();
  });
  await page.waitForFunction(() => window.__DEVICE_MGR_TEST__.callsOf('save_model').length > 0, { timeout: 8000 });
  const linkCalls = await page.evaluate(() => {
    const saved = window.__DEVICE_MGR_TEST__.callsOf('save_model')[0]?.args?.model || null;
    return {
      saved,
      activated: window.__DEVICE_MGR_TEST__.callsOf('set_active_model')[0]?.args || null,
    };
  });
  rec('⑤-b 接入品悟写入回环地址与模型并设为默认',
    !!linkCalls.saved && linkCalls.saved.base_url === 'http://127.0.0.1:11434/v1'
    && linkCalls.saved.model === 'qwen3:30b'
    && !!linkCalls.activated && linkCalls.activated.id === linkCalls.saved.id,
    JSON.stringify(linkCalls));

  // ⑥ 外部模型两段式移除：先静默登记（ollama 未登记行）→ 守卫移除命令携带确认名。
  // 后端警告注入：聚合 toast 的后端警告臂此前从未渲染（stub warnings 恒空）。
  await page.evaluate(() => window.__DEVICE_MGR_TEST__.setRemoveOutcome({
    removed_registry: true, removed_weights: false, ollama_deleted: false,
    warnings: ['ollama 删除失败（smoke 注入）'],
  }));
  // 确认按钮 400ms 死区：armed 面板与「移除」按钮同坐标原地替换，双击的第二击
  // 会落在确认按钮（或勾选框）上——死区把这个手势还原成两步（第四轮 P2）。
  // 点击与首见观察放在同一个 in-page Promise 里 5ms 粒度轮询（与 ⑥-b 同款）：
  // 跨 CDP 往返的两次 evaluate（click → 等存在 → 再读 disabled）在负载下会
  // 错过 400ms 窗口，把活着的死区误报成回归（九轮评审 P2）。
  const confirmDisabledAtFirstSight = await page.evaluate(() => new Promise(resolve => {
    document.querySelector('[data-testid="device-manager-remove"]')?.click();
    let tries = 0;
    const timer = setInterval(() => {
      tries += 1;
      const confirm = document.querySelector('[data-testid="device-manager-remove-confirm"]');
      if (confirm) {
        clearInterval(timer);
        resolve(confirm.disabled);
      } else if (tries > 400) {
        clearInterval(timer);
        resolve(null);
      }
    }, 5);
  }));
  await page.waitForFunction(() => {
    const confirm = document.querySelector('[data-testid="device-manager-remove-confirm"]');
    return !!confirm && !confirm.disabled;
  }, { timeout: 4000 });
  const armedState = await page.evaluate(() => {
    const row = document.querySelector('[data-testid="device-manager-external-row"]');
    return {
      intro: (row?.innerText || '').includes('登记信息'),
      // Ollama 行不再显示"同时删除模型文件"勾选框：后端对 Ollama 无条件从端点
      // 删除，勾选框暗示"不勾就保留文件"是假的（评审 P2）。
      weightsToggle: !!row?.querySelector('[data-testid="device-manager-delete-weights"]'),
    };
  });
  await page.evaluate(() => {
    document.querySelector('[data-testid="device-manager-remove-confirm"]')?.click();
  });
  await page.waitForFunction(() => window.__DEVICE_MGR_TEST__.callsOf('device_remove_external_model').length > 0, { timeout: 8000 });
  const removeCalls = await page.evaluate(() => ({
    registered: window.__DEVICE_MGR_TEST__.callsOf('device_register_external_model')[0]?.args,
    removed: window.__DEVICE_MGR_TEST__.callsOf('device_remove_external_model')[0]?.args,
    // ⑤-b 已把同一行接入品悟：移除必须回收接入的模型行（cleanLinkedRows 契约）。
    // 整段删掉清理循环此前全测试照绿（round-5 review P3）。
    deleteModelCalls: window.__DEVICE_MGR_TEST__.callsOf('delete_model').length,
  }));
  rec('⑥ ollama 行移除先登记再走守卫命令且带二次确认（无权重勾选框）',
    confirmDisabledAtFirstSight && armedState.intro && !armedState.weightsToggle
    && removeCalls.registered && removeCalls.registered.name === 'qwen3:30b' && removeCalls.registered.kind === 'ollama'
    && removeCalls.removed && removeCalls.removed.id === 'm-registered-new'
    && removeCalls.removed.confirmName === 'qwen3:30b'
    && removeCalls.removed.deleteWeights === false
    && removeCalls.deleteModelCalls === 1,
    JSON.stringify({ confirmDisabledAtFirstSight, armedState, removeCalls }));

  // ⑥-d 聚合 toast：后端警告与移除结果必须同屏（单槽 toast 曾只显示最后一条，
  // 第三轮修复；本轮补钉其渲染）。⑤-b 接入的那行被回收后必须逐行点名
  // （round-12 评审建议：逐行列出被回收行）。
  await page.waitForFunction(() => !!document.querySelector('[data-testid="model-store-toast"]'), { timeout: 4000 });
  const aggregateToast = await page.evaluate(() => document.querySelector('[data-testid="model-store-toast"]')?.innerText || '');
  rec('⑥-d 聚合 toast 同屏携带后端警告、移除结果与被回收行',
    aggregateToast.includes('ollama 删除失败（smoke 注入）') && aggregateToast.includes('已移除「qwen3:30b」')
    && aggregateToast.includes('已同时从品悟的模型列表移除配置「qwen3:30b」'),
    aggregateToast);
  await page.evaluate(() => window.__DEVICE_MGR_TEST__.setRemoveOutcome({
    removed_registry: true, removed_weights: false, ollama_deleted: true, warnings: [],
  }));

  // ⑥-b 带权重的登记行：armed 态回显权重目录完整路径 + 显示删除勾选框 +
  //     勾选后出现不可撤销提示（删除后果必须让用户看见，评审 P2）。
  // 点击与首见观察放在同一个 in-page Promise 里：React 离散事件同步渲染后
  // 5ms 粒度轮询，确定性地抓到 400ms 死区内的禁用态——跨 CDP 往返的两次
  // evaluate 会错过窗口（evaluate 返回时门禁可能已开）。
  const checkboxGatedAtFirstSight = await page.evaluate(() => new Promise(resolve => {
    const rows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
    const target = rows.find(candidate => (candidate.innerText || '').includes('my-endpoint'));
    target?.querySelector('[data-testid="device-manager-remove"]')?.click();
    let tries = 0;
    const timer = setInterval(() => {
      tries += 1;
      const armed = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')]
        .find(candidate => (candidate.innerText || '').includes('my-endpoint'));
      const toggle = armed?.querySelector('[data-testid="device-manager-delete-weights"]');
      const confirm = armed?.querySelector('[data-testid="device-manager-remove-confirm"]');
      if (toggle && confirm) {
        clearInterval(timer);
        resolve({ toggleDisabled: toggle.disabled, confirmDisabled: confirm.disabled });
      } else if (tries > 400) {
        clearInterval(timer);
        resolve(null);
      }
    }, 5);
  }));
  // 勾选框受 400ms 死区门禁（round-5 P3）：先等门禁放开再点击。
  await page.waitForFunction(
    () => {
      const rows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
      const target = rows.find(candidate => (candidate.innerText || '').includes('my-endpoint'));
      return target?.querySelector('[data-testid="device-manager-delete-weights"]')?.disabled === false;
    },
    { timeout: 4000 },
  );
  await page.evaluate(() => {
    const rows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
    const target = rows.find(candidate => (candidate.innerText || '').includes('my-endpoint'));
    target?.querySelector('[data-testid="device-manager-delete-weights"]')?.click();
  });
  // 勾选后的"不可撤销"提示是 React 状态渲染，等它出现再读。
  await page.waitForFunction(
    () => [...document.querySelectorAll('[data-testid="device-manager-external-row"]')]
      .some(row => row.querySelector('[data-testid="device-manager-remove-weights-path"]')
        && (row.innerText || '').includes('不可恢复')),
    { timeout: 4000 },
  );
  const armedEndpoint = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
    const target = rows.find(candidate => (candidate.innerText || '').includes('my-endpoint'));
    return {
      pathEcho: (target?.querySelector('[data-testid="device-manager-remove-weights-path"]')?.innerText || '').includes('/home/u/models/my-endpoint'),
      weightsToggle: !!target?.querySelector('[data-testid="device-manager-delete-weights"]'),
      irreversibleShown: (target?.innerText || '').includes('不可恢复'),
    };
  });
  await page.evaluate(() => {
    const rows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
    rows.find(candidate => (candidate.innerText || '').includes('my-endpoint'))
      ?.querySelector('[data-testid="device-manager-remove-cancel"]')?.click();
  });
  // 取消必须真的收起 armed 面板且不发起删除命令——此前取消按钮没有 testid，
  // 这一步是静默空转的死步骤，取消路径零覆盖（第三轮评审 P1）。
  await page.waitForFunction(
    () => ![...document.querySelectorAll('[data-testid="device-manager-external-row"]')]
      .some(row => row.querySelector('[data-testid="device-manager-remove-confirm"]')),
    { timeout: 4000 },
  );
  const cancelOutcome = await page.evaluate(() => ({
    armedClosed: ![...document.querySelectorAll('[data-testid="device-manager-external-row"]')]
      .some(row => row.querySelector('[data-testid="device-manager-remove-confirm"]')),
    removeCallsStillOne: window.__DEVICE_MGR_TEST__.callsOf('device_remove_external_model').length === 1,
  }));
  rec('⑥-b 登记行 armed 态回显权重路径+勾选框+不可撤销提示',
    armedEndpoint.pathEcho && armedEndpoint.weightsToggle && armedEndpoint.irreversibleShown
    && checkboxGatedAtFirstSight?.toggleDisabled === true
    && checkboxGatedAtFirstSight?.confirmDisabled === true,
    JSON.stringify({ armedEndpoint, checkboxGatedAtFirstSight }));
  rec('⑥-c 取消移除收起 armed 面板且不发起删除命令',
    cancelOutcome.armedClosed && cancelOutcome.removeCallsStillOne,
    JSON.stringify({ cancelOutcome }));

  // ⑦ 故障排查快捷卡 → device_doctor_prompt + 新建普通会话 + chat 携带提示词。
  const beforeDoctor = await page.evaluate(() => window.__DEVICE_MGR_TEST__.calls.length);
  await clickTestId(page, 'device-manager-doctor-packages');
  await page.waitForFunction(() => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'chat', { timeout: 15000 });
  await page.waitForFunction(() => window.__DEVICE_MGR_TEST__.callsOf('chat').length > 0, { timeout: 15000 });
  const doctorCalls = await page.evaluate(before => ({
    prompt: window.__DEVICE_MGR_TEST__.calls.slice(before).find(call => call.cmd === 'device_doctor_prompt'),
    created: window.__DEVICE_MGR_TEST__.calls.slice(before).some(call => call.cmd === 'create_session'),
    chat: window.__DEVICE_MGR_TEST__.calls.slice(before).find(call => call.cmd === 'chat'),
  }), beforeDoctor);
  rec('⑦ 快捷卡触发提示词→新会话→chat 携带标记',
    doctorCalls.prompt && doctorCalls.prompt.args.kind === 'packages'
    && doctorCalls.created
    && doctorCalls.chat && typeof doctorCalls.chat.args.message === 'string'
    && doctorCalls.chat.args.message.includes('DEVICE_DOCTOR_MARKER::packages'),
    JSON.stringify(doctorCalls));

  // ⑦-b 离开设备页后 2s 轮询必须停：视图卸载调 stopDevicePolling——此前只钉了
  // bridge 的启停语义，没钉视图的实际接线（第四轮评审 P3）。
  const snapshotCallsAtNav = await page.evaluate(() => window.__DEVICE_MGR_TEST__.callsOf('get_device_snapshot').length);
  await sleep(2600);
  const snapshotCallsAfterNav = await page.evaluate(() => window.__DEVICE_MGR_TEST__.callsOf('get_device_snapshot').length);
  rec('⑦-b 离开设备页后 2s 轮询停止',
    snapshotCallsAfterNav === snapshotCallsAtNav,
    `at nav=${snapshotCallsAtNav}, after 2.6s=${snapshotCallsAfterNav}`);

  // ⑧ 后端读取失败面：横幅 + 重试按钮 + 恢复（此前整条失败面只被断言"不存在"，
  // 横幅回归消失全绿——第四轮评审 P2）。重试按钮的接线必须真钉：2s 后台轮询在
  // failSnapshot=false 后 ≤2s 自愈，"恢复"半边曾是死按钮也全绿的假覆盖（第七轮
  // 评审 P2）——先在保持失败的状态下，以上一拍后台轮询为同步点，要求点击后
  // 1s 内（间隔 2s 的后台拍做不到）出现下一次命令重发；随后才放开失败开关验证恢复。
  await clickNavByLabel(page, '设备管理');
  await page.waitForFunction(() => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'monitor', { timeout: 8000 });
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-view"]'), { timeout: 8000 });
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failSnapshot = true; });
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-error"]'), { timeout: 8000 });
  const bannerText = await page.evaluate(() => document.querySelector('[data-testid="device-manager-error"]')?.innerText || '');
  const retryVisible = await page.evaluate(() => !!document.querySelector('[data-testid="device-manager-retry"]'));
  // 同步点：等一拍后台失败轮过去（计数 +1）——此刻下一拍后台轮询在 ~2s 之外。
  const beforeRetryClick = await page.evaluate(() => window.__DEVICE_MGR_TEST__.callsOf('get_device_snapshot').length);
  await page.waitForFunction(
    (before) => window.__DEVICE_MGR_TEST__.callsOf('get_device_snapshot').length > before,
    { timeout: 8000 },
    beforeRetryClick,
  );
  // 失败仍在生效时点重试：1s 内必须出现下一次 get_device_snapshot——后台拍
  // 刚走完、够不到这个窗口，只有重试按钮自己的 stop+start 立即轮询能满足；
  // 按钮拆线时本断言必红。
  await clickTestId(page, 'device-manager-retry');
  const retryFired = await page.waitForFunction(
    (before) => window.__DEVICE_MGR_TEST__.callsOf('get_device_snapshot').length > before,
    { timeout: 1000 },
    beforeRetryClick + 1,
  ).then(() => true, () => false);
  // 放开失败开关：恢复横幅消失（重试轮或放开后的后台轮触发，恢复口径同前）。
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failSnapshot = false; });
  await page.waitForFunction(() => !document.querySelector('[data-testid="device-manager-error"]'), { timeout: 8000 });
  rec('⑧ 快照失败出横幅、重试按钮真实重发命令、放开后恢复面板',
    bannerText.includes('设备状态读取失败') && retryVisible && retryFired,
    JSON.stringify({ bannerText, retryVisible, retryFired }));

  // ⑨ 自定义提问：计数/禁用态、发送失败保留输入（「问题已保留」的承诺此前
  // 无覆盖）、成功直达会话并携带问题原文。
  await page.type('[data-testid="device-manager-doctor-input"]', '风扇一直响正常吗');
  const customReady = await page.evaluate(() => {
    const send = document.querySelector('[data-testid="device-manager-doctor-send"]');
    return {
      sendEnabled: !!send && !send.disabled,
      counterHidden: !document.querySelector('[data-testid="device-manager-doctor-counter"]'),
    };
  });
  // ⑩-a 前半窗：①–⑧ 与 ⑨ 的准备段不得产生任何运行时报错。必须在注入预期
  // 失败**之前**断言累计错误——原先"清空后同步断言空集"的写法观测窗为零，
  // 任何实现都必过（round-6 review P1）。
  rec('⑩-a 全程无运行时报错（①–⑧）', errors.length === 0, errors.slice(0, 3).join(' | '));
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failChatSend = true; });
  await clickTestId(page, 'device-manager-doctor-send');
  await page.waitForFunction(() => !!document.querySelector('[data-testid="model-store-toast"]'), { timeout: 8000 });
  const failedSend = await page.evaluate(() => ({
    toast: document.querySelector('[data-testid="model-store-toast"]')?.innerText || '',
    keptQuestion: (document.querySelector('[data-testid="device-manager-doctor-input"]') || {}).value || '',
  }));
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failChatSend = false; });
  await clickTestId(page, 'device-manager-doctor-send');
  await page.waitForFunction(() => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'chat', { timeout: 15000 });
  const customQuestionDelivered = await page.evaluate(() => {
    const calls = window.__DEVICE_MGR_TEST__.callsOf('chat');
    return (calls.at(-1)?.args?.message || '').includes('风扇一直响正常吗');
  });
  rec('⑨ 自定义提问失败保留输入、成功携带问题直达会话',
    customReady.sendEnabled && customReady.counterHidden
    && failedSend.toast.includes('没有发出去') && failedSend.keptQuestion.includes('风扇一直响正常吗')
    && customQuestionDelivered,
    JSON.stringify({ customReady, failedSend, customQuestionDelivered }));

  // ⑩-b 商店库读取失败：错误行 + 前往商店入口 + 重试恢复——不得降级成
  // "还没有可管理的模型"假空态（round-5 review P3；离开再回来触发 remount
  // 重新拉取）。
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failStoreLibrary = true; });
  await clickNavByLabel(page, '设备管理');
  await page.waitForFunction(() => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'monitor', { timeout: 8000 });
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-store-error"]'), { timeout: 8000 });
  const storeErrorState = await page.evaluate(() => {
    const row = document.querySelector('[data-testid="device-manager-store-error"]');
    return {
      text: row?.innerText || '',
      retryButton: !!document.querySelector('[data-testid="device-manager-store-retry"]'),
      gotoStore: !!row && [...(row.querySelectorAll('button') || [])]
        .some(button => (button.innerText || '').includes('前往模型商店')),
    };
  });
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failStoreLibrary = false; });
  // 重试点击按 testid 定位（round-19 P3）：原先取行内第一个 button，依赖
  // DOM 序（重试/前往商店换位即误红）。
  await page.evaluate(() => document.querySelector('[data-testid="device-manager-store-retry"]')?.click());
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-store-row"]'), { timeout: 8000 });
  rec('⑩-b 商店库读取失败出错误行+前往商店入口+重试恢复',
    storeErrorState.text.includes('加载失败') && storeErrorState.retryButton && storeErrorState.gotoStore,
    JSON.stringify(storeErrorState));

  // ⑩-c 外部模型读取失败（商店行仍在、外部区块为空）：失败此前只剩 3.2s 的
  // toast，外部区块需要自己的常驻错误行 + 重试（round-5 review P3）。先把外部
  // 列表刷空（错误行只在区块为空时呈现——列表尚有旧行时旧行继续展示 + toast）。
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.emptyExternalModels = true; });
  await clickTestId(page, 'device-manager-models-refresh');
  await page.waitForFunction(() => ![...document.querySelectorAll('[data-testid="device-manager-external-row"]')].length, { timeout: 8000 });
  await page.evaluate(() => {
    window.__DEVICE_MGR_TEST__.emptyExternalModels = false;
    window.__DEVICE_MGR_TEST__.failExternalModels = true;
  });
  await clickTestId(page, 'device-manager-models-refresh');
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-external-error"]'), { timeout: 8000 });
  const externalErrorShown = await page.evaluate(() =>
    (document.querySelector('[data-testid="device-manager-external-error"]')?.innerText || '').includes('刷新失败'));
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failExternalModels = false; });
  await page.evaluate(() => document.querySelector('[data-testid="device-manager-external-retry"]')?.click());
  await page.waitForFunction(() => !document.querySelector('[data-testid="device-manager-external-error"]'), { timeout: 8000 });
  rec('⑩-c 外部模型读取失败常驻错误行+重试恢复', externalErrorShown,
    JSON.stringify({ externalErrorShown }));

  // ⑩ 后半窗：⑨ 的预期注入失败（transition 基建的 console.error）与 ⑩-b/⑩-c
  // 故意注入的读取失败都在上面收尾了——清空后走一段**真实交互**（模型区刷新 →
  // 等可观察效果 → 再 settle 两拍），让"无报错"断言拥有真实观察窗；对空集同步
  // 断言等于没有断言（round-6 review P1）。
  errors.length = 0;
  await clickTestId(page, 'device-manager-models-refresh');
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-external-row"]'), { timeout: 8000 });
  await sleep(300);
  rec('⑩ 后半窗无运行时报错（⑩-b/⑩-c/刷新路径，⑨ 预期噪音除外）', errors.length === 0, errors.slice(0, 3).join(' | '));

  // ⑩-d servicesStale 陈旧旗：有基线后连续 3 拍（10s 慢轮询，≈30s）探测失败
  // 立「状态可能过期」旗、旧 chips 保留；恢复拍旗退（第六轮 P2 特性，此前
  // smoke 零触达——旗位接错/与 failed 互换时套件照绿，round-19 P2 补钉）。
  // 注入的失败只 console.warn，不影响 ⑩ 的 errors 收集（已先行断言完毕）。
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failServices = true; });
  const staleShown = await page.waitForFunction(
    () => !!document.querySelector('[data-testid="device-manager-services-stale"]')
      && !!document.querySelector('[data-testid="device-manager-service-chip"]'),
    { timeout: 45000 },
  ).then(() => true).catch(() => false);
  await page.evaluate(() => { window.__DEVICE_MGR_TEST__.failServices = false; });
  const staleRecovered = await page.waitForFunction(
    () => !document.querySelector('[data-testid="device-manager-services-stale"]'),
    { timeout: 20000 },
  ).then(() => true).catch(() => false);
  rec('⑩-d 服务探测连续失败立陈旧旗且旧行保留、恢复后旗退',
    staleShown && staleRecovered, JSON.stringify({ staleShown, staleRecovered }));

  // ⑪ 非 MegaCube 负控：megacubeDevice=false 整页重载后，侧栏回到「运行状态」、
  // 无「设备管理」入口，点击后是旧 MonitorView 而非设备管理页（十二轮评审 P2：
  // 门控此前只钉了正向臂——main.jsx 把 megacubeDevice 恒置 true 时当时全部
  // 测试照绿，非 MegaCube 平台入口会被静默换掉）。
  await page.evaluate(() => { window.__testSetMegacubeDevice(false); });
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForFunction(() => window.TauriBridge && document.querySelector('[data-testid="app-root"]'), { timeout: 20000 });
  await sleep(1000);
  const plainState = await page.evaluate(() => {
    // 侧栏收起时 label 只落在 title 属性（与 clickNavByLabel 同一双判据）。
    const spanLabels = [...document.querySelectorAll('span')].map(node => (node.textContent || '').trim());
    const has = text => spanLabels.includes(text) || !!document.querySelector(`[title="${text}"]`);
    return {
      monitorEntry: has('运行状态'),
      deviceEntryAbsent: !has('设备管理'),
    };
  });
  rec('⑪ 非 MegaCube 平台侧栏保持运行状态且无设备管理入口',
    plainState.monitorEntry && plainState.deviceEntryAbsent, JSON.stringify(plainState));
  await clickNavByLabel(page, '运行状态');
  await page.waitForFunction(() => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'monitor', { timeout: 8000 });
  // 正向证据（round-19 P2）：switch 位就绪 ≠ 视图本体挂载——慢机上懒 chunk 未
  // 加载时「什么都没挂载」也会让下面的 absence 断言假绿。等 MonitorView 的
  // 真实内容（应用名 + stub 快照的 CPU 型号）出现后再断设备管理视图缺席。
  const monitorRendered = await page.waitForFunction(() => {
    const text = document.body.innerText || '';
    return text.includes('pinvou3-app') && text.includes('NVIDIA GB10');
  }, { timeout: 15000 }).then(() => true).catch(() => false);
  const monitorOnly = await page.evaluate(() => ({
    deviceViewAbsent: !document.querySelector('[data-testid="device-manager-view"]'),
  }));
  rec('⑪-b 非 MegaCube 平台运行状态页渲染旧 MonitorView 且不渲染设备管理视图',
    monitorRendered && monitorOnly.deviceViewAbsent,
    JSON.stringify({ monitorRendered, ...monitorOnly }));

  await browser.close();

  const failed = results.filter(result => !result.pass).length;
  console.log(failed ? `\n❌ ${failed}/${results.length} FAILED` : `\n✅ ALL ${results.length} PASS`);
  process.exit(failed ? 1 : 0);
})().catch(error => {
  console.error('FATAL', error.stack || error.message);
  process.exit(1);
});
