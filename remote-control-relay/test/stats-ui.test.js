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
    assert.match(statsHtml, /WEVTA 是可观测价值下界/);
    assert.match(statsHtml, /样本不足 30 时不展示百分比/);
    assert.doesNotMatch(statsHtml, /细分组安装数低于隐私阈值|细分组已抑制|隐私分组阈值/);
    assert.match(statsHtml, /主体：安装实例/);
    assert.match(statsHtml, /模型质量排行榜/);
    assert.match(statsHtml, /row\.success_confidence/);
    assert.match(statsHtml, /metrics\.explicit_value_confidence/);
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
            data_quality: { confidence: "low", warnings: [] },
          } : { rows: [], funnel_rows: [], suppressed_group_count: 0 });
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
  } finally {
    dom.window.close();
  }
});
