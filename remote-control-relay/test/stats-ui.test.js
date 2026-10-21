import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { JSDOM } from "jsdom";

const testDir = fileURLToPath(new URL(".", import.meta.url));
const statsHtml = readFileSync(join(testDir, "..", "web", "stats.html"), "utf8");

test('behavior metric switch uses instance metadata in Beijing time and marks unknown dates honestly', async () => {
  let startedAt = Date.parse('2034-02-04T23:21:00Z');
  let revision = 0;
  const dom = new JSDOM(statsHtml, {
    runScripts:'dangerously', url:'https://www.pinvou.com/pinvou3/stats',
    beforeParse(window) {
      window.fetch = async url => ({ ok:true, status:200, json:async () => String(url).endsWith('/session')
        ? { role:'behavior_analyst' }
        : { range:'30', data_status:'ready', metric_version:revision ? `behavior-v1.5-${revision}` : 'behavior-v1.5', metric_version_started_at:startedAt } });
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });
  const meta = () => dom.window.document.querySelector('#behaviorMeta').textContent;
  const until = async expected => {
    for (let attempt = 0; attempt < 100 && !expected.test(meta()); attempt++) {
      await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    }
    assert.match(meta(), expected);
  };
  try {
    await until(/本实例切换时间 2034\/02\/05 07:21/);
    // Complete the startup refresh's trailing microtasks before another refresh
    // or JSDOM teardown. Its fetch mocks resolve without external I/O.
    await new Promise(setImmediate);
    assert.match(meta(), /behavior-v1\.5/);
    assert.doesNotMatch(meta(), /2026-09-10|2026-09-11/);
    for (const value of [null, undefined, '2034-02-05', -1, 1e99]) {
      startedAt = value;
      revision++;
      // Await the complete production refresh, including its DOM bookkeeping,
      // rather than treating renderBehavior's earlier mutation as completion.
      await dom.window.refreshData(true);
      await until(new RegExp(`behavior-v1\\.5-${revision}`));
      await until(/本实例切换时间未记录/);
      assert.doesNotMatch(meta(), /2034\/02\/05|2026-09-10|2026-09-11/);
    }
  } finally { dom.window.close(); }
});

for (const [olderStatus, newerStatus] of [[200, 200], [500, 200], [401, 200], [200, 500]]) {
  test(`overlapping refreshes ignore older ${olderStatus} after newer ${newerStatus}`, async () => {
    let batch = -1;
    let releaseOlder;
    const olderPending = new Promise(resolve => { releaseOlder = resolve; });
    const dom = new JSDOM(statsHtml, {
      runScripts: "dangerously",
      url: "https://www.pinvou.com/pinvou3/stats",
      beforeParse(window) {
        window.fetch = async url => {
          const path = String(url);
          if (path.endsWith("/session")) return { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) };
          if (path.includes("/behavior/overview")) batch++;
          const requestBatch = batch;
          if (path.includes("/behavior/models")) {
            if (requestBatch === 1) await olderPending;
            const status = requestBatch === 1 ? olderStatus : requestBatch === 2 ? newerStatus : 200;
            if (status !== 200) return { ok: false, status };
          }
          const value = path.includes("/behavior/overview")
            ? { range: "30", data_status: "ready", metrics: { task_success_count: requestBatch * 100 + 42 } }
            : { range: "30", rows: [{ model_key: `model-${requestBatch}`, tool_key: `tool-${requestBatch}` }] };
          return { ok: true, status: 200, json: async () => value };
        };
        window.setInterval = () => 1;
        window.clearInterval = () => {};
      },
    });
    try {
      const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
      await wait();
      const { document } = dom.window;
      document.querySelector("#refreshButton").click();
      await wait();
      document.querySelector("#refreshButton").click();
      await wait();
      assert.equal(document.querySelector("#taskSuccess").textContent, "242");
      releaseOlder();
      await wait();
      assert.equal(document.querySelector("#taskSuccess").textContent, "242");
      assert.match(document.querySelector("#toolCallRows").textContent, /tool-2/);
      assert.match(document.querySelector("#modelRows").textContent, newerStatus === 200 ? /model-2/ : /model-0/);
      assert.equal(document.querySelector("#behaviorLoadStatus").classList.contains("hidden"), newerStatus === 200);
      assert.equal(document.querySelector("#loginLayer").classList.contains("hidden"), true);
      assert.match(document.querySelector("#refreshButton").textContent, newerStatus === 200 ? /已更新/ : /部分失败/);
    } finally {
      releaseOlder();
      dom.window.close();
    }
  });
}

