import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { JSDOM } from "jsdom";

const html = readFileSync(new URL("../web/stats.html", import.meta.url), "utf8");
function openDashboard({ failSeven = false, legacy = false } = {}) {
  return new JSDOM(html, { runScripts: "dangerously", url: "https://stats.example/pinvou3/stats", beforeParse(window) {
    window.setInterval = () => 1;
    window.clearInterval = () => {};
    window.fetch = async url => {
      const path = String(url), range = new URL(path, "https://stats.example").searchParams.get("range") || "30";
      if (path.endsWith("/session")) return { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) };
      if (failSeven && range === "7") return { ok: false, status: 500 };
      const value = { range, data_status: "ready", metrics: {}, rows: [], remote_control: {
        task_count: range === "7" ? 7 : 3, submitted_count: 4, admitted_count: 3,
        succeeded_count: 2, failed_count: 1, cancelled_count: 0, success_rate: null,
        awaiting_receipt_count: 1, controller_installations: 1, controlled_installations: 1,
        rows: [{ stage: "task_rejected", outcome: "rejected", reason: "<unsafe>", count: 1 }],
      } };
      if (legacy) value.remote_control = { event_count: 0, rows: [] };
      return { ok: true, status: 200, json: async () => value };
    };
  } });
}
const settled = async dom => { for (let i = 0; i < 4; i++) await new Promise(resolve => dom.window.setTimeout(resolve, 15)); };

test("analyst can use independent remote tab, change its range, and keep other ranges", async () => {
  const dom = openDashboard();
  try {
    await settled(dom);
    const d = dom.window.document;
    const tab = d.querySelector('[data-view="remote"]');
    assert.equal(tab.hidden, false);
    tab.click();
    assert.equal(d.querySelector("#remoteView").classList.contains("hidden"), false);
    assert.equal(d.querySelector("#remoteTasks").textContent, "3");
    assert.equal(d.querySelector("#remoteSuccess").textContent, "样本不足");
    assert.match(d.querySelector("#remoteEventRows").textContent, /<unsafe>/);
    assert.equal(d.querySelector("#remoteEventRows unsafe"), null);
    d.querySelector('[data-behavior-section="remote"] [data-range="7"]').click();
    await settled(dom);
    assert.equal(d.querySelector("#remoteTasks").textContent, "7");
    assert.equal(d.querySelector('[data-behavior-section="summary"] .active').dataset.range, "30");
    d.querySelector("#refreshButton").click();
    await settled(dom);
    assert.equal(tab.getAttribute("aria-selected"), "true");
  } finally { dom.window.close(); }
});

test("remote range failure retains prior counts and explains unavailable data", async () => {
  const dom = openDashboard({ failSeven: true });
  try {
    await settled(dom);
    const d = dom.window.document;
    d.querySelector('[data-view="remote"]').click();
    d.querySelector('[data-behavior-section="remote"] [data-range="7"]').click();
    await settled(dom);
    assert.equal(d.querySelector("#remoteTasks").textContent, "3");
    assert.match(d.querySelector("#remoteLoadStatus").textContent, /加载失败/);
    assert.equal(d.querySelector('[data-behavior-section="remote"] .active').dataset.range, "30");
  } finally { dom.window.close(); }
});

test("older Relay is explicit about unavailable remote statistics", async () => {
  const dom = openDashboard({ legacy: true });
  try {
    await settled(dom);
    assert.match(dom.window.document.querySelector("#remoteMeta").textContent, /升级 Relay/);
    assert.equal(dom.window.document.querySelector("#remoteTasks").textContent, "—");
  } finally { dom.window.close(); }
});
