import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { JSDOM } from "jsdom";

const testDir = fileURLToPath(new URL(".", import.meta.url));
const statsHtml = readFileSync(join(testDir, "..", "web", "stats.html"), "utf8");

test("device details expose sortable conversation counts without encoding damage", () => {
  assert.match(statsHtml, /<span>今日对话次数<\/span>/);
  assert.match(statsHtml, /id="todayTurns"/);
  assert.match(statsHtml, /device-metrics/);
  assert.match(statsHtml, /data-sort="turns_7d"[^>]*>近 7 日对话/);
  assert.match(statsHtml, /data-sort="turns_today"[^>]*>今日对话/);
  assert.match(statsHtml, /device\.turns_today/);
  assert.match(statsHtml, /aria-sort/);
  assert.doesNotMatch(statsHtml, /�/);
});

const overview = {
  generated_at: Date.now(),
  counts: { online: 3, active_today: 3, active_7d: 3 },
  usage_trend: [{ day: "2026-08-10", active_devices: 3, turns: 12 }],
  active_versions: [],
};

const seenAt = Date.parse("2026-08-10T09:00:00+08:00");
const devices = [
  { device_id: "alpha", online: true, platform: "windows", first_seen_at: seenAt, last_active_at: seenAt, turns_7d: 4, turns_today: 1 },
  { device_id: "beta", online: true, platform: "linux", first_seen_at: seenAt, last_active_at: seenAt, turns_7d: 3, turns_today: 5 },
  { device_id: "gamma", online: true, platform: "macos", first_seen_at: seenAt, last_active_at: seenAt, turns_7d: 5, turns_today: 2 },
];

test("usage trend exposes exact values on hover and after a range rerender", async () => {
  const trendOverview = {
    ...overview,
    usage_trend: Array.from({ length: 8 }, (_, index) => ({
      day: `2026-08-${String(index + 3).padStart(2, "0")}`,
      active_devices: index === 7 ? 3 : index + 1,
      turns: index === 7 ? 12 : index + 2,
    })),
  };
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously",
    url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async (url) => ({
        ok: true,
        status: 200,
        json: async () => String(url).endsWith("/overview") ? trendOverview : { devices },
      });
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });

  try {
    await new Promise((resolve) => dom.window.setTimeout(resolve, 10));
    const { document } = dom.window;
    const svg = document.querySelector("#trendSvg");
    svg.getBoundingClientRect = () => ({ left: 0, width: 780 });
    let hitArea = document.querySelector("#trendHitArea");
    const tooltip = document.querySelector("#trendTooltip");
    hitArea.dispatchEvent(new dom.window.MouseEvent("pointermove", { clientX: 770, bubbles: true }));
    assert.equal(tooltip.hidden, false);
    assert.match(tooltip.textContent, /2026-08-10/);
    assert.match(tooltip.textContent, /活跃设备3 台/);
    assert.match(tooltip.textContent, /新增设备3 台/);
    assert.match(tooltip.textContent, /对话次数12 次/);
    assert.equal(document.querySelector("#trendHover").getAttribute("visibility"), "visible");
    assert.match(hitArea.getAttribute("aria-valuetext"), /活跃设备 3 台/);

    hitArea.dispatchEvent(new dom.window.MouseEvent("pointerleave", { bubbles: true }));
    assert.equal(tooltip.hidden, true);

    document.querySelector('[data-days="7"]').click();
    hitArea = document.querySelector("#trendHitArea");
    hitArea.dispatchEvent(new dom.window.MouseEvent("pointermove", { clientX: 35, bubbles: true }));
    assert.equal(tooltip.hidden, false);
    assert.match(tooltip.textContent, /2026-08-04/);
    assert.match(tooltip.textContent, /对话次数3 次/);
    assert.equal(hitArea.getAttribute("aria-valuemax"), "6");

    hitArea.dispatchEvent(new dom.window.KeyboardEvent("keydown", { key: "End", bubbles: true }));
    assert.match(tooltip.textContent, /2026-08-10/);
    assert.match(tooltip.textContent, /对话次数12 次/);
  } finally {
    dom.window.close();
  }
});