for (const olderStatus of [200, 401]) {
  test(`an older delayed session ${olderStatus} cannot restart refresh or undo login`, async () => {
    let sessionCount = 0;
    let overviewCount = 0;
    let releaseSession;
    const pending = new Promise(resolve => { releaseSession = resolve; });
    const dom = new JSDOM(statsHtml, {
      runScripts: "dangerously",
      url: "https://www.pinvou.com/pinvou3/stats",
      beforeParse(window) {
        window.fetch = async url => {
          const path = String(url);
          if (path.endsWith("/session")) {
            const request = sessionCount++;
            if (request === 1) {
              await pending;
              if (olderStatus !== 200) return { ok: false, status: olderStatus };
            }
            return { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) };
          }
          if (path.includes("/behavior/overview")) overviewCount++;
          return { ok: true, status: 200, json: async () => ({ range: "30", rows: [], metrics: { task_success_count: overviewCount } }) };
        };
        window.setInterval = () => 1;
        window.clearInterval = () => {};
      },
    });
    try {
      const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
      await wait();
      const { document } = dom.window;
      document.querySelector("#refreshButton").click();
      await wait();
      document.querySelector("#refreshButton").click();
      await wait();
      assert.equal(overviewCount, 2);
      releaseSession();
      await wait();
      assert.equal(overviewCount, 2);
      assert.equal(document.querySelector("#taskSuccess").textContent, "2");
      assert.equal(document.querySelector("#loginLayer").classList.contains("hidden"), true);
    } finally {
      releaseSession();
      dom.window.close();
    }
  });
}

test("an older bulk result cannot overwrite a section that switched away and back", async () => {
  let modelRequests = 0;
  let releaseOlder;
  const pending = new Promise(resolve => { releaseOlder = resolve; });
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously",
    url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async url => {
        const path = String(url);
        if (path.endsWith("/session")) return { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) };
        const range = new URL(path, "https://www.pinvou.com").searchParams.get("range") || "30";
        let modelKey;
        if (path.includes("/behavior/models")) {
          const request = modelRequests++;
          if (request === 1) await pending;
          modelKey = `model-${request}`;
        }
        return { ok: true, status: 200, json: async () => ({ range, rows: [{ model_key: modelKey }] }) };
      };
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });
  try {
    const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
    await wait();
    const { document } = dom.window;
    document.querySelector("#refreshButton").click();
    await wait();
    document.querySelector('[data-behavior-section="models"] [data-range="7"]').click();
    await wait();
    document.querySelector('[data-behavior-section="models"] [data-range="30"]').click();
    await wait();
    assert.match(document.querySelector("#modelRows").textContent, /model-3/);
    releaseOlder();
    await wait();
    assert.match(document.querySelector("#modelRows").textContent, /model-3/);
    assert.equal(document.querySelector('[data-behavior-section="models"] button.active').dataset.range, "30");
  } finally {
    releaseOlder();
    dom.window.close();
  }
});

for (const section of ["models", "summary"]) {
  test(`a full refresh preserves an in-flight ${section} range switch`, async () => {
    let releaseSwitch;
    const pending = new Promise(resolve => { releaseSwitch = resolve; });
    const dom = new JSDOM(statsHtml, {
      runScripts: "dangerously",
      url: "https://www.pinvou.com/pinvou3/stats",
      beforeParse(window) {
        window.fetch = async url => {
          const path = String(url);
          if (path.endsWith("/session")) return { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) };
          const range = new URL(path, "https://www.pinvou.com").searchParams.get("range") || "30";
          if (path.includes(section === "models" ? "/behavior/models" : "/behavior/overview") && range === "7") await pending;
          return { ok: true, status: 200, json: async () => ({ range, metrics: { task_success_count: Number(range) }, rows: [{ model_key: `model-${range}` }] }) };
        };
        window.setInterval = () => 1;
        window.clearInterval = () => {};
      },
    });
    try {
      const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
      await wait();
      const { document } = dom.window;
      document.querySelector(`[data-behavior-section="${section}"] [data-range="7"]`).click();
      await wait();
      document.querySelector("#refreshButton").click();
      await wait();
      releaseSwitch();
      await wait();
      if (section === "models") assert.match(document.querySelector("#modelRows").textContent, /model-7/);
      else assert.equal(document.querySelector("#taskSuccess").textContent, "7");
      assert.equal(document.querySelector(`[data-behavior-section="${section}"] button.active`).dataset.range, "7");
      assert.equal(document.querySelector(`[data-behavior-section="${section}"]`).getAttribute("aria-busy"), "false");
    } finally {
      releaseSwitch();
      dom.window.close();
    }
  });
}

