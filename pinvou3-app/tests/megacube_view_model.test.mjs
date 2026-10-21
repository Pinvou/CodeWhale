// MegaCube 面板：视图模型纯逻辑 + 集成点源码契约。
// 数据样本是 2026-10-12 真机 cmsd 的实际应答（V2/V7 验收抓取）。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

import {
  coarseUptime,
  formatEpoch,
  formatGiB,
  normalizeCapacity,
  normalizeEngines,
  normalizeModels,
  normalizeStatus,
} from '../src/features/megacube/cms-view-model.js';

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..');

// ── 真机样本 ────────────────────────────────────────────────
const STATUS_SAMPLE = {
  active_model: 'ornith15-35b-a3b-nvfp4',
  degraded: [],
  engines: { installed: 2, external_processes: 0 },
  gateway: '127.0.0.1:8000',
  service: 'cmsd',
  started_at: 1791813210,
  systemd_mode: 'user',
  uptime_s: 4520,
  version: '0.1.0',
};

const MODELS_SAMPLE = {
  active_model: 'ornith15-35b-a3b-nvfp4',
  models: [
    {
      card: 'ornith15-35b-a3b-nvfp4',
      model_id: 'Ornith-1.5-35B-A3B-NVFP4',
      status: 'running',
      healthy: true,
      port: 18080,
      restart_count: 3,
      last_restart_at: 1791814956,
      measured_footprint_gib: 33,
      boot_enabled: true,
      weights_bytes: 22892664832,
    },
    {
      card: 'qwen38-27b-nvfp4-tachyon',
      model_id: 'Qwen3.8-27B-NVFP4',
      status: 'stopped',
      weights_bytes: 21944459776,
    },
  ],
};

const ENGINES_SAMPLE = {
  engines: [
    {
      id: '0f8a4a7c',
      engine_type: 'native',
      path: '/home/example/.local/share/local-model-manager/engines/tachyon-serve/0f8a4a7c',
      installed: true,
      instances: [
        { source: 'managed', pid: 1026973, port: 18080, healthy: true },
      ],
    },
    {
      id: 'ec14a422',
      engine_type: 'native',
      path: '/home/example/.local/share/local-model-manager/engines/tachyon-serve/ec14a422',
      installed: true,
      instances: [
        { source: 'external', pid: 990915, port: 18099, healthy: true, alive: true, model_id: 'Qwen3.8-27B-NVFP4' },
      ],
    },
  ],
};

const CAPACITY_SAMPLE = {
  memory: { total_gib: 121.7, used_gib: 13.3, available_gib: 108.4, swap_total_gib: 80, swap_free_gib: 65.6 },
  safety_margin_gib: 9.7,
  resident_models: [{ card: 'ornith15-35b-a3b-nvfp4', measured_footprint_gib: 33 }],
};

// ── 视图模型 ────────────────────────────────────────────────
test('normalizeStatus：真机样本各字段', () => {
  const s = normalizeStatus(STATUS_SAMPLE);
  assert.equal(s.version, '0.1.0');
  assert.deepEqual(s.uptime, { value: 1, unit: 'h' });
  assert.equal(s.gateway, '127.0.0.1:8000');
  assert.equal(s.activeModel, 'ornith15-35b-a3b-nvfp4');
  assert.deepEqual(s.degraded, []);
});

test('normalizeStatus：degraded 列表透传', () => {
  const s = normalizeStatus({
    ...STATUS_SAMPLE,
    degraded: [{ card: '_failtest', reason: '5 分钟内启动失败 3 次，systemd 已熔断' }],
  });
  assert.equal(s.degraded.length, 1);
  assert.equal(s.degraded[0].card, '_failtest');
  assert.match(s.degraded[0].reason, /熔断/);
});

test('normalizeStatus：坏输入不炸', () => {
  assert.equal(normalizeStatus(null), null);
  assert.equal(normalizeStatus(undefined), null);
  assert.equal(normalizeStatus('x'), null);
});