test("behavior analytics views render alongside the device dashboard", async () => {
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously",
    url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async (url) => ({
        ok: true,
        status: 200,
        json: async () => String(url).endsWith("/overview") ? overview : { devices },
      });
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });

  try {
    await new Promise((resolve) => dom.window.setTimeout(resolve, 10));
    const { document } = dom.window;
    const renderedDeviceIds = () => [...document.querySelectorAll("#deviceRows .device b")]
      .map((element) => element.textContent);
    assert.equal(document.querySelector("#todayTurns").textContent, "12");
    assert.deepEqual(renderedDeviceIds(), ["gamma", "alpha", "beta"]);
    document.querySelector('[data-sort="turns_7d"]').click();
    assert.deepEqual(renderedDeviceIds(), ["beta", "alpha", "gamma"]);
    document.querySelector('[data-sort="turns_today"]').click();
    assert.deepEqual(renderedDeviceIds(), ["beta", "gamma", "alpha"]);
    document.querySelector('[data-sort="turns_today"]').click();
    assert.deepEqual(renderedDeviceIds(), ["alpha", "gamma", "beta"]);
    assert.equal(document.querySelector('[data-view="behavior"]').textContent, "行为分析");
    assert.equal(document.querySelector('[data-view="commercial"]').textContent, "商业（未接入）");
    assert.ok(document.querySelector("#valueWevta"));
    assert.ok(document.querySelector("#retentionD30"));
    assert.ok(document.querySelector("#voiceFunnelRows"));
    assert.ok(document.querySelector("#activationFunnelRows"));
    assert.ok(document.querySelector("#modelRows"));
    assert.ok(document.querySelector("#versionQualityRows"));
    assert.ok(document.querySelector("#behaviorMeta"));
    assert.deepEqual(
      [...document.querySelectorAll(".behavior-range[data-behavior-section]")]
        .map(control => control.dataset.behaviorSection)
        .sort(),
      ["activation", "errors", "models", "scenes", "summary", "toolCalls", "toolInstalls", "versions", "voice"],
    );
    assert.match(statsHtml, /WEVTA 是可观测价值下界/);
    assert.match(statsHtml, /固定口径/);
    assert.match(statsHtml, /样本不足 30 时不展示百分比/);
    assert.doesNotMatch(statsHtml, /细分组安装数低于隐私阈值|细分组已抑制|隐私分组阈值/);
    assert.match(statsHtml, /主体：安装实例/);
    assert.match(statsHtml, /模型质量排行榜/);
    assert.match(statsHtml, /row\.success_confidence/);
    assert.match(statsHtml, /metrics\.explicit_value_confidence/);
    assert.match(statsHtml, /metrics\.turn_mismatch_value_signal_count/);
    assert.match(statsHtml, /explicit_value_signal_turn_mismatch/);
    document.querySelector('[data-view="behavior"]').click();
    assert.equal(document.querySelector("#behaviorView").classList.contains("hidden"), false);
    document.querySelector('[data-view="commercial"]').click();
    assert.equal(document.querySelector("#commercialView").classList.contains("hidden"), false);
    document.querySelector('[data-view="devices"]').click();
    assert.equal(document.querySelector("#devicesView").classList.contains("hidden"), false);
  } finally {
    dom.window.close();
  }
});

test("behavior analyst role never requests or exposes device-level views", async () => {
  const requested = [];
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously",
    url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async (url) => {
        requested.push(String(url));
        const path = String(url);
        const value = path.endsWith("/session")
          ? { role: "behavior_analyst" }
          : (path.includes("/behavior/overview") ? {
            generated_at: Date.now(),
            range: "30",
            data_status: "tracking_not_enabled",
            data_quality: { confidence: "low", warnings: ["explicit_value_signal_turn_mismatch"] },
            metrics: {
              turn_mismatch_value_signal_count: 3,
              task_first_output_p50_ms: null,
              task_first_output_p90_ms: null,
              task_total_duration_p50_ms: null,
              task_total_duration_p90_ms: null,
            },
          } : { range: "30", rows: [], funnel_rows: [], suppressed_group_count: 0 });
        return { ok: true, status: 200, json: async () => value };
      };
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });
  try {
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    const { document } = dom.window;
    assert.equal(document.querySelector('[data-view="devices"]').hidden, true);
    assert.equal(document.querySelector('[data-view="commercial"]').hidden, true);
    assert.equal(document.querySelector('[data-view="behavior"]').hidden, false);
    assert.equal(document.querySelector("#behaviorView").classList.contains("hidden"), false);
    assert.equal(requested.some(url => url.endsWith("/devices")), false);
    assert.equal(requested.some(url => url.endsWith("/overview") && !url.includes("/behavior/")), false);
    assert.match(document.querySelector("#taskLatency").textContent, /—/);
    assert.doesNotMatch(document.querySelector("#taskLatency").textContent, /0 ms/);
    assert.match(document.querySelector("#valueSignalQuality").textContent, /疑似回合标识不一致 3 条/);
    assert.match(document.querySelector("#behaviorQuality").textContent, /价值信号疑似关联了错误回合/);
  } finally {
    dom.window.close();
  }
});