for (const failedPath of ["/behavior/overview", "/behavior/models", "/commercial/overview"]) {
  test(`behavior dashboard isolates ${failedPath} failures and recovers on refresh`, async () => {
    let fail = true;
    let successCount = 42;
    const dom = new JSDOM(statsHtml, {
      runScripts: "dangerously",
      url: "https://www.pinvou.com/pinvou3/stats",
      beforeParse(window) {
        window.fetch = async url => {
          const path = String(url);
          if (fail && path.includes(failedPath)) return { ok: false, status: 400 };
          const value = path.endsWith("/session") ? { role: "behavior_analyst" }
            : path.includes("/behavior/overview") ? {
              range: "30", data_status: "ready", metrics: { task_success_count: successCount },
            } : { range: "30", rows: [{ tool_key: "working-tool", model_key: "working-model" }] };
          return { ok: true, status: 200, json: async () => value };
        };
        window.setInterval = () => 1;
        window.clearInterval = () => {};
      },
    });
    try {
      const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
      await wait();
      const { document } = dom.window;
      const banner = document.querySelector("#behaviorLoadStatus");
      assert.equal(banner.classList.contains("hidden"), false);
      assert.match(banner.textContent, /加载失败/);
      assert.match(document.querySelector("#toolCallRows").textContent, /working-tool/);
      if (failedPath === "/behavior/overview") {
        assert.equal(document.querySelector("#taskSuccess").textContent, "—");
        assert.match(document.querySelector("#activationFunnelRows").textContent, /加载失败/);
        assert.match(document.querySelector("#modelRows").textContent, /working-model/);
      } else {
        assert.equal(document.querySelector("#taskSuccess").textContent, "42");
      }
      document.querySelector("#refreshButton").click();
      await wait();
      assert.match(document.querySelector("#refreshButton").textContent, /部分失败/);
      fail = false;
      document.querySelector("#refreshButton").click();
      await wait();
      assert.equal(banner.classList.contains("hidden"), true);
      assert.equal(document.querySelector("#taskSuccess").textContent, "42");
      assert.match(document.querySelector("#modelRows").textContent, /working-model/);
      fail = true;
      successCount = 99;
      document.querySelector("#refreshButton").click();
      await wait();
      assert.equal(banner.classList.contains("hidden"), false);
      assert.match(document.querySelector("#modelRows").textContent, /working-model/);
      assert.equal(document.querySelector("#taskSuccess").textContent, failedPath === "/behavior/overview" ? "42" : "99");
    } finally {
      dom.window.close();
    }
  });
}