test('normalizeModels：运行态与停止态分列', () => {
  const { activeModel, models } = normalizeModels(MODELS_SAMPLE);
  assert.equal(activeModel, 'ornith15-35b-a3b-nvfp4');
  assert.equal(models.length, 2);
  const ornith = models[0];
  assert.equal(ornith.status, 'running');
  assert.equal(ornith.healthy, true);
  assert.equal(ornith.port, 18080);
  assert.equal(ornith.restartCount, 3);
  assert.ok(ornith.lastRestart); // epoch → MM-DD HH:MM
  assert.equal(ornith.footprintGiB, 33);
  assert.equal(ornith.bootEnabled, true);
  assert.ok(Math.abs(ornith.weightsGiB - 21.3) < 0.1);
  const qwen = models[1];
  assert.equal(qwen.status, 'stopped');
  assert.equal(qwen.healthy, null); // 未探测过 → null 而非 false
  assert.equal(qwen.port, null);
  assert.equal(qwen.bootEnabled, null);
});

test('model discovery and cleanup reservation do not become lifecycle authority', () => {
  const { models } = normalizeModels({ models: [
    { card: 'declared', managed: false, can_adopt: true, can_reserve_uninstall: true },
    { card: 'partial', managed: false, can_adopt: false, can_reserve_uninstall: true },
    { card: 'unknown' },
    { card: 'owned', managed: true, boot_enabled: true, recovery_owner: 'cmsd', restart_policy: 'no' },
  ] });
  assert.equal(models[0].managed, false);
  assert.equal(models[0].canAdopt, true);
  assert.equal(models[1].canReserveUninstall, true);
  assert.equal(models[1].canAdopt, false);
  assert.equal(models[2].managed, false);
  assert.equal(models[2].canAdopt, false);
  assert.equal(models[3].bootEnabled, true);
  assert.equal(models[3].recoveryOwner, 'cmsd');
});

test('normalizeEngines：managed 与 external 实例', () => {
  const engines = normalizeEngines(ENGINES_SAMPLE);
  assert.equal(engines.length, 2);
  assert.equal(engines[0].instances[0].source, 'managed');
  assert.equal(engines[0].instances[0].healthy, true);
  assert.equal(engines[1].instances[0].source, 'external');
  assert.equal(engines[1].instances[0].port, 18099);
  // 未知 source 一律归 managed 之外只剩 external：防御坏值
  const weird = normalizeEngines({ engines: [{ id: 'x', instances: [{ source: 'alien' }] }] });
  assert.equal(weird[0].instances[0].source, 'managed');
});

test('normalizeCapacity：容量与常驻模型', () => {
  const c = normalizeCapacity(CAPACITY_SAMPLE);
  assert.equal(c.totalGiB, 121.7);
  assert.equal(c.availableGiB, 108.4);
  assert.equal(c.marginGiB, 9.7);
  assert.equal(c.resident[0].measuredGiB, 33);
});

test('normalizeCapacity：空负载不炸', () => {
  const c = normalizeCapacity(null);
  assert.equal(c.totalGiB, null);
  assert.deepEqual(c.resident, []);
});

test('formatGiB / coarseUptime / formatEpoch 边界', () => {
  assert.equal(formatGiB(33), '33.0 GiB');
  assert.equal(formatGiB(undefined), '—');
  assert.deepEqual(coarseUptime(4520), { value: 1, unit: 'h' });
  assert.deepEqual(coarseUptime(300), { value: 5, unit: 'm' });
  assert.deepEqual(coarseUptime(30), { value: 30, unit: 's' });
  assert.equal(coarseUptime(-1), null);
  assert.match(formatEpoch(1791814956) || '', /^\d{2}-\d{2} \d{2}:\d{2}$/);
  assert.equal(formatEpoch(0), null);
});

