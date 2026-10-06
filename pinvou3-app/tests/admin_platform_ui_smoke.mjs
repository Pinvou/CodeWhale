// Real React/browser behavior against public controlled clients, not a running
// BFF, PostgreSQL, Keycloak, payment provider, Safari, or production acceptance.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import puppeteer from "puppeteer-core";
import { createServer } from "vite";

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const fixture = path.join(appRoot, "tests/fixtures/admin-platform-ui.jsx").replaceAll("\\", "/");
const chrome = [
  process.env.CHROME,
  process.env.EDGE,
  "C:/Program Files/Google/Chrome/Application/chrome.exe",
  "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
  "C:/Program Files/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
  "/usr/bin/chromium",
  "/usr/bin/chromium-browser",
  "/usr/bin/google-chrome",
  "/usr/bin/google-chrome-stable",
  "/snap/bin/chromium",
].find((candidate) => candidate && fs.existsSync(candidate));
if (!chrome) throw new Error("Browser required: set CHROME or EDGE to an installed browser executable");

const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "pinvou-admin-ui-"));
let browser;
let vite;
let origin;
let passed = 0;
const failures = [];
async function run(name, scenario, check) {
  const page = await browser.newPage();
  const pageErrors = [];
  const externalRequests = [];
  const keyWarnings = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  page.on("console", (message) => {
    if (/same key|Each child in a list|unique.*key/iu.test(message.text())) keyWarnings.push(message.text());
  });
  await page.setRequestInterception(true);
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.origin === origin || url.protocol === "data:") void request.continue();
    else {
      externalRequests.push(url.origin);
      void request.abort("blockedbyclient");
    }
  });
  try {
    await page.goto(`${origin}/__admin_review__?scenario=${encodeURIComponent(scenario)}`, {
      waitUntil: "networkidle0",
    });
    await page.waitForSelector("nav", { timeout: 15_000 });
    await check(page);
    assert.deepEqual(pageErrors, [], "React/browser must not raise an uncaught exception");
    assert.deepEqual(externalRequests, [], "The fixture must not contact an external origin");
    assert.deepEqual(keyWarnings, [], "Entity rows must not produce duplicate or missing React keys");
    assert.equal(
      await page.evaluate(() => window.__adminReview.unhandled()),
      0,
      "Every rejected action must be handled",
    );
    passed += 1;
    console.log(`PASS ${name}`);
  } catch (error) {
    failures.push(name);
    console.error(`FAIL ${name}: ${error.message}`);
  } finally {
    await page.close();
  }
}
async function settle(page, key, status, name) {
  await page.evaluate(
    (key, status, name) => window.__adminReview.settle(key, status, name),
    key,
    status,
    name,
  );
}
async function pending(page, key) {
  await page.waitForFunction((key) => window.__adminReview.pending().includes(key), { timeout: 15_000 }, key);
}
async function nav(page, index) {
  await page.click(`nav button:nth-child(${index})`);
}
async function search(page, value) {
  await page.$eval(
    ".adm-search input",
    (element, value) => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set;
      setter.call(element, value);
      element.dispatchEvent(new Event("input", { bubbles: true }));
    },
    value,
  );
  await page.click(".adm-search button");
}
async function input(page, selector, value) {
  await page.$eval(selector, (element, value) => {
    const prototype = element.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(prototype, 'value').set.call(element, value);
    element.dispatchEvent(new Event('input', { bubbles:true }));
  }, value);
}
async function expectRows(page, cell, expected, attribute) {
  await page.waitForFunction(
    (cell, expected, attribute) => {
      const rows = [...document.querySelectorAll("tbody tr")];
      const value = (row) =>
        attribute
          ? row.cells[cell].querySelector("[title]").getAttribute(attribute)
          : row.cells[cell].textContent;
      return rows.length === 3 && rows.every((row, index) => value(row) === expected[index]);
    },
    { timeout: 15_000 },
    cell,
    expected,
    attribute,
  );
  assert.equal(
    await page.$$eval("tbody tr", (rows) => rows.length),
    3,
    "Each of the three unique entities must appear once",
  );
  assert.deepEqual(
    await page.$$eval(
      "tbody tr",
      (rows, cell, attribute) =>
        rows.map((row) =>
          attribute
            ? row.cells[cell].querySelector("[title]").getAttribute(attribute)
            : row.cells[cell].textContent,
        ),
      cell,
      attribute,
    ),
    expected,
  );
}
try {
  vite = await createServer({
    configFile: false,
    root: path.join(appRoot, "src"),
    cacheDir: path.join(tempRoot, "vite-cache"),
    optimizeDeps: { entries: [] },
    plugins: [
      react(),
      {
        name: "admin-ui-review-fixture",
        configureServer(server) {
          server.middlewares.use(async (request, response, next) => {
            if (new URL(request.url, "http://127.0.0.1").pathname !== "/__admin_review__") return next();
            try {
              const html = await server.transformIndexHtml(
                request.url,
                `<html><body><div id="admin-root"></div><script type="module" src="/@fs/${fixture}"></script></body></html>`,
              );
              response.setHeader("Content-Type", "text/html; charset=utf-8");
              response.end(html);
            } catch (error) {
              next(error);
            }
          });
        },
      },
    ],
    server: { host: "127.0.0.1", port: 0, strictPort: true, fs: { allow: [appRoot] } },
  });
  await vite.listen();
  const address = vite.httpServer.address();
  assert.equal(typeof address, "object");
  assert.equal(address.address, "127.0.0.1");
  origin = `http://127.0.0.1:${address.port}`;
  browser = await puppeteer.launch({
    executablePath: chrome,
    headless: true,
    userDataDir: path.join(tempRoot, "browser-profile"),
  });
  await run("late_authorization_failure_ignored", "navigation", async (page) => {
    await nav(page, 3);
    await pending(page, "users");
    await nav(page, 4);
    await pending(page, "wallets");
    await settle(page, "users", 401);
    await settle(page, "wallets");
    await page.waitForSelector(".adm-tabs");
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
  });
  await run("current_authorization_failure_fences_old_success", "navigation", async (page) => {
    await nav(page, 3);
    await pending(page, "users");
    await nav(page, 4);
    await pending(page, "wallets");
    await settle(page, "wallets", 401);
    await page.waitForSelector(".adm-state a");
    await settle(page, "users");
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.equal(await page.$$("nav").then((x) => x.length), 0);
  });
  await run("cached_navigation_cancels_pending", "navigation", async (page) => {
    await nav(page, 3);
    await pending(page, "users");
    await nav(page, 1);
    await settle(page, "users", 403);
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
    assert.equal(await page.$$(".adm-progress").then((x) => x.length), 0);
  });
  await run("logout_failure_recoverable", "normal", async (page) => {
    await page.click(".adm-admin button");
    await page.waitForSelector("[role=alert]");
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
  });
  await run("search_failure_recoverable", "search-error", async (page) => {
    await nav(page, 3);
    await page.waitForSelector(".adm-search");
    await search(page, "public");
    await page.waitForSelector("[role=alert]");
  });
  await run("nested_search_401_closes_business_view", "search-auth", async (page) => {
    await nav(page, 3);
    await page.waitForSelector(".adm-search");
    await search(page, "public");
    await page.waitForSelector(".adm-state a");
    assert.equal(await page.$$("nav").then((x) => x.length), 0);
  });
  await run("nested_mutation_403_preserves_other_authorized_views", "normal", async (page) => {
    await nav(page, 3);
    await page.waitForSelector(".adm-row-actions");
    const originalStatus = await page.$eval(".adm-status", (element) => element.textContent);
    await page.click(".adm-row-actions button");
    await page.waitForSelector("[role=alert]");
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
    assert.equal(await page.$eval(".adm-status", (element) => element.textContent), originalStatus);
    assert.equal(await page.$$(".adm-inline-message.ok").then((x) => x.length), 0);
    await nav(page, 1);
    await page.waitForSelector(".adm-metrics");
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
    assert.equal(await page.$$(".adm-state").then((x) => x.length), 0);
  });
  await run("latest_search_result_preserved", "search-race", async (page) => {
    await nav(page, 3);
    await page.waitForSelector(".adm-search");
    await search(page, "older");
    await pending(page, "older");
    await search(page, "newer");
    await pending(page, "newer");
    await settle(page, "newer", 0, "Newer public result");
    await page.waitForFunction(() => document.body.textContent.includes("Newer public result"));
    await settle(page, "older", 0, "Older public result");
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(await page.evaluate(() => document.body.textContent.includes("Older public result")), false);
    assert.equal(await page.evaluate(() => document.body.textContent.includes("Newer public result")), true);
  });
  await run("detail_leave_ignores_late_401", "detail-leave", async (page) => {
    await nav(page, 2);
    await page.waitForSelector(".adm-name-link");
    await page.click(".adm-name-link");
    await pending(page, "load-1");
    await page.click(".adm-back");
    await page.waitForSelector(".adm-search");
    await settle(page, "load-1", 401);
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
  });
  await run("detail_language_reload_ignores_old_200", "detail-language", async (page) => {
    await nav(page, 2);
    await page.waitForSelector(".adm-name-link");
    await page.click(".adm-name-link");
    await pending(page, "load-1");
    await page.select(".adm-sidebar select", "ja");
    await pending(page, "load-2");
    await settle(page, "load-2", 0, "New public unit");
    await page.waitForFunction(() => document.body.textContent.includes("New public unit"));
    await settle(page, "load-1", 0, "Old public unit");
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(await page.evaluate(() => document.body.textContent.includes("Old public unit")), false);
    assert.equal(await page.evaluate(() => document.body.textContent.includes("New public unit")), true);
  });
  await run("csrf_failure_is_recoverable", "csrf", async (page) => {
    await page.click(".adm-admin button");
    await page.waitForSelector(".adm-state button");
    assert.equal(await page.$$("nav").then((x) => x.length), 0);
    assert.equal(
      await page.evaluate(() => document.body.textContent.includes("Administrator access required")),
      false,
    );
    assert.equal(await page.$$(".adm-state a").then((x) => x.length), 0);
  });
  await run("directory_mutation_late_error_ignored", "directory-mutation", async (page) => {
    await nav(page, 3);
    await page.waitForSelector(".adm-row-actions");
    await page.click(".adm-row-actions button");
    await pending(page, "mutation");
    await nav(page, 1);
    await settle(page, "mutation", 401);
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
    assert.equal(await page.$$(".adm-state").then((x) => x.length), 0);
  });
  await run("detail_mutation_late_error_ignored", "detail-mutation", async (page) => {
    await nav(page, 2);
    await page.waitForSelector(".adm-name-link");
    await page.click(".adm-name-link");
    await page.waitForSelector(".adm-org-name-display button:not(:disabled)");
    await page.type(".adm-unit-form input", "Public new node");
    await page.click(".adm-unit-form button");
    await pending(page, "mutation");
    await nav(page, 1);
    await settle(page, "mutation", 401);
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
    assert.equal(await page.$$(".adm-state").then((x) => x.length), 0);
  });
  await run("mutation_busy_survives_language_load", "mutation-busy", async (page) => {
    await nav(page, 2);
    await page.waitForSelector(".adm-name-link");
    await page.click(".adm-name-link");
    await page.waitForSelector(".adm-org-name-display button:not(:disabled)");
    await page.type(".adm-unit-form input", "Public new node");
    await page.click(".adm-unit-form button");
    await pending(page, "mutation");
    assert.equal(await page.$eval(".adm-unit-form button", (element) => element.disabled), true);
    await page.select(".adm-sidebar select", "ja");
    await page.waitForFunction(() => document.querySelector(".adm-unit-tree")?.textContent.includes("Language read completed"));
    assert.equal(await page.$eval(".adm-unit-form button", (element) => element.disabled), true);
    assert.equal(await page.evaluate(() => window.__adminReview.pending().includes("mutation")), true);
    assert.equal(await page.evaluate(() => window.__adminReview.mutations()), 1);
    await settle(page, "mutation");
    await page.waitForSelector(".adm-org-name-display button:not(:disabled)");
    assert.equal(await page.$eval(".adm-unit-form input", (element) => element.value), "");
  });
  await run("logout_lock_survives_language_read", "logout-busy", async (page) => {
    await page.click(".adm-admin button");
    await pending(page, "logout");
    await page.select(".adm-sidebar select", "ja");
    await page.waitForFunction(() => document.querySelector(".adm-metric strong")?.textContent === "7");
    assert.equal(await page.$eval(".adm-admin button", (element) => element.disabled), true);
    assert.equal(await page.evaluate(() => window.__adminReview.pending().includes("logout")), true);
    await settle(page, "logout", 503);
    await page.waitForSelector("[role=alert]");
    assert.equal(await page.$eval(".adm-admin button", (element) => element.disabled), false);
    assert.equal(await page.$$("nav").then((x) => x.length), 1);
  });

  for (const [kind, navigation, tab, cell, labels, attribute] of [
    ["orders", 4, 3, 0, ["Order APinvou order: 00000000…00101", "Order BPinvou order: 00000000…00102", "Order CPinvou order: 00000000…00103"]],
    ["adjustments", 4, 4, 2, ["Adjustment A", "Adjustment B", "Adjustment C"]],
    [
      "holds",
      4,
      5,
      0,
      [
        "00000000-0000-4000-8000-000000000801",
        "00000000-0000-4000-8000-000000000802",
        "00000000-0000-4000-8000-000000000803",
      ],
      "title",
    ],
    ["tokens", 5, 1, 0, ["Token A", "Token B", "Token C"]],
    ["audits", 7, null, 0, ["Audit A", "Audit B", "Audit C"]],
  ]) {
    await run(
      `${kind} retain three distinct rows while shared owners are reordered`,
      "rows",
      async (page) => {
        await nav(page, navigation);
        if (tab != null) {
          await page.waitForSelector(".adm-tabs");
          await page.click(`.adm-tabs button:nth-child(${tab})`);
        }
        await expectRows(page, cell, labels, attribute);
        if (kind === "orders") await expectRows(page, 3, ["ALIPAY", "WECHAT", "UNIONPAY"]);
        for (const order of [
          [2, 0, 1],
          [1, 2, 0],
        ]) {
          await page.click(".adm-actions > button");
          await expectRows(
            page,
            cell,
            order.map((index) => labels[index]),
            attribute,
          );
          if (kind === "orders") await expectRows(page, 3, order.map(index => ["ALIPAY", "WECHAT", "UNIONPAY"][index]));
        }
      },
    );
  }
  await run("payment_capabilities_disable_unsupported_actions", "limited-payment", async page => {
    await nav(page, 4); await page.waitForSelector(".adm-tabs"); await page.click(".adm-tabs button:nth-child(3)");
    await page.waitForSelector("tbody tr");
    assert.equal(await page.$$eval("tbody tr .adm-row-actions", rows => rows.every(row => !row.children[0].disabled && row.children[1].disabled && row.children[2].disabled)), true);
    await page.click(".adm-tabs button:nth-child(6)");
    assert.equal(await page.$(".adm-ops-form"), null);
  });
  await run("payment_repair_retains_owner_across_navigation", "payment-repair", async page => {
    page.on("dialog", dialog => void dialog.accept("Public review reason"));
    await nav(page, 4); await page.waitForSelector(".adm-tabs"); await page.click(".adm-tabs button:nth-child(3)");
    await page.waitForSelector("tbody tr .adm-row-actions button:nth-child(2)");
    await page.click("tbody tr .adm-row-actions button:nth-child(2)"); await pending(page, "repair");
    await page.click("tbody tr .adm-row-actions button:nth-child(2)");
    await nav(page, 3); await page.waitForSelector(".adm-search"); await nav(page, 4);
    await page.waitForSelector(".adm-tabs"); await page.click(".adm-tabs button:nth-child(3)");
    assert.equal(await page.$eval("tbody tr .adm-row-actions button:nth-child(2)", button => button.disabled), true);
    assert.equal(await page.evaluate(() => window.__adminReview.mutations()), 1);
    await settle(page, "repair", 503); await page.waitForFunction(() => !document.querySelector("tbody tr .adm-row-actions button:nth-child(2)").disabled);
    assert.equal(await page.$(".adm-inline-message.error"), null, "departed repair cannot set new-view error");
  });
  await run("payment_order_search_preserves_filters_and_handles_auth", "order-search-auth", async page => {
    await nav(page, 4); await page.waitForSelector(".adm-tabs"); await page.click(".adm-tabs button:nth-child(3)");
    await page.$eval(".adm-ops-form input", element => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(element,"Public order query"); element.dispatchEvent(new Event("input",{bubbles:true})); });
    await page.select(".adm-ops-form select", "MANUAL_REVIEW"); await page.click(".adm-ops-form button");
    await page.waitForFunction(() => !document.querySelector("nav"));
    assert.deepEqual(await page.evaluate(() => window.__adminReview.orderQueries().at(-1)), {q:"Public order query",status:"MANUAL_REVIEW"});
  });
  await run("payment_order_filter_survives_completion_refresh_and_remount", "order-filter-completion", async page => {
    await nav(page, 4); await page.waitForSelector(".adm-tabs"); await page.click(".adm-tabs button:nth-child(3)");
    await page.$eval(".adm-ops-form input", element => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(element,"Order A"); element.dispatchEvent(new Event("input",{bubbles:true})); });
    await page.click(".adm-ops-form button"); await page.waitForFunction(() => document.querySelectorAll("tbody tr").length === 1);
    await page.click("tbody tr .adm-row-actions button:first-child"); await pending(page, "payment-sync");
    await settle(page, "payment-sync");
    await page.waitForFunction(() => window.__adminReview.orderQueries().length >= 4 && document.querySelectorAll("tbody tr").length === 1);
    assert.deepEqual(await page.evaluate(() => window.__adminReview.orderQueries().slice(1).map(query => query.q)), ["Order A", "Order A", "Order A"]);
    await page.click(".adm-actions > button"); await page.waitForFunction(() => window.__adminReview.orderQueries().length >= 5 && document.querySelectorAll("tbody tr").length === 1);
    await nav(page, 3); await page.waitForSelector(".adm-search"); await nav(page, 4); await page.waitForSelector(".adm-tabs"); await page.click(".adm-tabs button:nth-child(3)");
    assert.equal(await page.$eval(".adm-ops-form input", input => input.value), "Order A");
    assert.equal(await page.$$eval("tbody tr", rows => rows.length), 1);
  });
  await run('metering_safe_integer_and_price_draft_real_form', 'metering', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(7)');
    await page.waitForFunction(() => document.querySelectorAll('.adm-panel form').length === 4);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('00000000-0000-4000-8000-000000002001')), true, 'Published unit UUID must be available for formula version selection');
    await input(page, '.adm-panel form:nth-of-type(1) input', 'OUTPUT');
    await page.click('.adm-panel form:nth-of-type(1) button');
    await page.waitForFunction(() => window.__adminReview.commands().some(item => item.kind === 'metering'));
    assert.deepEqual(await page.evaluate(() => window.__adminReview.commands().find(item => item.kind === 'metering').input), { unit_code:'OUTPUT', minimum_precision:1, price_denominator:1000000, rounding_rule:'CEILING' });
    const forms = await page.$$('.adm-panel form');
    const priceForm = forms.at(-1);
    await priceForm.$eval('input', element => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(element,'controlled-model'); element.dispatchEvent(new Event('input',{bubbles:true})); });
    const formulaSelect = await priceForm.$('label:nth-child(3) select');
    await formulaSelect.select('00000000-0000-4000-8000-000000002201');
    await priceForm.$eval('input[type=date]', element => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(element,'2030-01-01'); element.dispatchEvent(new Event('input',{bubbles:true})); });
    await priceForm.$eval('label:last-of-type input', element => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(element,'9007199254740992'); element.dispatchEvent(new Event('input',{bubbles:true})); });
    await priceForm.$eval('button', element => element.click()); await page.waitForSelector('[role=alert]');
    assert.equal(await page.evaluate(() => window.__adminReview.commands().filter(item => item.kind === 'pricing').length), 0);
    await priceForm.$eval('label:last-of-type input', element => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(element,'0'); element.dispatchEvent(new Event('input',{bubbles:true})); });
    await priceForm.$eval('button', element => element.click());
    await page.waitForFunction(() => window.__adminReview.commands().some(item => item.kind === 'pricing'));
    const draft = await page.evaluate(() => window.__adminReview.commands().find(item => item.kind === 'pricing').input);
    assert.equal(draft.scope_id, 'controlled-model'); assert.equal(draft.units[0].fixed_price_per_denominator, 0); assert.equal(draft.expected_etag, 1);
  });
  await run('metering_partial_403_preserves_protocol_access', 'metering-denied', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(7)');
    await page.waitForSelector('[role=alert]');
    assert.equal(await page.$$('nav').then(value => value.length), 1);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('CHAT · v1')), true);
    assert.equal(await page.$$eval('form', forms => forms.length), 3);
  });
  for (const [status, previewSource] of [[403, 'formula'], [503, 'formula'], [403, 'price']]) {
    await run(`protocol_refresh_${status}_clears_formula_and_${previewSource}_preview`, `protocol-access-${previewSource}`, async page => {
      await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(7)');
      await page.waitForFunction(() => [...document.querySelectorAll('select option')].some(item => item.textContent.includes('PRIVATE_FORMULA_MARKER')));
      await page.evaluate(() => {
        const select = [...document.querySelectorAll('select')].find(item => [...item.options].some(option => option.value.endsWith('002201')));
        select.value = [...select.options].find(option => option.value.endsWith('002201')).value;
        select.dispatchEvent(new Event('change', { bubbles:true }));
      });
      await page.evaluate(source => {
        const panels = [...document.querySelectorAll('.adm-panel > article')];
        const panel = panels.find(item => item.querySelector('h3')?.textContent === (source === 'formula' ? 'Protocols and formulas' : 'Price versions'));
        const button = [...panel.querySelectorAll('button')].find(item => item.textContent === 'Preview');
        button.click();
      }, previewSource);
      await page.waitForSelector('.adm-panel > article > pre');
      await page.evaluate(status => window.__adminReview.protocolAccess(status), status);
      await page.click('.adm-section-head button');
      await page.waitForSelector('[role=alert]');
      assert.equal(await page.$$eval('select option', options => options.some(item => item.textContent.includes('PRIVATE_FORMULA_MARKER'))), false, 'Failed protocol read must remove cached formulas from price choices');
      assert.equal(await page.$('.adm-panel > article > pre'), null, 'Related preview must not survive failed authority read');
      assert.equal(await page.$$('nav').then(items => items.length), 1, 'Other authorized areas remain available');
      assert.equal(await page.evaluate(() => window.__adminReview.commands().length), 0, 'Read failure must not issue a mutation');
      await page.evaluate(() => window.__adminReview.protocolAccess(0));
      await page.click('.adm-section-head button');
      await page.waitForFunction(() => !document.querySelector('[role=alert]') && [...document.querySelectorAll('select option')].some(item => item.textContent.includes('PRIVATE_FORMULA_MARKER')));
    });
  }
  await run('protocol_preview_from_left_view_cannot_restore_cached_data', 'protocol-access-late-preview', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(7)');
    await page.waitForFunction(() => document.body.textContent.includes('PRIVATE_FORMULA_MARKER'));
    await page.evaluate(() => [...document.querySelectorAll('button')].find(item => item.textContent === 'Preview').click());
    await pending(page, 'formula-preview');
    await page.click('.adm-tabs button:first-child');
    await settle(page, 'formula-preview');
    await page.click('.adm-tabs button:nth-child(7)');
    await page.waitForFunction(() => document.body.textContent.includes('PRIVATE_FORMULA_MARKER'));
    assert.equal(await page.$('.adm-panel > article > pre'), null, 'Late preview must not enter a new view lifetime');
  });
  await run('pricing_new_client_pending_does_not_display_prior_resources_or_preview', 'protocol-access-client', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(7)');
    await page.waitForFunction(() => document.body.textContent.includes('PRIVATE_FORMULA_MARKER'));
    await page.evaluate(() => [...document.querySelectorAll('button')].find(item => item.textContent === 'Preview').click());
    await page.waitForSelector('.adm-panel > article > pre');
    await page.evaluate(() => window.__adminReview.replaceClient());
    await pending(page, 'next-client-protocols');
    assert.equal(await page.evaluate(() => document.body.textContent.includes('PRIVATE_FORMULA_MARKER')), false, 'Prior client formulas/prices/preview must disappear before the next client replies');
    assert.equal(await page.$('.adm-panel > article > pre'), null);
    await settle(page, 'next-client-units'); await settle(page, 'next-client-prices'); await settle(page, 'next-client-protocols');
    await page.waitForFunction(() => !document.querySelector('.adm-section-head button').disabled);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('PRIVATE_FORMULA_MARKER')), false);
  });
  await run('hold_evidence_keeps_exact_money_and_unknown_consumption', 'hold-evidence', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(5)');
    await page.waitForSelector('tbody tr .adm-row-actions button'); await page.click('tbody tr .adm-row-actions button');
    await page.waitForFunction(() => document.body.textContent.includes('9223372036854775808'));
    assert.equal(await page.evaluate(() => document.body.textContent.includes('"actual_microcredits": null')), true);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('LEASE_EXPIRED')), true);
  });
  await run('late_review_shows_immutable_evidence_and_claims', 'late-review', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(10)');
    await page.waitForSelector('tbody tr button'); await page.click('tbody tr button');
    await page.waitForFunction(() => document.body.textContent.includes('public-controlled-evidence') && document.body.textContent.includes('ENTER_REVIEW'));
    await page.click('.adm-row-actions button:first-child');
    await page.waitForFunction(() => window.__adminReview.commands().some(item => item.kind === 'late-claim'));
    assert.equal(await page.$$('nav').then(value => value.length), 1);
  });
  await run('reconciliation_command_retains_all_four_evidence_snapshots', 'reconciliation-cases', async page => {
    page.on('dialog', dialog => void dialog.accept('Controlled review reason'));
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(6)');
    await page.waitForSelector('tbody tr button'); await page.click('tbody tr button');
    await page.waitForSelector('.adm-row-actions button'); await page.click('.adm-row-actions button:first-child');
    assert.equal(await page.evaluate(() => ['provider-evidence','internal-evidence','refund-evidence','ledger-evidence'].every(value => document.body.textContent.includes(value))), true);
    await page.click('.adm-row-actions button:nth-child(2)');
    await page.waitForFunction(() => window.__adminReview.commands().some(item => item.kind === 'reconciliation-command') && document.body.textContent.includes(window.__adminReview.ids.user));
    const command = await page.evaluate(() => window.__adminReview.commands().find(item => item.kind === 'reconciliation-command'));
    assert.deepEqual(command.input, { action:'CLAIM', reason:'Controlled review reason' });
  });
  await run('file_import_rejects_unsafe_money_and_uses_exact_import_receipt', 'reconciliation-file', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(6)');
    await page.waitForSelector('textarea'); await input(page, '.adm-ops-form input[type=date]', '2030-01-01');
    const setItems = async amount => page.$eval('textarea', (element, amount) => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set.call(element, JSON.stringify([{ amount_fen:amount }])); element.dispatchEvent(new Event('input',{bubbles:true}));
    }, amount);
    await setItems(9007199254740992); await page.click('.adm-ops-form button'); await page.waitForSelector('.adm-inline-message.error');
    assert.equal(await page.evaluate(() => window.__adminReview.commands().filter(item => item.kind === 'bill-import').length), 0);
    await setItems(100); await page.click('.adm-ops-form button');
    await page.waitForFunction(() => document.querySelector('textarea').value === '[]');
    const command = await page.evaluate(() => window.__adminReview.commands().find(item => item.kind === 'bill-import'));
    assert.equal(command.channel, 'ALIPAY'); assert.equal(command.businessDate, '2030-01-01'); assert.deepEqual(command.items, [{ amount_fen:100 }]);
  });
  await run('degraded_query_is_not_described_as_supplier_bill_reconciliation', 'reconciliation-degraded', async page => {
    await nav(page, 4); await page.waitForSelector('.adm-tabs'); await page.click('.adm-tabs button:nth-child(6)');
    assert.equal(await page.evaluate(() => document.body.textContent.includes('QUERY_DEGRADED') && document.body.textContent.includes('supplier bill reconciliation is not claimed')), true);
    assert.equal(await page.$('.adm-ops-form'), null);
  });
  console.log(
    `Admin UI: ${passed} PASS, ${failures.length} FAIL, 0 SKIP. Real React/browser with controlled public clients; not BFF/PG/Keycloak, Safari, or production E2E.`,
  );
  assert.equal(failures.length, 0, `Failing scenarios: ${failures.join(", ")}`);
} finally {
  try {
    const shutdown = await Promise.allSettled([browser?.close(), vite?.close()]);
    const resolved = path.resolve(tempRoot);
    assert.ok(
      resolved.startsWith(`${path.resolve(os.tmpdir())}${path.sep}`) &&
        path.basename(resolved).startsWith("pinvou-admin-ui-"),
      "Cleanup must stay inside its owned temporary directory",
    );
    fs.rmSync(resolved, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
    for (const result of shutdown) {
      if (result.status === "rejected") {
        console.error(`Admin UI resource shutdown failed: ${result.reason}`);
        process.exitCode = 1;
      }
    }
  } catch (error) {
    console.error(`Admin UI cleanup failed: ${error.message}`);
    process.exitCode = 1;
  }
}