test("a behavior API authentication failure returns to login", async () => {
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously",
    url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async url => String(url).endsWith("/session")
        ? { ok: true, status: 200, json: async () => ({ role: "behavior_analyst" }) }
        : { ok: false, status: 401 };
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });
  try {
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    assert.equal(dom.window.document.querySelector("#loginLayer").classList.contains("hidden"), false);
  } finally {
    dom.window.close();
  }
});

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
    usage_trend_hourly_today: [{ hour: seenAt, active_devices: 3, new_devices: 1, turns: 4 }],
    usage_trend_hourly_previous: [{ hour: seenAt - 86_400_000, active_devices: 2, new_devices: 1, turns: 3 }],
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
    const controls = [...document.querySelectorAll('[data-daily-metric]')];
    assert.equal(controls.length, 3);
    assert(controls.every(input => input.checked && input.labels.length === 1));
    const hourlyBefore = document.querySelector('#hourlyTrendSvg').innerHTML;
    const classes = ['line', 'line3', 'line2'];
    const labels = ['活跃设备', '新增设备', '对话次数'];
    for (let mask = 0; mask < 8; mask++) {
      controls.forEach((input, index) => {
        if (input.checked !== Boolean(mask & (1 << index))) input.click();
      });
      for (const range of ['30', '7']) {
        document.querySelector(`[data-days="${range}"]`).click();
        controls.forEach((input, index) => {
          assert.equal(svg.querySelectorAll(`path.${classes[index]}`).length, input.checked ? 1 : 0);
          assert.equal(svg.querySelectorAll('.trend-dot').length, controls.filter(control => control.checked).length);
        });
        assert.equal(Boolean(svg.querySelector('.area')), controls[0].checked);
        if (mask) {
          hitArea = document.querySelector('#trendHitArea');
          assert.equal(hitArea.getAttribute('aria-valuemax'), range === '7' ? '6' : '7');
          hitArea.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'End', bubbles: true }));
          labels.forEach((label, index) => {
            assert.equal(tooltip.textContent.includes(label), controls[index].checked);
            assert.equal(hitArea.getAttribute('aria-valuetext').includes(label), controls[index].checked);
          });
        } else {
          assert.equal(document.querySelector('#trendHitArea'), null);
          assert.equal(tooltip.hidden, true);
          assert.match(svg.textContent, /请选择至少一项指标/);
        }
        assert.equal(document.querySelector('#hourlyTrendSvg').innerHTML, hourlyBefore);
      }
    }
    controls[0].click();
    controls[1].click();
    document.querySelector('#refreshButton').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    assert.deepEqual(controls.map(input => input.checked), [false, false, true]);
    assert.equal(svg.querySelectorAll('path.line2').length, 1);
    assert.equal(svg.querySelector('.area'), null);
    const dailyBefore = svg.innerHTML;
    document.querySelector('[data-hourly-metric="turns"]').click();
    assert.equal(svg.innerHTML, dailyBefore);
    controls[2].click();
    document.querySelector('#refreshButton').click();
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    assert(controls.every(input => !input.checked));
    assert.match(svg.textContent, /请选择至少一项指标/);
    controls.forEach(input => input.click());
    hitArea = document.querySelector('#trendHitArea');
    hitArea.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'End', bubbles: true }));
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

test("version coverage shows four independent charts with five versions and remaining share", async () => {
  const versions = Array.from({ length: 7 }, (_, index) => ({ version: `1.${index}`, count: 7 - index }));
  const report = {
    ...overview, active_versions: versions,
    active_versions_by_platform: {
      windows: versions.slice(0, 6), linux: [{ version: '<img src=x onerror="alert(1)">', count: 2 }], macos: [],
    },
  };
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously", url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async url => ({ ok: true, status: 200, json: async () => String(url).endsWith("/overview") ? report : { devices } });
      window.setInterval = () => 1;
    },
  });
  try {
    await new Promise(resolve => dom.window.setTimeout(resolve, 20));
    const doc = dom.window.document;
    assert.equal(doc.querySelectorAll(".version-coverage .version-panel").length, 4);
    assert.equal(doc.querySelector(".usage-panels .version-coverage"), null);
    for (const prefix of ["version", "windowsVersion"]) {
      const labels = [...doc.querySelectorAll(`#${prefix}List .version-row b`)].map(el => el.textContent);
      assert.deepEqual(labels, ["1.0", "1.1", "1.2", "1.3", "1.4", "其他版本"]);
    }
    assert.equal(doc.querySelector("#versionTotal").textContent, "28");
    assert.equal(doc.querySelector("#windowsVersionTotal").textContent, "27");
    assert.match(doc.querySelector("#versionList .version-row:last-child").textContent, /3 台 · 11%/);
    assert.match(doc.querySelector("#windowsVersionList .version-row:last-child").textContent, /2 台 · 7%/);
    assert.match(doc.querySelector("#linuxVersionList").textContent, /2 台 · 100%/);
    assert.equal(doc.querySelector("#linuxVersionList img"), null);
    assert.equal(doc.querySelector("#macosVersionTotal").textContent, "0");
    assert.match(doc.querySelector("#macosVersionList").textContent, /暂无活跃版本数据/);
    assert.equal(doc.querySelector("#versionCount").textContent, "版本 7 个");
  } finally { dom.window.close(); }
});

