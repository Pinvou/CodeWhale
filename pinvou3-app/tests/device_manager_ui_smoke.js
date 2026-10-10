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

function externalModelsFixture() {
  return {
    models: [
      { id: 'm-ollama-1', name: 'qwen3:30b', kind: 'ollama', base_url: 'http://127.0.0.1:11434/v1', model: 'qwen3:30b', weights_path: null, installed_at: '2026-10-01T00:00:00Z', note: '', status: 'available', registered: false, source: 'ollama' },
      { id: 'm-reg-1', name: 'my-endpoint', kind: 'openai_endpoint', base_url: 'http://127.0.0.1:9999/v1', model: 'my-model', weights_path: '/home/u/models/my-endpoint', installed_at: '2026-10-02T00:00:00Z', note: '', status: 'unreachable', registered: true, source: 'registry' },
      { id: 'm-reg-2', name: 'lan-endpoint', kind: 'openai_endpoint', base_url: 'http://192.168.1.50:8000/v1', model: 'lan-model', weights_path: null, installed_at: '2026-10-03T00:00:00Z', note: '', status: 'available', registered: true, source: 'registry' },
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
    function record(cmd, args) { calls.push({ cmd: cmd, args: args || null }); }
    function emit(name, payload) {
      return Promise.all((handlers[name] || []).slice().map(function (handler) {
        return handler({ payload: payload || {} });
      }));
    }
    function invoke(cmd, args) {
      record(cmd, args);
      switch (cmd) {
        case 'get_platform_capabilities': return Promise.resolve({
          os: 'linux',
          megacubeDevice: true,
          showMegacubeSite: false,
          showSuperPermissionSettings: false,
          usesBundledDependencyInstaller: true,
          taskCompletionNotificationsDefault: true,
          voiceShortcutNative: false,
          browserNativeDisplay: false,
          codexAcpSupported: false,
          localVllmSupported: false,
          teamRelaySupported: true,
        });
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
        case 'chat': return Promise.resolve(null);
        // 内嵌运行状态页每秒轮询；给一份最小快照，避免错误轮刷屏（内存 chip 也随 bs.monitor._fmt 生效）。
        case 'get_monitor_snapshot': return Promise.resolve({
          cpu: { name: 'NVIDIA GB10', total_usage_pct: 12 },
          ram: { total_kib: 129*1024*1024, used_kib: 51*1024*1024, swap_total_kib: 0, swap_used_kib: 0 },
          gpu: null, vllm: null, self_perf: null,
          app: { pinvou3_version: '0.10.6', deepseek_tui_version: '0.0.0', session_uptime_secs: 60 },
        });
        // ── MegaCube 设备管理命令 ──
        case 'get_device_snapshot': return Promise.resolve(${JSON.stringify(deviceSnapshotFixture())});
        case 'get_device_services': return Promise.resolve([{ key: 'ollama', running: false, detail: 'probe timeout' }, { key: 'docker', running: true, detail: null }]);
        case 'get_external_models': return Promise.resolve(${JSON.stringify(externalModelsFixture())});
        case 'model_store_availability': return Promise.resolve({ available: true, reason: null });
        case 'model_store_library': return Promise.resolve({
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
  rec('④ 快照驱动头部/磁盘/传感器/内嵌概览渲染',
    headerState.deviceName && headerState.osLine && headerState.rootBadge
    && headerState.diskRows === 2 && headerState.sensorChips === 3 && headerState.nvmeChip
    && headerState.critAdvice && headerState.healthChips >= 3 && headerState.embeddedMonitor
    && headerState.netRows === 2 && headerState.noErrorBanner,
    JSON.stringify(headerState));

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
      serviceChips: (() => {
        const chips = [...document.querySelectorAll('[data-testid="device-manager-service-chip"]')];
        return chips.length === 2
          && (chips[0].innerText || '').includes('Ollama')
          && (chips[1].innerText || '').includes('Docker')
          && chips[1].querySelector('.bg-\\[\\#34C759\\]') != null;
      })(),
      // 远端端点行：接入被禁用并带说明（回环尺子，第三轮评审 P1）。
      remoteLinkBlocked: (() => {
        const remote = externalRows[2];
        const button = remote?.querySelector('[data-testid="device-manager-link-remote"]');
        return !!button && button.disabled;
      })(),
    };
  });
  rec('⑤ 商店行锁定态与外部模型行徽标齐全', Object.values(modelSection).every(Boolean), JSON.stringify(modelSection));

  // ⑥ 外部模型两段式移除：先静默登记（ollama 未登记行）→ 守卫移除命令携带确认名。
  await clickTestId(page, 'device-manager-remove');
  await page.waitForFunction(() => !!document.querySelector('[data-testid="device-manager-remove-confirm"]'), { timeout: 4000 });
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
  }));
  rec('⑥ ollama 行移除先登记再走守卫命令且带二次确认（无权重勾选框）',
    armedState.intro && !armedState.weightsToggle
    && removeCalls.registered && removeCalls.registered.name === 'qwen3:30b' && removeCalls.registered.kind === 'ollama'
    && removeCalls.removed && removeCalls.removed.id === 'm-registered-new'
    && removeCalls.removed.confirmName === 'qwen3:30b'
    && removeCalls.removed.deleteWeights === false,
    JSON.stringify({ armedState, removeCalls }));

  // ⑥-b 带权重的登记行：armed 态回显权重目录完整路径 + 显示删除勾选框 +
  //     勾选后出现不可撤销提示（删除后果必须让用户看见，评审 P2）。
  await page.evaluate(() => {
    const rows = [...document.querySelectorAll('[data-testid="device-manager-external-row"]')];
    const target = rows.find(candidate => (candidate.innerText || '').includes('my-endpoint'));
    target?.querySelector('[data-testid="device-manager-remove"]')?.click();
  });
  await page.waitForFunction(
    () => !!document.querySelector('[data-testid="device-manager-remove-weights-path"]'),
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
    armedEndpoint.pathEcho && armedEndpoint.weightsToggle && armedEndpoint.irreversibleShown,
    JSON.stringify({ armedEndpoint }));
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

  rec('⑧ 全程无运行时报错', errors.length === 0, errors.slice(0, 3).join(' | '));
  await browser.close();

  const failed = results.filter(result => !result.pass).length;
  console.log(failed ? `\n❌ ${failed}/${results.length} FAILED` : `\n✅ ALL ${results.length} PASS`);
  process.exit(failed ? 1 : 0);
})().catch(error => {
  console.error('FATAL', error.stack || error.message);
  process.exit(1);
});
