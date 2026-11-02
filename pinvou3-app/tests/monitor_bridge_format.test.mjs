import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, '..');
const read = (...parts) => fs.readFileSync(path.join(root, ...parts), 'utf8');

// 两份 bridge(tauri 桌面 / web)的 TTFT/TPS 无数据回退必须统一为「—」,
// 与 MonitorView.jsx 的 MonitorMetricCard(值为 — 时不渲染 unit)配套。
// 防止某一侧单独回退成字面 "0 s"/"0 tok/s" 造成桌面/网页显示漂移。
// #701 T0-a/C 壳化 + T4a web 大 C 面壳化：两侧 pollMonitor 均已委托
// helpers（格式化本体在共享基座），两 lane 都钉共享基座（上游同形）。
const bridges = {
  tauri: read('src', 'shared', 'bridge-shared-helpers.js'),
  web: read('src', 'shared', 'bridge-shared-helpers.js'),
};
// T4a：web pollMonitor 壳必须保留（转发面结构钉）。
assert.match(
  read('src', 'platform', 'web', 'bridge.js'),
  /async function pollMonitor\(\) \{ return pinvouSharedweb\(\)\.pollMonitor\(\); \}/,
  'web pollMonitor must forward to the shared cluster',
);

// pollBackendStatus 仍是两条 lane 各自声明的手抄体(round-9 review R2:仅
// 对齐语义,未单源)。钉住在飞守卫必须同时存在于两侧 —— get_backend_status
// 挂起时,10 秒间隔的重入会在慢响应上叠轮次;守卫缺失曾只发生在桌面侧。
const pollBackendStatusGuard = /backendStatusPollInFlight = true/;
assert.match(
  read('src', 'platform', 'tauri', 'bridge', 'monitor.js'),
  pollBackendStatusGuard,
  'desktop pollBackendStatus must keep the in-flight reentrancy guard',
);
assert.match(
  read('src', 'platform', 'web', 'bridge.js'),
  pollBackendStatusGuard,
  'web pollBackendStatus must keep the in-flight reentrancy guard',
);
// 仅钉赋值不够：删掉早退会退回慢响应上的叠轮询（原 bug），删掉 finally 重置
// 会在首轮之后永久闩死（live dot 冻结，比原 bug 更糟）。两份手抄体单源之前，
// 三个片段都是契约（评审变异实测：早退/重置删除在仅钉赋值时均保持绿）。
const pollBackendStatusEarlyReturn = /if \(backendStatusPollInFlight\) return;/;
const pollBackendStatusReset = /finally \{\s*backendStatusPollInFlight = false;/;
for (const lane of [
  ['src', 'platform', 'tauri', 'bridge', 'monitor.js'],
  ['src', 'platform', 'web', 'bridge.js'],
]) {
  assert.match(
    read(...lane),
    pollBackendStatusEarlyReturn,
    'pollBackendStatus must keep the in-flight early return (' + lane.join('/') + ')',
  );
  assert.match(
    read(...lane),
    pollBackendStatusReset,
    'pollBackendStatus must keep the in-flight reset (' + lane.join('/') + ')',
  );
}

const TTFT_FALLBACK_PATTERN =
  /vllmTtft: sadj && sadj\.ttft_count > 0\s*\?\s*\(sadj\.ttft_sum_s \/ sadj\.ttft_count\)\.toFixed\(2\) \+ " s" : "—"/;
const TPS_FALLBACK_PATTERN =
  /vllmTps: sadj && sadj\.tps_time_s > 0\s*\?\s*\(sadj\.tps_tokens \/ sadj\.tps_time_s\)\.toFixed\(1\) \+ " tok\/s" : "—"/;

for (const [name, source] of Object.entries(bridges)) {
  assert.match(
    source,
    TTFT_FALLBACK_PATTERN,
    `${name} bridge TTFT 无数据必须回退为 —`,
  );
  assert.match(
    source,
    TPS_FALLBACK_PATTERN,
    `${name} bridge TPS 无数据必须回退为 —`,
  );
  assert.doesNotMatch(
    source,
    /vllmTtft:[\s\S]*?toFixed\(2\) \+ " s" : "0 s"/,
    `${name} bridge TTFT 无数据不得回退为字面 "0 s"`,
  );
  assert.doesNotMatch(
    source,
    /vllmTps:[\s\S]*?toFixed\(1\) \+ " tok\/s" : "0 tok\/s"/,
    `${name} bridge TPS 无数据不得回退为字面 "0 tok/s"`,
  );
}

// Exercise the shipped shared poller through both lane factories. Even a
// stale/contradictory backend snapshot must not paint an invalid target online.
const bridgeWindow = {};
vm.runInNewContext(bridges.tauri, { window: bridgeWindow, console });
for (const lane of ['tauriMonitor', 'web']) {
  for (const kind of ['local', 'remote', 'invalid', 'unknown', undefined]) {
    for (const status of ['ready', 'busy', 'offline', 'mismatch']) {
      for (const health of ['verified', 'unverified', 'auth_failed', undefined]) {
        const state = { settings: {}, tokens: {}, monitor: null, monitorError: null };
        const inFlight = { value: false };
        const poller = bridgeWindow.PinvouBridgeShared.create(lane, {
          state,
          sessionStates: {},
          invoke: async () => ({
            vllm: { target_kind: kind, status, health_status: health, metrics_applicable: false },
          }),
          notify() {},
          bt: (key) => key,
          monitorBaseline: { value: null },
          clearMonitorBaseline() {},
          monitorIntervalId: { value: null },
          gpuUtilHistory: { value: [] },
          monitorPollInFlight: inFlight,
        });
        await poller.pollMonitor();
        const label = `${lane}: ${kind}/${status}/${health}`;
        assert.equal(state.monitorError, null, `${label}: shared poller succeeds`);
        assert.equal(inFlight.value, false, `${label}: in-flight guard resets`);
        assert.equal(
          state.monitor._fmt.vllmOnline,
          (kind === 'local' || kind === 'remote')
            && (status === 'ready' || status === 'busy')
            && (health === 'verified' || health === undefined),
          `${label}: online needs a valid target and the existing verified ready/busy contract`,
        );
      }
    }
  }
}

console.log('monitor bridge 无数据 TTFT/TPS fallback 在 tauri/web 两份 bridge 中一致为 —');