test("behavior ranges refresh only the selected overview or table section", async () => {
  const requested = [];
  let failModelsToday = false;
  let mismatchScenesToday = false;
  const rangeValue = (range) => ({ today: 1, 7: 7, 30: 30 })[range] || 30;
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously",
    url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async (url) => {
        const path = String(url);
        requested.push(path);
        if (path.endsWith("/session")) {
          return { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) };
        }
        const parsed = new URL(path, "https://www.pinvou.com");
        const range = parsed.searchParams.get("range") || "30";
        const value = rangeValue(range);
        if (path.includes("/behavior/overview")) {
          return {
            ok: true,
            status: 200,
            json: async () => ({
              generated_at: Date.now(),
              range,
              data_status: "ready",
              data_quality: { confidence: "sufficient", warnings: [], task_sample_size: value },
              metrics: {
                task_success_count: value,
                weekly_successful_task_installations: 900,
                wevta_installations: 800,
                wevta_rate: 0.8,
                explicit_value_confidence: "sufficient",
                positive_feedback_installations: 700,
                response_copied_installations: 600,
                artifact_exported_installations: 500,
                negative_feedback_rate: 0.1,
              },
              retention: {
                d1: { eligible_installations: 100, retained_installations: 50, retention_rate: 0.5, confidence: "sufficient" },
                d7: { eligible_installations: 90, retained_installations: 36, retention_rate: 0.4, confidence: "sufficient" },
                d30: { eligible_installations: 80, retained_installations: 24, retention_rate: 0.3, confidence: "sufficient" },
              },
              funnels: {
                activation: { cohort_installations: value, confidence: "sufficient" },
                voice: { entry_clicked: value, confidence: "sufficient" },
              },
              error_categories: { [`error-${range}`]: value },
              version_quality: [{ app_version: `version-${range}`, platform: "windows", arch: "x64", sample_size: value, confidence: "sufficient" }],
            }),
          };
        }
        if (path.includes("/behavior/tools") && parsed.searchParams.get("type") === "call") {
          return { ok: true, status: 200, json: async () => ({ range, rows: [{ tool_name: `calls-${range}`, call_count: value }] }) };
        }
        if (path.includes("/behavior/tools") && parsed.searchParams.get("type") === "install") {
          return { ok: true, status: 200, json: async () => ({ range, funnel_rows: [{ tool_name: `installs-${range}`, impression_count: value }] }) };
        }
        if (path.includes("/behavior/scenes")) {
          const responseRange = mismatchScenesToday && range === "today" ? "30" : range;
          return { ok: true, status: 200, json: async () => ({ range: responseRange, rows: [{ scene_l1: `scenes-${range}`, impression_count: value, confidence: "sufficient" }] }) };
        }
        if (path.includes("/behavior/models")) {
          if (failModelsToday && range === "today") return { ok: false, status: 500, json: async () => ({}) };
          return { ok: true, status: 200, json: async () => ({ range, rows: [{ model_key: `models-${range}`, use_count: value, confidence: "sufficient" }] }) };
        }
        return { ok: true, status: 200, json: async () => ({}) };
      };
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });

  try {
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    const { document } = dom.window;
    const activeRange = section => document.querySelector(`[data-behavior-section="${section}"] button.active`)?.dataset.range;
    const rowText = id => document.querySelector(`#${id}`)?.textContent || "";

    assert.equal(document.querySelector("#taskSuccess").textContent, "30");
    assert.equal(document.querySelector('[data-behavior-section="summary"] [data-range="30"]').getAttribute("aria-pressed"), "true");
    assert.match(rowText("activationFunnelRows"), /^30/);
    assert.match(rowText("voiceFunnelRows"), /^30/);
    assert.match(rowText("toolCallRows"), /calls-30/);
    const fixedMetrics = () => [
      document.querySelector("#valueWsta").textContent,
      document.querySelector("#valueWevta").textContent,
      document.querySelector("#valueWevtaRate").textContent,
      document.querySelector("#valueSignals").textContent,
      document.querySelector("#retentionD1").textContent,
      document.querySelector("#retentionD7").textContent,
      document.querySelector("#retentionD30").textContent,
    ];
    const initialFixedMetrics = fixedMetrics();

    let requestStart = requested.length;
    document.querySelector('[data-behavior-section="toolCalls"] [data-range="today"]').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    assert.deepEqual(requested.slice(requestStart), ["/pinvou3/stats/api/behavior/tools?range=today&type=call"]);
    assert.equal(activeRange("toolCalls"), "today");
    assert.equal(document.querySelector('[data-behavior-section="toolCalls"] [data-range="today"]').getAttribute("aria-pressed"), "true");
    assert.match(rowText("toolCallRows"), /calls-today/);
    assert.equal(document.querySelector("#taskSuccess").textContent, "30");
    assert.match(rowText("activationFunnelRows"), /^30/);

    requestStart = requested.length;
    document.querySelector('[data-behavior-section="activation"] [data-range="7"]').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    assert.deepEqual(requested.slice(requestStart), ["/pinvou3/stats/api/behavior/overview?range=7"]);
    assert.equal(activeRange("activation"), "7");
    assert.match(rowText("activationFunnelRows"), /^7/);
    assert.match(rowText("voiceFunnelRows"), /^30/);
    assert.equal(document.querySelector("#taskSuccess").textContent, "30");

    requestStart = requested.length;
    document.querySelector('[data-behavior-section="summary"] [data-range="today"]').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    assert.deepEqual(requested.slice(requestStart), ["/pinvou3/stats/api/behavior/overview?range=today"]);
    assert.equal(activeRange("summary"), "today");
    assert.equal(document.querySelector("#taskSuccess").textContent, "1");
    assert.deepEqual(fixedMetrics(), initialFixedMetrics);
    assert.match(rowText("activationFunnelRows"), /^7/);
    assert.match(rowText("voiceFunnelRows"), /^30/);

    requestStart = requested.length;
    document.querySelector("#refreshButton").click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    const refreshRequests = requested.slice(requestStart);
    assert.equal(refreshRequests.filter(path => path.includes("/behavior/overview?range=today")).length, 1);
    assert.equal(refreshRequests.filter(path => path.includes("/behavior/overview?range=7")).length, 1);
    assert.equal(refreshRequests.filter(path => path.includes("/behavior/overview?range=30")).length, 1);
    assert.equal(activeRange("summary"), "today");
    assert.equal(activeRange("activation"), "7");
    assert.equal(activeRange("toolCalls"), "today");
    assert.match(rowText("toolCallRows"), /calls-today/);

    failModelsToday = true;
    requestStart = requested.length;
    document.querySelector('[data-behavior-section="models"] [data-range="today"]').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    assert.deepEqual(requested.slice(requestStart), ["/pinvou3/stats/api/behavior/models?range=today"]);
    assert.equal(activeRange("models"), "30");
    assert.match(rowText("modelRows"), /models-30/);
    assert.equal(document.querySelector('[data-behavior-section="models"]').getAttribute("aria-busy"), "false");

    mismatchScenesToday = true;
    requestStart = requested.length;
    document.querySelector('[data-behavior-section="scenes"] [data-range="today"]').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    assert.deepEqual(requested.slice(requestStart), ["/pinvou3/stats/api/behavior/scenes?range=today"]);
    assert.equal(activeRange("scenes"), "30");
    assert.match(rowText("sceneRows"), /scenes-30/);
  } finally {
    dom.window.close();
  }
});
