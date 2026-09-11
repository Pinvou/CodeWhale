import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const statsHtml = fs.readFileSync(path.join(__dirname, "../web/stats.html"), "utf8");
const sortingScript = statsHtml.match(/<script id="stats-table-sorting">([\s\S]*?)<\/script>/)?.[1];

function renderTable(headings, rows) {
  const table = `<table><thead><tr>${headings.map((heading) => `<th>${heading}</th>`).join("")}</tr></thead><tbody>${rows.map((row) => `<tr>${row.map((cell) => `<td>${cell}</td>`).join("")}</tr>`).join("")}</tbody></table>`;
  const dom = new JSDOM(table, { runScripts: "outside-only" });
  dom.window.eval(sortingScript);
  return dom;
}

const firstColumn = (dom) => Array.from(
  dom.window.document.querySelectorAll("tbody tr"),
  (row) => row.cells[0].textContent,
);
const flush = (dom) => new Promise((resolve) => dom.window.setTimeout(resolve, 0));

test("single-row tables enable sorting only after a second row is added", async () => {
  const dom = renderTable(["调用次数"], [["2 次"]]);
  const button = dom.window.document.querySelector(".stats-sort-trigger");
  assert.equal(button.disabled, true);
  assert.equal(button.title, "");
  assert.equal(dom.window.document.querySelector("th").hasAttribute("aria-sort"), false);

  dom.window.document.querySelector("tbody").insertAdjacentHTML("beforeend", "<tr><td>5 次</td></tr>");
  await flush(dom);
  assert.equal(button.disabled, false);
  assert.equal(dom.window.document.querySelector("th").getAttribute("aria-sort"), "none");
});

test("single-row composite tables keep their sub-property controls disabled", () => {
  const dom = renderTable(["总耗时 P50/P90"], [["10 / 100 ms"]]);
  const header = dom.window.document.querySelector("th");
  const buttons = dom.window.document.querySelectorAll(".stats-sort-trigger");
  assert.equal(header.classList.contains("stats-sortable-column"), false);
  assert.deepEqual(Array.from(buttons, (button) => button.disabled), [true, true]);
  assert.deepEqual(Array.from(buttons, (button) => button.title), ["", ""]);
  assert.match(statsHtml, /th:not\(\.stats-sortable-column\) \.stats-sort-options \{ display:none; \}/);
});

test("numeric values sort in both directions and keep empty values last", () => {
  const dom = renderTable(["成功率"], [["12.5%"], ["—"], ["80%"]]);
  const button = dom.window.document.querySelector(".stats-sort-trigger");
  button.click();
  assert.deepEqual(firstColumn(dom), ["80%", "12.5%", "—"]);
  button.click();
  assert.deepEqual(firstColumn(dom), ["12.5%", "80%", "—"]);
});

test("unavailable statistical values always remain after valid values", () => {
  const dom = renderTable(["成功率"], [["样本不足"], ["12.5%"], ["暂无数据"], ["80%"]]);
  const button = dom.window.document.querySelector(".stats-sort-trigger");
  button.click();
  assert.deepEqual(firstColumn(dom), ["80%", "12.5%", "样本不足", "暂无数据"]);
  button.click();
  assert.deepEqual(firstColumn(dom), ["12.5%", "80%", "样本不足", "暂无数据"]);
});

test("combined quantiles rank independently and retain sorting after refresh", async () => {
  const dom = renderTable(["总耗时 P50/P90"], [["10 / 100 ms"], ["20 / 50 ms"]]);
  const buttons = dom.window.document.querySelectorAll(".stats-sort-trigger");
  assert.deepEqual(Array.from(buttons, (button) => button.textContent), ["P50", "P90"]);

  buttons[1].click();
  assert.deepEqual(firstColumn(dom), ["10 / 100 ms", "20 / 50 ms"]);

  dom.window.document.querySelector("tbody").innerHTML = "<tr><td>30 / 40 ms</td></tr><tr><td>5 / 200 ms</td></tr>";
  await flush(dom);
  assert.deepEqual(firstColumn(dom), ["5 / 200 ms", "30 / 40 ms"]);
});

test("failure and cancellation counts expose separate ranking controls", () => {
  const dom = renderTable(["失败/取消"], [["8 / 1"], ["2 / 7"]]);
  const buttons = dom.window.document.querySelectorAll(".stats-sort-trigger");
  assert.deepEqual(Array.from(buttons, (button) => button.textContent), ["失败", "取消"]);
  buttons[1].click();
  assert.deepEqual(firstColumn(dom), ["2 / 7", "8 / 1"]);
});

test("selection sources expose one ranking control per observed source", async () => {
  const dom = renderTable(["选择来源"], [["auto 8 · explicit 1"], ["auto 2 · explicit 7"]]);
  await flush(dom);
  const buttons = dom.window.document.querySelectorAll(".stats-sort-trigger");
  assert.deepEqual(Array.from(buttons, (button) => button.textContent), ["auto", "explicit"]);
  buttons[1].click();
  assert.deepEqual(firstColumn(dom), ["auto 2 · explicit 7", "auto 8 · explicit 1"]);
});

test("device details retain their existing state-driven sorting", () => {
  const dom = new JSDOM('<table><thead><tr><th class="sortable sort-active desc" data-sort="turns_7d" aria-sort="descending">近 7 日对话</th></tr></thead><tbody id="deviceRows"><tr><td>8</td></tr><tr><td>2</td></tr></tbody></table>', { runScripts: "outside-only" });
  dom.window.eval(sortingScript);
  const header = dom.window.document.querySelector("th");
  assert.equal(header.dataset.sort, "turns_7d");
  assert.equal(header.classList.contains("sort-active"), true);
  assert.equal(header.getAttribute("aria-sort"), "descending");
  assert.equal(dom.window.document.querySelector(".stats-sort-trigger"), null);
});
