#!/usr/bin/env node
/**
 * MegaCube 设备管理面板冒烟 — headless chromium + mock __TAURI__，加载真 dist。
 *
 * 覆盖：
 *   ① 管理能力可用（cms_status available:true）时运行状态页出现「MegaCube」页签，
 *     面板渲染出真实数据（模型 id、引擎 sha8、容量数字、活跃标记）；
 *   ② 商店保留未绑定的存量用户路由，不按名字在前端自动改写；
 *   ④ switch 换胎进行中弹维护窗，完成后保留管理连接并给出终结确认卡；
 *   ⑤ 仅挪默认（无任务）时保留管理连接，且不弹维护窗；
 *   ⑥ 启停（enable/disable）任务不弹维护窗、不触发选择跟随——进度由发起方
 *     （商店 run 日志 / MCP 应答）各自呈现；
 *   ⑦ cmsd 侧装好但无接入行的健康模型：不自动建行或替换管理连接。
 *   ① 面板数据断言通过后截图（默认 /tmp/megacube-panel.png，可用参数覆盖）。
 *
 * 数据源：默认用 2026-10-12 真机验收抓取的应答样本；设了 CMSD_SOCKET 且可达时
 * 改走真实 cmsd 现场抓取（curl --unix-socket），用于真机目验。
 *
 * 用法:node tests/megacube_panel_smoke.js [截图路径]
 * （全 PASS → exit 0，任一 FAIL → exit 1，缺依赖 → 打印 SKIP: 行首说明并 exit 2）
 */
const finishFixtures = require('./helpers/fixture-exit.cjs');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execSync } = require('child_process');
const { startUiTestServer } = require('./ui_test_server');

function loadPuppeteer() {
  try { return require('./helpers/puppeteer-fixture.cjs'); } catch (_) { /* fall through */ }
  const npx = path.join(os.homedir(), '.npm', '_npx');
  if (fs.existsSync(npx)) {
    for (const d of fs.readdirSync(npx)) {
      const p = path.join(npx, d, 'node_modules', 'puppeteer-core');
      if (fs.existsSync(p)) { try { return require('./helpers/browser-fixture.cjs').wrapNative(require(p)); } catch (_) { /* next */ } }
    }
  }
  console.error('SKIP: 找不到 puppeteer-core');
  process.exit(2);
}
const puppeteer = loadPuppeteer();
const CHROME = process.env.CHROME || [
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/snap/bin/chromium',
].find(p => fs.existsSync(p));
if (!CHROME) { console.error('SKIP: 未找到 chromium/chrome'); process.exit(2); }
const SCREENSHOT = process.argv[2] || '/tmp/megacube-panel.png';

// ── 数据：默认真机验收样本；CMSD_SOCKET 可达时抓现场 ──────────
const FIXTURE = {
  status: {
    active_model: 'ornith15-35b-a3b-nvfp4', degraded: [],
    engines: { installed: 2, external_processes: 0 }, gateway: '127.0.0.1:8000',
    service: 'cmsd', started_at: 1791813210, systemd_mode: 'user', uptime_s: 4520, version: '0.1.0',
  },
  models: {
    active_model: 'ornith15-35b-a3b-nvfp4',
    models: [
      {
        card: 'ornith15-35b-a3b-nvfp4', model_id: 'Ornith-1.5-35B-A3B-NVFP4', status: 'running',
        healthy: true, port: 18080, restart_count: 3, last_restart_at: 1791814956,
        measured_footprint_gib: 33, boot_enabled: true, weights_bytes: 22892664832,
      },
      {
        card: 'qwen38-27b-nvfp4-tachyon', model_id: 'Qwen3.8-27B-NVFP4', status: 'stopped',
        weights_bytes: 21944459776,
      },
    ],
  },
  engines: {
    engines: [
      {
        id: '0f8a4a7c', engine_type: 'native',
        path: '/home/example/.local/share/local-model-manager/engines/tachyon-serve/0f8a4a7c',
        installed: true,
        instances: [{ source: 'managed', pid: 1026973, port: 18080, healthy: true }],
      },
      {
        id: 'ec14a422', engine_type: 'native',
        path: '/home/example/.local/share/local-model-manager/engines/tachyon-serve/ec14a422',
        installed: true,
        instances: [{ source: 'external', pid: 990915, port: 18099, healthy: true, alive: true }],
      },
    ],
  },
  capacity: {
    memory: { total_gib: 121.7, used_gib: 13.3, available_gib: 108.4, swap_total_gib: 80, swap_free_gib: 65.6 },
    safety_margin_gib: 9.7,
    resident_models: [{ card: 'ornith15-35b-a3b-nvfp4', measured_footprint_gib: 33 }],
  },
};

