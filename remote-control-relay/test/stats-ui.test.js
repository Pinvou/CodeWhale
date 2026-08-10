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
  { device_id: "alpha", online: true, platform: "windows", first_seen_at: seenAt, last_active_at: seenAt, turns_today: 1, turns_7d: 4 },
  { device_id: "beta", online: true, platform: "linux", first_seen_at: seenAt, last_active_at: seenAt, turns_today: 3, turns_7d: 3 },
  { device_id: "gamma", online: true, platform: "macos", first_seen_at: seenAt, last_active_at: seenAt, turns_today: 2, turns_7d: 5 },
];

function renderedDeviceIds(document) {
  return [...document.querySelectorAll("#deviceRows .device b")].map((node) => node.textContent);
}

test("device detail sortable headers toggle descending and ascending order", async () => {
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
    assert.deepEqual(renderedDeviceIds(document), ["alpha", "beta", "gamma"]);

    const todaySort = document.querySelector('[data-sort="turns_today"]');
    todaySort.click();
    assert.deepEqual(renderedDeviceIds(document), ["beta", "gamma", "alpha"]);
    assert.equal(todaySort.closest("th").getAttribute("aria-sort"), "descending");
    assert.equal(todaySort.querySelector(".sort-arrow").textContent, "↓");

    todaySort.click();
    assert.deepEqual(renderedDeviceIds(document), ["alpha", "gamma", "beta"]);
    assert.equal(todaySort.closest("th").getAttribute("aria-sort"), "ascending");

    document.querySelector('[data-sort="turns_7d"]').click();
    assert.deepEqual(renderedDeviceIds(document), ["gamma", "alpha", "beta"]);

    const deviceSort = document.querySelector('[data-sort="device_id"]');
    deviceSort.click();
    assert.deepEqual(renderedDeviceIds(document), ["gamma", "beta", "alpha"]);
    deviceSort.click();
    assert.deepEqual(renderedDeviceIds(document), ["alpha", "beta", "gamma"]);
  } finally {
    dom.window.close();
  }
});