test("hourly and daily charts coexist with independent pointers, keyboard navigation and refresh", async () => {
  const start = Date.parse("2026-10-19T00:00:00+08:00");
  let report = {
    ...overview,
    usage_trend_hourly_today: Array.from({ length: 24 }, (_, index) => ({
      hour: start + index * 3_600_000, active_devices: index === 23 ? 2 : 0,
      new_devices: index === 23 ? 1 : 0, turns: index === 23 ? 9 : 0,
    })),
  };
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously", url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async url => ({ ok: true, status: 200, json: async () => String(url).endsWith("/overview") ? report : { devices } });
      window.setInterval = () => 1;
    },
  });
  try {
    const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
    await wait();
    const doc = dom.window.document;
    assert.equal(doc.querySelectorAll(".usage-panels .chart").length, 2);
    assert.equal(doc.querySelectorAll("#hourlyTrendSvg path.line,#hourlyTrendSvg path.line2,#hourlyTrendSvg path.line3").length, 3);
    const hourlySvg = doc.querySelector("#hourlyTrendSvg");
    hourlySvg.getBoundingClientRect = () => ({ left: 0, width: 780 });
    let hit = doc.querySelector("#hourlyTrendHitArea");
    const tooltip = doc.querySelector("#hourlyTrendTooltip");
    hit.dispatchEvent(new dom.window.MouseEvent("pointermove", { clientX: 770, bubbles: true }));
    assert.equal(tooltip.hidden, false);
    assert.match(tooltip.textContent, /2026-10-19 23:00–00:00（北京时间）/);
    assert.match(tooltip.textContent, /活跃设备2 台/);
    assert.match(tooltip.textContent, /新增设备1 台/);
    assert.match(tooltip.textContent, /对话次数9 次/);
    assert.equal(doc.querySelector("#trendTooltip").hidden, true);
    hit.dispatchEvent(new dom.window.KeyboardEvent("keydown", { key: "Home", bubbles: true }));
    assert.match(hit.getAttribute("aria-valuetext"), /2026-10-19 00:00–01:00/);
    assert.match(tooltip.textContent, /对话次数0 次/);
    assert.equal(hit.getAttribute("aria-valuemax"), "23");
    const before = hourlySvg.innerHTML;
    doc.querySelector('[data-days="7"]').click();
    assert.equal(hourlySvg.innerHTML, before);
    assert.equal(new Set([...doc.querySelectorAll("[id]")].map(el => el.id)).size, doc.querySelectorAll("[id]").length);
    report = { ...report, usage_trend_hourly_today: report.usage_trend_hourly_today.map(row => ({ ...row, turns: 4 })) };
    doc.querySelector("#refreshButton").click();
    await wait();
    hit = doc.querySelector("#hourlyTrendHitArea");
    hit.dispatchEvent(new dom.window.KeyboardEvent("keydown", { key: "End", bubbles: true }));
    assert.match(tooltip.textContent, /对话次数4 次/);
    report = { ...report, usage_trend_hourly_today: [] };
    doc.querySelector("#refreshButton").click();
    await wait();
    assert.equal(tooltip.hidden, true);
    assert.equal(doc.querySelector("#hourlyTrendHitArea"), null);
    assert.match(hourlySvg.textContent, /暂无趋势数据/);
  } finally { dom.window.close(); }
});

