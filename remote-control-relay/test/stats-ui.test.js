import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { JSDOM } from "jsdom";

const testDir = fileURLToPath(new URL(".", import.meta.url));
const statsHtml = readFileSync(join(testDir, "..", "web", "stats.html"), "utf8");

const overview = {
  generated_at: Date.now(),
  counts: { online: 3, active_today: 3, active_7d: 3 },
  usage_trend: [],
  active_versions: [],
};

const seenAt = Date.parse("2026-08-10T09:00:00+08:00");
const devices = [
  { device_id: "alpha", online: true, platform: "windows", first_seen_at: seenAt, last_active_at: seenAt, turns_7d: 4 },
  { device_id: "beta", online: true, platform: "linux", first_seen_at: seenAt, last_active_at: seenAt, turns_7d: 3 },
  { device_id: "gamma", online: true, platform: "macos", first_seen_at: seenAt, last_active_at: seenAt, turns_7d: 5 },
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
    assert.match(statsHtml, /细分组安装数低于隐私阈值/);
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
  } finally {
    dom.window.close();
  }
});