// ── 集成点源码契约（无 DOM 环境，钉源码文本）────────────────
test('设备管理入口：面板迁入运行状态，旧设置跳转及商店恢复入口保留', () => {
  const read = file => fs.readFileSync(path.join(ROOT, file), 'utf8');
  const settings = read('src/features/settings/SettingsView.jsx');
  const view = read('src/features/megacube/DeviceManagementView.jsx');
  const app = read('src/app/main.jsx');
  const discovery = read('src/features/megacube/use-device-management.js');
  assert.match(discovery, /canInvoke\('cms_device_status'\)/, 'Web 权限核对必须使用真实设备查询命令');
  assert.doesNotMatch(settings, /<CmsSettingsPanel|<SectionButton id="megacube"/);
  assert.match(view, /<MonitorView theme=\{theme\} t=\{t\} bs=\{bs\} \/>/);
  assert.match(view, /<CmsSettingsPanel copy=\{t\.uiMegacube\} onOpenModelStore=\{onOpenModelStore\} \/>/);
  assert.match(app, /if \(section === 'megacube'\) \{\s*setMonitorSection\('management'\);\s*return navigateFromScheduledRun\('monitor'\);/);
});

test('cms-api.js：四个命令名与 Rust 侧一致', () => {
  const api = fs.readFileSync(
    path.join(ROOT, 'src/features/megacube/cms-api.js'),
    'utf8'
  );
  for (const cmd of ['cms_status', 'cms_models', 'cms_engines', 'cms_capacity']) {
    assert.ok(api.includes(`invokeTauri('${cmd}')`), `缺命令 ${cmd}`);
  }
});

test('面板文案只从 copy 取（不写死中文）', () => {
  const panel = fs.readFileSync(
    path.join(ROOT, 'src/features/megacube/CmsSettingsPanel.jsx'),
    'utf8'
  );
  // 面板消费 copy 命名空间
  assert.match(panel, /copy\.sectionStatus/);
  assert.match(panel, /copy\.sectionModels/);
  assert.match(panel, /copy\.sectionEngines/);
  assert.match(panel, /copy\.sectionCapacity/);
  // 活跃中文字面量禁令（i18n 公约）：面板 JSX 文本不得出现裸中文字符串
  const chineseLiterals = panel.match(/>[^<{}]*[一-龥][^<{}]*</g) || [];
  assert.deepEqual(chineseLiterals, [], `面板里有裸中文文案：${chineseLiterals.join(', ')}`);
});

// ── 商店行徽标源码契约（ModelRow.jsx）────────────────────────
test('商店行：cmsd 托管徽标与降级徽标的渲染契约', () => {
  const row = fs.readFileSync(
    path.join(ROOT, 'src/features/model-store/ModelRow.jsx'),
    'utf8'
  );
  assert.match(row, /data-testid="model-store-row-cmsd"/, 'cmsd 托管徽标必须有 testid');
  assert.match(row, /data-testid="model-store-row-cmsd-degraded"/, '降级徽标必须有 testid');
  assert.match(row, /model\.record\?\.cmsd/, '徽标读 record.cmsd（后端融合透出）');
  assert.match(row, /copy\.cmsdManaged/, '文案走 i18n copy');
  assert.match(row, /copy\.cmsdDegraded/);
  assert.match(row, /copy\.cmsdRestarts/);
  // 徽标不得影响无 cmsd 字段的普通行（条件渲染）
  assert.match(row, /\{model\.record\?\.cmsd && <div/, 'cmsd 字段缺席时不渲染');
});

test('委托与融合的文案键在三语字典里', () => {
  // 由 ui_language_coverage 兜底全量一致性；这里钉关键键存在于 zh
  const zh = fs.readFileSync(path.join(ROOT, 'src/shared/i18n/zh.js'), 'utf8');
  for (const key of ['cmsdManaged', 'cmsdManagedHint', 'cmsdDegraded', 'cmsdRestarts']) {
    assert.ok(zh.includes(`${key}:`), `zh 缺 ${key}`);
  }
});

// ── 换胎后自动切换的行选择（pickLinkedRowForCard）─────────────
import { pickLinkedRowForCard, cmsdCardRowName } from '../src/features/megacube/cms-view-model.js';

test('换胎选择：卡片身份标记精确命中，同名撞车不误伤', () => {
  const rows = [
    // 撞名诱饵：sglang 卡同供一个模型 id，也指网关（先于目标行出现）
    { id: 'decoy', model: 'Qwen3.8-27B-NVFP4', base_url: 'http://127.0.0.1:8000/v1' },
    { id: 'm_real', model: 'Qwen3.8-27B-NVFP4', base_url: 'http://127.0.0.1:8000/v1', store_card_id: 'qwen38-27b-nvfp4-tachyon' },
  ];
  const hit = pickLinkedRowForCard(rows, 'qwen38-27b-nvfp4-tachyon', 'Qwen3.8-27B-NVFP4', 'http://127.0.0.1:8000/v1');
  assert.equal(hit.id, 'm_real', '有标记的行必须赢过裸名字相同的诱饵');
});

test('换胎选择：无标记时按 网关+真名 > 别名存量 > 裸名字 回退', () => {
  const gw = 'http://127.0.0.1:8000/v1';
  const byGw = pickLinkedRowForCard(
    [{ id: 'a', model: 'M', base_url: 'http://127.0.0.1:9999/v1' }, { id: 'b', model: 'M', base_url: gw }],
    'card-x', 'M', gw);
  assert.equal(byGw.id, 'b');
  const alias = pickLinkedRowForCard(
    [{ id: 'a', model: 'megacube-default', base_url: gw }],
    'card-x', 'M', gw);
  assert.equal(alias.id, 'a', '别名存量行兜底');
  const bare = pickLinkedRowForCard([{ id: 'a', model: 'M', base_url: 'http://x/v1' }], 'c', 'M', null);
  assert.equal(bare.id, 'a', '裸名字最后兜底');
  assert.equal(pickLinkedRowForCard(null, 'c', 'M', gw), null);
});

// ── cmsd 托管模型的接入行显示名推导（cmsdCardRowName）─────────────

test('接入行显示名：驭光卡带引擎标签，认不出引擎就不贴', () => {
  assert.equal(
    cmsdCardRowName('qwen38-27b-nvfp4-tachyon', 'Qwen3.8-27B-NVFP4'),
    'Qwen3.8-27B-NVFP4 (驭光)');
  assert.equal(cmsdCardRowName('ornith15-35b-a3b-nvfp4', 'Ornith-1.5-35B-A3B-NVFP4'),
    'Ornith-1.5-35B-A3B-NVFP4', '无引擎尾缀不硬贴');
  assert.equal(cmsdCardRowName('somecard', ''), 'somecard', 'modelId 缺失退回卡名');
});

// ── 引擎自报 id 可否当显示名（modelIdNeedsNameFallback）─────────────
import { modelIdNeedsNameFallback } from '../src/features/megacube/cms-view-model.js';

test('路径形/空 id 不能当显示名（真机 mtp 卡报 /models/target）', () => {
  assert.equal(modelIdNeedsNameFallback('/models/target'), true);
  assert.equal(modelIdNeedsNameFallback(''), true);
  assert.equal(modelIdNeedsNameFallback(null), true);
  assert.equal(modelIdNeedsNameFallback('Qwen3.8-27B-NVFP4'), false);
});


test('container inventory keeps unassociated resources read-only and unknown health explicit', () => {
  const rows = normalizeEngines({engines: [], containers: [
    {name:'external-model',image:'image-a',card:null,running:true,host_port:30001},
    {name:'owned-model',image:'image-b',card:'owned-card',running:false,healthy:false},
  ]});
  assert.equal(rows.length, 2);
  assert.equal(rows[0].instances[0].source, 'external');
  assert.equal(rows[0].instances[0].healthy, null);
  assert.equal(rows[1].instances[0].source, 'managed');
  assert.equal(rows[1].instances[0].alive, false);
});

test('matching a card name does not turn an observed container into managed authority', () => {
  const rows = normalizeEngines({containers:[{name:'observed',card:'known-card',managed:false,running:true,health_model_id:'Shared model name'}]});
  assert.equal(rows[0].instances[0].source, 'external');
  assert.equal(rows[0].instances[0].modelId, 'Shared model name');
  assert.equal(normalizeModels({models:[{card:'known-card',managed:false,status:'running'}]}).models[0].managed, false);
});