test("hourly comparison shares scales, styles yesterday separately and does not draw future zeros", async () => {
  const today = Date.parse("2026-10-19T00:00:00+08:00");
  let report = {
    ...overview,
    usage_trend_hourly_today: Array.from({ length: 24 }, (_, index) => ({
      hour: today + index * 3_600_000, active_devices: index === 0 ? 1 : null,
      new_devices: index === 0 ? 0 : null, turns: index === 0 ? 10 : null,
    })),
    usage_trend_hourly_previous: Array.from({ length: 24 }, (_, index) => ({
      hour: today - 86_400_000 + index * 3_600_000, active_devices: 2, new_devices: 1, turns: 20,
    })).reverse(), // Match timestamps, never rely on transport ordering.
  };
  const dom = new JSDOM(statsHtml, {
    runScripts: "dangerously", url: "https://www.pinvou.com/pinvou3/stats",
    beforeParse(window) {
      window.fetch = async url => ({ ok: true, status: 200, json: async () => String(url).endsWith("/overview") ? report : { devices } });
      window.setInterval = () => 1;
    },
  });
  try {
    const wait = () => new Promise(resolve => dom.window.setTimeout(resolve, 20));
    await wait();
    const doc = dom.window.document;
    const svg = doc.querySelector("#hourlyTrendSvg");
    assert.equal(svg.querySelectorAll("path.previous-day").length, 3);
    const turns = svg.querySelector("path.line2:not(.previous-day)");
    const previous = svg.querySelector("path.line2.previous-day");
    assert.equal(turns.getAttribute("d"), "M35.0 103.0"); // 10 on the shared 20 scale.
    assert.match(previous.getAttribute("d"), /^M35\.0 22\.0 L/);
    assert.equal(dom.window.getComputedStyle(turns).strokeDasharray, "none");
    assert.equal(dom.window.getComputedStyle(previous).strokeDasharray, "5 5");
    assert.equal(dom.window.getComputedStyle(previous).opacity, "0.5");
    assert.equal(svg.querySelectorAll("circle.current-point").length, 3);
    assert.match(svg.parentElement.querySelector(".chart-axis").textContent, /^00:00.*23:00$/);
    let hit = doc.querySelector("#hourlyTrendHitArea");
    const tooltip = doc.querySelector("#hourlyTrendTooltip");
    hit.dispatchEvent(new dom.window.Event("focus"));
    assert.equal(hit.getAttribute("aria-valuenow"), "0");
    assert.match(tooltip.textContent, /今日.*对话次数10 次.*昨日 2026-10-18 00:00.*对话次数20 次/);
    hit.dispatchEvent(new dom.window.KeyboardEvent("keydown", { key: "End", bubbles: true }));
    assert.match(tooltip.textContent, /今日尚未到达.*昨日 2026-10-18 23:00.*对话次数20 次/);
    assert.match(hit.getAttribute("aria-valuetext"), /尚未到达.*昨日.*对话次数 20 次/);
    assert.equal(doc.querySelector("#hourlyTrendDotTurns").style.display, "none");
    hit.dispatchEvent(new dom.window.MouseEvent("pointerleave"));
    assert.equal(tooltip.hidden, true);
    assert.equal(doc.querySelector("#hourlyTrendHover").getAttribute("visibility"), "hidden");
    const controls = [...doc.querySelectorAll('[data-hourly-metric]')];
    assert.equal(controls.length, 3);
    assert(controls.every(input => input.checked && input.labels.length === 1));
    const dailyBefore = doc.querySelector('#trendSvg').innerHTML;
    for (const [key, className, label] of [
      ['active_devices', 'line', '活跃设备'],
      ['new_devices', 'line3', '新增设备'],
      ['turns', 'line2', '对话次数'],
    ]) {
      const input = doc.querySelector(`[data-hourly-metric="${key}"]`);
      input.click();
      assert.equal(svg.querySelectorAll(`path.${className}`).length, 0);
      assert.equal(svg.querySelectorAll('path.previous-day').length, 2);
      assert.equal(svg.querySelectorAll('circle.current-point').length, 2);
      if (key === 'active_devices') assert.equal(svg.querySelector('.area'), null);
      hit = doc.querySelector('#hourlyTrendHitArea');
      hit.dispatchEvent(new dom.window.Event('focus'));
      assert(!tooltip.textContent.includes(label));
      assert(!hit.getAttribute('aria-valuetext').includes(label));
      assert.equal(doc.querySelector('#trendSvg').innerHTML, dailyBefore);
      input.click();
      assert.equal(svg.querySelectorAll(`path.${className}`).length, 2);
      assert.equal(tooltip.hidden, true);
    }
    controls[0].click();
    controls[1].click();
    doc.querySelector('#refreshButton').click();
    await wait();
    assert.deepEqual(controls.map(input => input.checked), [false, false, true]);
    assert.equal(svg.querySelectorAll('path.previous-day').length, 1);
    assert.equal(svg.querySelectorAll('path.line2').length, 2);
    assert.equal(svg.querySelectorAll('.area').length, 0);
    controls[2].click();
    assert.equal(svg.querySelectorAll('path,circle,rect.trend-hit').length, 0);
    assert.match(svg.textContent, /请选择至少一项指标/);
    assert.equal(tooltip.hidden, true);
    doc.querySelector('#refreshButton').click();
    await wait();
    assert(controls.every(input => !input.checked));
    assert.match(svg.textContent, /请选择至少一项指标/);
    controls.forEach(input => input.click());
    assert.equal(svg.querySelectorAll('path.previous-day').length, 3);
    assert.equal(svg.querySelectorAll('circle.current-point').length, 3);
    report = { ...report, usage_trend_hourly_previous: [] };
    doc.querySelector("#refreshButton").click();
    await wait();
    assert.equal(svg.querySelectorAll("path.previous-day").length, 0);
    hit = doc.querySelector("#hourlyTrendHitArea");
    hit.dispatchEvent(new dom.window.Event("focus"));
    assert.match(tooltip.textContent, /昨日.*暂无数据/);
  } finally { dom.window.close(); }
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
    assert.match(statsHtml, /metrics\.explicit_value_sample_size/);
    assert.match(statsHtml, /metrics\.explicit_value_ineligible_installations/);
    assert.match(statsHtml, /row\.explicit_value_confidence/);
    assert.match(statsHtml, /metrics\.turn_mismatch_value_signal_count/);
    assert.match(statsHtml, /explicit_value_join_key_missing/);
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
            data_quality: { confidence: "low", warnings: ["explicit_value_join_key_missing", "explicit_value_signal_turn_mismatch"] },
            metrics: {
              explicit_value_ineligible_installations: 2,
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
    assert.match(document.querySelector("#behaviorQuality").textContent, /缺少会话或回合标识.*2 台/);
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
                explicit_value_sample_size: 850,
                explicit_value_confidence: "sufficient",
                positive_feedback_installations: 700,
                response_copied_installations: 600,
                artifact_exported_installations: 500,
                negative_feedback_rate: 0.1,
              },
              retention: {
                d1: { basis:"first_success_beijing_day", observation:"realtime", eligible_installations: 100, retained_installations: 50, retention_rate: 0.5, confidence: "sufficient" },
                d7: { basis:"first_success_beijing_day", observation:"realtime", eligible_installations: 90, retained_installations: 36, retention_rate: 0.4, confidence: "sufficient" },
                d30: { basis:"first_success_beijing_day", observation:"realtime", eligible_installations: 80, retained_installations: 24, retention_rate: 0.3, confidence: "sufficient", provisional_installations: 3, history_status: "partial" },
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
    assert.match(document.querySelector("#retentionD30Sample").textContent, /首次成功起算 · 实时更新/);
    assert.match(document.querySelector("#retentionD30Sample").textContent, /纳入 80 · 留存 24 · 今日观察 3/);
    assert.match(document.querySelector("#retentionD30Sample").textContent, /历史可能不完整/);

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

test('retention labels follow known API semantics and behavior timestamps explicitly use Beijing time', async () => {
  const instant = Date.parse('2034-02-04T23:21:00Z');
  let row = { basis:'first_success_beijing_day', observation:'realtime', eligible_installations:31, retained_installations:20 };
  const dom = new JSDOM(statsHtml, {
    runScripts:'dangerously', url:'https://www.pinvou.com/pinvou3/stats',
    beforeParse(window) {
      const format = window.Date.prototype.toLocaleString;
      // Simulate a browser whose local zone differs from the report's zone.
      window.Date.prototype.toLocaleString = function(locale, options) {
        return format.call(this, locale, { timeZone:'America/Los_Angeles', ...options });
      };
      window.fetch = async url => ({ ok:true, status:200, json:async () => String(url).endsWith('/session')
        ? { role:'behavior_analyst' }
        : { range:'30', data_status:'ready', metric_version:'behavior-v1.5', metric_version_started_at:instant,
          latest_event_received_at:instant, generated_at:instant, retention:{ d1:row } } });
      window.setInterval = () => 1;
      window.clearInterval = () => {};
    },
  });
  try {
    const { document } = dom.window;
    for (let attempt = 0; attempt < 100 && !document.querySelector('#retentionD1Sample').textContent.includes('纳入 31'); attempt++) {
      await new Promise(resolve => dom.window.setTimeout(resolve, 10));
    }
    await new Promise(setImmediate);
    const sample = () => document.querySelector('#retentionD1Sample').textContent;
    assert.match(sample(), /首次成功起算 · 实时更新 · 纳入 31 · 留存 20/);
    const meta = document.querySelector('#behaviorMeta').textContent;
    assert.match(meta, /最后事件（北京时间）：2034\/02\/05 07:21/);
    assert.match(meta, /报表更新（北京时间）：2034\/02\/05 07:21/);
    for (const value of [undefined, '<private-unrecognized-value>']) {
      row = { ...row, basis:value, observation:value };
      await dom.window.refreshData(true);
      assert.match(sample(), /起算口径未知 · 更新口径未知/);
      assert.doesNotMatch(sample(), /首次成功起算|实时更新|private-unrecognized/);
    }
  } finally { dom.window.close(); }
});