function liveFetch() {
  const socket = process.env.CMSD_SOCKET;
  if (!socket) return null;
  try {
    const get = (p) => JSON.parse(execSync(
      `curl -fsS --max-time 3 --unix-socket "${socket}" "http://localhost${p}"`,
      { encoding: 'utf8' }
    ));
    return {
      status: get('/v1/status'),
      models: get('/v1/models'),
      engines: get('/v1/engines'),
      capacity: get('/v1/capacity'),
    };
  } catch (error) {
    console.error(`CMSD_SOCKET 抓现场失败，退回验收样本：${error.message}`);
    return null;
  }
}
const CMS_DATA = liveFetch() || FIXTURE;
const CMS_UNAVAILABLE = { available: false, reason: 'unsupported_platform' };

function injectSource(cmsData, opts = {}) {
  const ok = (data) => ({ available: true, data });
  // cmsData 为 null = 能力缺席场景：四个命令统一返回 unavailable
  const status = cmsData ? ok(cmsData.status || null) : CMS_UNAVAILABLE;
  const models = cmsData ? ok(cmsData.models || null) : CMS_UNAVAILABLE;
  const engines = cmsData ? ok(cmsData.engines || null) : CMS_UNAVAILABLE;
  const capacity = cmsData ? ok(cmsData.capacity || null) : CMS_UNAVAILABLE;
  // 场景装配开关：④ 换胎任务（withSwitchTask）；⑤ 活跃指针翻转（flipActiveAfter）；
  // ⑥ 启停任务不弹窗（withEnableTask）；⑦ 无接入行的 cmsd 模型（orphanActive，
  // 注入一张已保存在列表之外的「在跑且健康」卡，活跃指针翻向它）
  const orphan = opts.orphanActive || null;
  const flipSetup = opts.flipActiveAfter
    ? `window.__CMS_TEST__.flipActiveAfter = ${Number(opts.flipActiveAfter)};
       window.__CMS_TEST__.flipActiveTo = ${JSON.stringify((orphan && orphan.card) || 'qwen38-27b-nvfp4-tachyon')};`
    : '';
  const tasksCase = opts.withSwitchTask
    ? `case 'cms_tasks': {
          // The guidance panel may read tasks before the deferred observer.
          // Hold backend state until the test has observed the running UI;
          // read-only queries must never complete the operation themselves.
          window.__CMS_TEST__.switchPolls = (window.__CMS_TEST__.switchPolls || 0) + 1;
          var running = window.__CMS_TEST__.completeSwitch !== true;
          return Promise.resolve({ available: true, data: { tasks: [{
            id: 't-sw-1', kind: 'switch', card: 'qwen38-27b-nvfp4-tachyon',
            status: running ? 'running' : 'succeeded',
            created_at: Math.floor(Date.now() / 1000) - 8,
            last_event: { message: '启动 qwen38-27b-nvfp4-tachyon（权重加载分钟级）' },
            result: '已切换到 qwen38-27b-nvfp4-tachyon，全程 24s',
          }] } });
        }`
    : opts.withEnableTask
      ? `case 'cms_tasks': return Promise.resolve({ available: true, data: { tasks: [{
            // 商店/MCP 启停：一直 running 的 enable——不应弹维护窗、不应触发选择跟随
            id: 't-en-1', kind: 'enable', card: 'qwen38-27b-nvfp4-tachyon',
            status: 'running', created_at: Math.floor(Date.now() / 1000) - 5,
            last_event: { message: '等待服务就绪…' },
          }] } });`
      : `case 'cms_tasks': return Promise.resolve({ available: true, data: { tasks: [] } });`;
  return `(function(){
    window.__TAURI_EVENT_HANDLERS__ = {};
    window.__CMS_TEST__ = { savedModelCalls: [], setActiveCalls: [] };
    ${flipSetup}
    var settings = {
      theme: 'liquid-light', color_scheme: 'light', language: 'zh-Hans',
      memory_enabled: true,
      notifications: { enabled: true, task_completed: true },
      pet: { enabled: false },
      search: { provider: 'bing', enabled_providers: ['bing'], api_key: null, credentials: {} },
      advanced: {},
    };
    var models = [{
      id: 'local-ornith', name: '本地 Ornith', preset: 'local_vllm',
      model: 'Ornith-1.5-35B-A3B-NVFP4', base_url: 'http://127.0.0.1:8000/v1',
      has_secret: false, credential_state: 'missing',
    }, {
      // 存量坏行（真机实测：直连引擎端口 + 前 cmsd 时代乱码名）
      id: 'm_mv3avlzc58b0a', name: '本地 · Qwen3.8-27B (NVFP4 · 驭光)',
      model: 'qwen3.8-27b-nvfp4--nvfp4', base_url: 'http://127.0.0.1:18081/v1',
      preset: 'openai_compatible', provider_kind: 'custom',
      has_secret: true, credential_state: 'configured',
    }, {
      // 真名接入行（带卡片身份标记；设备换胎不会替换管理连接）
      id: 'm_qwen_real', name: '本地 · Qwen3.8-27B (NVFP4 · 驭光)',
      model: 'Qwen3.8-27B-NVFP4', base_url: 'http://127.0.0.1:8000/v1',
      preset: 'openai_compatible', provider_kind: 'custom',
      has_secret: true, credential_state: 'configured',
      store_card_id: 'qwen38-27b-nvfp4-tachyon',
    }, {
      // 撞名诱饵：sglang 卡同供同一模型 id 且也指网关——必须输给带标记的
      id: 'm_decoy', name: '本地 · Qwen3.8-27B (NVFP4 · SGLang)',
      model: 'Qwen3.8-27B-NVFP4', base_url: 'http://127.0.0.1:8000/v1',
      preset: 'openai_compatible', provider_kind: 'custom',
      has_secret: true, credential_state: 'configured',
    }];
    window.__CMS_TEST__.savedModelCalls = [];
    window.__CMS_TEST__.setActiveCalls = [];
    function invoke(cmd, args) {
      switch (cmd) {
        case 'get_platform_capabilities': return Promise.resolve({
          os: 'linux', showMegacubeSite: true, showSuperPermissionSettings: false,
          usesBundledDependencyInstaller: true, taskCompletionNotificationsDefault: true,
          voiceShortcutNative: false,
        });
        case 'get_settings': return Promise.resolve(settings);
        case 'update_settings':
          settings = Object.assign({}, settings, args.patch || {});
          return Promise.resolve(settings);
        case 'list_models': return Promise.resolve({ models: models.slice(), active_model_id: 'local-ornith' });
        case 'get_effective_model_config': return Promise.resolve({ model: 'Ornith-1.5-35B-A3B-NVFP4', base_url: 'http://127.0.0.1:8000/v1', preset: 'local_vllm', api_key_set: false });
        case 'get_behavior_telemetry_status': return Promise.resolve({ enabled: true, upload_endpoint: null });
        case 'model_store_availability': return Promise.resolve({ available: true, reason: null });
        case 'model_store_library': return Promise.resolve({
          models: [{
            card_id: 'qwen38-27b-nvfp4-tachyon', card_name: 'Qwen3.8-27B (NVFP4 · 驭光)',
            model_name: 'qwen3.8-27b-nvfp4--nvfp4', status: 'deployed', running: true,
            engine_name: '驭光', engine_version: null, org: 'Qwen', description: null,
            quantization: 'NVFP4', size_bytes: 21944459776,
            endpoint_url: 'http://127.0.0.1:8000/v1/chat/completions',
            endpoint_model: 'Qwen3.8-27B-NVFP4',
            profiles: [], cmsd: { status: 'running', healthy: true, restartCount: 1, degraded: false, port: 18081 },
          }],
          corrected: [], cmsd_corrected: [],
        });
        case 'model_store_cards': return Promise.resolve(${JSON.stringify(
          // 场景⑦b：引擎自报路径形 id 时，显示名回退到商店目录的卡片名
          [{ id: 'mtp-like-card', name: 'Qwen3.8-27B (NVFP4 + Native MTP)' }]
        )});
        case 'model_store_active_runs': return Promise.resolve({ runs: [] });
        case 'save_model':
          window.__CMS_TEST__.savedModelCalls.push(args.model);
          return Promise.resolve(null);
        case 'set_active_model':
          window.__CMS_TEST__.setActiveCalls.push(args.id);
          return Promise.resolve(null);
        ${tasksCase}
        case 'cms_status': {
          var statusData = JSON.parse(JSON.stringify(${JSON.stringify(status)}));
          if (window.__CMS_TEST__.flipActiveAfter) {
            var pollN = (window.__CMS_TEST__.statusPolls = (window.__CMS_TEST__.statusPolls || 0) + 1);
            if (pollN >= window.__CMS_TEST__.flipActiveAfter) {
              statusData.data.active_model = window.__CMS_TEST__.flipActiveTo || 'qwen38-27b-nvfp4-tachyon';
            }
          }
          return Promise.resolve(statusData);
        }
        case 'cms_models': {
          var modelsPayload = JSON.parse(JSON.stringify(${JSON.stringify(models)}));
          ${orphan ? `if (modelsPayload.data && Array.isArray(modelsPayload.data.models)) modelsPayload.data.models.push(${JSON.stringify(orphan)});` : ''}
          return Promise.resolve(modelsPayload);
        }
        case 'cms_service_status': return Promise.resolve({ supported: false });
        case 'cms_device_status': return Promise.resolve({ supported: false });
        case 'cms_engines': return Promise.resolve(${JSON.stringify(engines)});
        case 'cms_capacity': return Promise.resolve(${JSON.stringify(capacity)});
        default: return Promise.resolve(null);
      }
    }
    window.__TAURI__ = {
      core: { invoke: invoke },
      event: {
        listen: function (name, handler) {
          (window.__TAURI_EVENT_HANDLERS__[name] || (window.__TAURI_EVENT_HANDLERS__[name] = [])).push(handler);
          return Promise.resolve(function () {});
        },
        emit: function () { return Promise.resolve(); },
      },
      window: {
        getCurrentWindow: function () {
          return {
            minimize: function () {}, maximize: function () {}, close: function () {},
            toggleMaximize: function () {}, isMaximized: function () { return Promise.resolve(false); },
            onResized: function () { return Promise.resolve(function () {}); },
            startDragging: function () {},
          };
        },
      },
      dialog: { open: function () { return Promise.resolve(null); } },
    };
  })();`;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function bootApp(browser, url, cmsData, opts = {}) {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('console', (message) => { if (message.type() === 'error') errors.push('console:' + message.text()); });
  await page.evaluateOnNewDocument(injectSource(cmsData, opts));
  await page.setViewport({ width: 1440, height: 1000 });
  await page.goto(url, { waitUntil: 'networkidle0' });
  await page.waitForFunction(
    () => window.TauriBridge && document.querySelector('[data-testid="app-root"]'),
    { timeout: 20000 }
  );
  await sleep(1000);
  return { page, errors };
}

async function openDeviceManagement(page) {
  await page.evaluate(() => {
    const button = [...document.querySelectorAll('[data-nav="monitor"]')].find(button => button.offsetParent !== null);
    if (button) button.click();
  });
  await page.waitForFunction(
    () => document.querySelector('[data-testid="app-root"]')?.getAttribute('data-current-view') === 'monitor',
    { timeout: 8000 }
  );
  await sleep(600);
}

(async () => {
  const results = [];
  const rec = (name, pass, detail = '') => {
    results.push({ name, pass });
    console.log(`${pass ? '✅' : '❌'} ${name}${detail ? '  ' + detail : ''}`);
  };

  const { url } = await startUiTestServer();
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: 'new',
    args: ['--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check'],
  });

  try {
    // 场景①②：能力可用 → 页签出现，面板渲染样本或只读现场数据
    const { page, errors } = await bootApp(browser, url, CMS_DATA);
    await openDeviceManagement(page);
    await page.waitForFunction(() => [...document.querySelectorAll('[role="tab"]')].some(button => button.textContent.trim() === 'MegaCube'));
    const navVisible = await page.evaluate(() =>
      [...document.querySelectorAll('[role="tab"]')].some(
        (button) => (button.textContent || '').trim() === 'MegaCube'
      ));
    rec('① 管理能力可用时运行状态页出现 MegaCube 页签', navVisible);
    await page.evaluate(() => [...document.querySelectorAll('[role="tab"]')].find(button => button.textContent.trim() === 'MegaCube').click());
    const firstModel = CMS_DATA.models.models[0] || {};
    await page.waitForFunction(
      (id) => document.body.innerText.includes(id),
      { timeout: 8000 },
      firstModel.model_id || 'Ornith-1.5-35B-A3B-NVFP4'
    );
    const collapsedText = await page.evaluate(() => document.body.innerText);
    rec('① 高级技术信息默认折叠', !(CMS_DATA.engines.engines || []).some(e => collapsedText.includes(e.id)));
    await page.click('[data-testid="cms-advanced"] > summary');
    await page.$$eval('[data-testid="cms-engine-row"] details', nodes => nodes.forEach(node => { node.open = true; }));
    const panelText = await page.evaluate(() => document.body.innerText);
    // 断言由数据驱动（fixture 与现场两种数据源同口径）
    const statusLabels = { running: '运行中', starting: '启动中', stopped: '已停止', degraded: '已降级' };
    const expectStatus = statusLabels[firstModel.status] || firstModel.status;
    rec('① 面板渲染模型与状态', panelText.includes(firstModel.model_id) && panelText.includes(expectStatus));
    const engineIdsOk = (CMS_DATA.engines.engines || []).every((e) => panelText.includes(e.id));
    const hasExternal = (CMS_DATA.engines.engines || []).some((e) =>
      (e.instances || []).some((i) => i.source === 'external'));
    const externalOk = !hasExternal || panelText.includes('外部资源（只读）');
    rec('① 面板渲染引擎注册表（含外部收编）', engineIdsOk && externalOk);
    const totalGiB = Number(CMS_DATA.capacity.memory.total_gib).toFixed(1) + ' GiB';
    const availGiB = Number(CMS_DATA.capacity.memory.available_gib).toFixed(1) + ' GiB';
    rec('① 面板渲染容量数字', panelText.includes(totalGiB) && panelText.includes(availGiB));
    rec('① 页面无未捕获错误', errors.length === 0, errors.slice(0, 2).join(' | '));
    await page.click('[data-testid="cms-advanced"] > summary');
    await page.screenshot({ path: SCREENSHOT });
    console.log(`截图: ${SCREENSHOT}`);
    await page.close();

    // 场景②：存量路由由后端按已确认的商店绑定迁移，前端不按名字改写用户设置。
    // 用干净的新页面跑（设置页的残留状态不把场景搅浑）。
    const storeBoot = await bootApp(browser, url, CMS_DATA);
    const page2 = storeBoot.page;
    await page2.waitForSelector('[data-testid="nav-model-store"]', { timeout: 10000 });
    // testid 有桌面/移动两份（隐藏的 offsetParent 为 null），点可见的那个
    await page2.evaluate(() => {
      const wraps = [...document.querySelectorAll('[data-testid="nav-model-store"]')];
      const visible = wraps.find((w) => w.offsetParent !== null) || wraps[0];
      // NavItem 渲的是带 onClick 的 div（不是 button），点 wrapper 的第一个孩子
      const target = visible?.querySelector('button')
        || visible?.firstElementChild
        || visible;
      target.click();
    });
    try {
      await page2.waitForFunction(() => document.body.innerText.includes('本机已有'), { timeout: 10000 });
    } catch (e) {
      const bodyText = await page2.evaluate(() => document.body.innerText.slice(0, 400)).catch(() => '<页面已死>');
      console.error('商店页未出现「本机已有」。页面文本：', bodyText.replace(/\n/g, ' | '));
      console.error('页面错误：', storeBoot.errors.slice(0, 3).join(' ;; '));
      throw e;
    }
    await page2.waitForFunction(() => document.querySelector('[data-testid="model-store-row"]'));
    const savedModelCalls = await page2.evaluate(() => window.__CMS_TEST__.savedModelCalls);
    rec('② 商店保留未绑定的用户路由，不按模型名自动改写', savedModelCalls.length === 0);
    await page2.close();

    // 场景④：换胎维护窗 + 完成后保留管理对话连接
    const sw = await bootApp(browser, url, CMS_DATA, { withSwitchTask: true });
    const page4 = sw.page;
    try {
      await page4.waitForFunction(() => {
        const m = document.querySelector('[data-testid="cmsd-task-modal"]');
        return m && m.innerText.includes('正在切换到');
      }, { timeout: 15000 });
    } catch (error) {
      console.error('换胎维护窗未出现：', JSON.stringify(await page4.evaluate(() => ({
        taskPolls: window.__CMS_TEST__.switchPolls,
        modal: document.querySelector('[data-testid="cmsd-task-modal"]')?.innerText,
        body: document.body.innerText.slice(-1000),
      }))), sw.errors);
      throw error;
    }
    rec('④ 换胎进行中弹维护窗', true);
    await page4.evaluate(() => { window.__CMS_TEST__.completeSwitch = true; });
    await page4.waitForSelector('[data-testid="cmsd-task-modal-close"]', { timeout: 15000 });
    rec('④ 换胎完成保留管理对话连接', await page4.evaluate(() => window.__CMS_TEST__.setActiveCalls.length === 0));
    const settledText = await page4.evaluate(() =>
      document.querySelector('[data-testid="cmsd-task-modal"]')?.innerText || '');
    rec('④ 终结态给出结果与确认键', settledText.includes('已完成') && settledText.includes('已切换到 qwen38-27b-nvfp4-tachyon'));
    await page4.click('[data-testid="cmsd-task-modal-close"]');
    await page4.close();

    // 场景⑤：仅挪设备默认（无任务），保留管理连接且维护窗不出现
    const follow = await bootApp(browser, url, CMS_DATA, { flipActiveAfter: 3 });
    const page5 = follow.page;
    await page5.waitForFunction(() =>
      window.__CMS_TEST__.statusPolls >= 3, { timeout: 15000 });
    rec('⑤ 设备默认变化不改变当前管理对话模型', await page5.evaluate(() => window.__CMS_TEST__.setActiveCalls.length === 0));
    await sleep(1000);
    const noModal = await page5.evaluate(() => !document.querySelector('[data-testid="cmsd-task-modal"]'));
    rec('⑤ 仅挪默认时不弹维护窗（无任务）', noModal);
    await page5.close();

    // 场景⑥：商店/MCP 启停不打扰——running 的 enable 任务既不弹维护窗，
    // 也不触发模型选择跟随（进度由商店 run 日志 / MCP 应答各自呈现）
    const en = await bootApp(browser, url, CMS_DATA, { withEnableTask: true });
    const page6 = en.page;
    await sleep(7000); // 覆盖两轮以上任务轮询（3s 周期），排除「还没来得及弹」
    const enNoModal = await page6.evaluate(() => !document.querySelector('[data-testid="cmsd-task-modal"]'));
    rec('⑥ 启停任务进行中不弹维护窗', enNoModal);
    const enNoFollow = await page6.evaluate(() => window.__CMS_TEST__.setActiveCalls.length === 0);
    rec('⑥ 启停任务不触发模型选择跟随', enNoFollow);
    rec('⑥ 页面无未捕获错误', en.errors.length === 0, en.errors.slice(0, 2).join(' | '));
    await page6.close();

    // 场景⑦：cmsd 侧装好的健康模型没有接入行，设备默认翻向它时
    // 不自动建行或替换管理连接
    const orphanCard = {
      card: 'test9b-nvfp4-tachyon', model_id: 'Test-9B-NVFP4', status: 'running',
      healthy: true, port: 18082, restart_count: 0, weights_bytes: 1000000,
    };
    const auto = await bootApp(browser, url, CMS_DATA, { flipActiveAfter: 3, orphanActive: orphanCard });
    const page7 = auto.page;
    await page7.waitForFunction(() => window.__CMS_TEST__.statusPolls >= 3, { timeout: 15000 });
    rec('⑦ 后台健康模型不自动登记为管理模型', await page7.evaluate(() => !window.__CMS_TEST__.savedModelCalls.some((m) => m.store_card_id === 'test9b-nvfp4-tachyon')));
    rec('⑦ 后台健康模型不替换管理连接', await page7.evaluate(() => window.__CMS_TEST__.setActiveCalls.length === 0));
    rec('⑦ 页面无未捕获错误', auto.errors.length === 0, auto.errors.slice(0, 2).join(' | '));
    await page7.close();

    // 场景⑦b：引擎自报路径形 id（真机 mtp 卡实测报 /models/target），
    // 同样不因设备默认变化而自动晋升为管理连接
    const mtpLike = {
      card: 'mtp-like-card', model_id: '/models/target', status: 'running',
      healthy: true, port: 30000, restart_count: 0, weights_bytes: 1000000,
    };
    const auto2 = await bootApp(browser, url, CMS_DATA, { flipActiveAfter: 3, orphanActive: mtpLike });
    const page7b = auto2.page;
    await page7b.waitForFunction(() => window.__CMS_TEST__.statusPolls >= 3, { timeout: 15000 });
    rec('⑦b 路径形模型身份也不自动晋升管理连接', await page7b.evaluate(() => !window.__CMS_TEST__.savedModelCalls.some((m) => m.store_card_id === 'mtp-like-card')));
    rec('⑦b 页面无未捕获错误', auto2.errors.length === 0, auto2.errors.slice(0, 2).join(' | '));
    await page7b.close();

    // 场景③：能力缺席 → 分区不渲染（无感缺席）
    const negative = await bootApp(browser, url, null);
    await openDeviceManagement(negative.page);
    const navHidden = await negative.page.evaluate(() =>
      ![...document.querySelectorAll('[role="tab"]')].some(
        (button) => (button.textContent || '').trim() === 'MegaCube'
      ));
    rec('③ 全部能力明确不支持时保留运行状态、不显示管理页签', navHidden);
    rec('③ 缺席场景页面无未捕获错误', negative.errors.length === 0, negative.errors.slice(0, 2).join(' | '));
    await negative.page.close();
  } finally {
    await browser.close();
  }

  const failed = results.filter((r) => !r.pass);
  console.log(failed.length === 0 ? `\n全部通过（${results.length} 项）` : `\n${failed.length} 项失败`);
  await finishFixtures(failed.length === 0 ? 0 : 1);
})().catch((error) => {
  console.error('FAIL:', error.message);
  console.error(error.stack ? error.stack.split('\n').slice(0, 4).join('\n') : '');
  void finishFixtures(1);
});
