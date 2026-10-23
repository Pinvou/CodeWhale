import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";

const html = readFileSync(new URL("../web/stats.html", import.meta.url), "utf8");
const tick = () => new Promise(resolve => setTimeout(resolve, 35));
const fingerprint = "a".repeat(64);
function dashboard(role = "operations_admin") {
  const requests = [];
  let detailGate = null;
  const dom = new JSDOM(html, { runScripts: "dangerously", url: "https://stats.invalid/pinvou3/stats", beforeParse(window) {
    window.setInterval = () => 1; window.clearInterval = () => {};
    window.fetch = async (url, options = {}) => {
      const path = String(url); requests.push({path,options});
      let value = { rows: [], metrics: {}, devices: [] };
      if (path.endsWith("/session")) value = { role };
      if (path.includes("/behavior/failures")) value = { detail_allowed: role === "operations_admin", rows: [{ fingerprint,code:"stream_unexpected_eof",component:"model",stage:"model",count:21,installations:21,versions:{"0.10.12":21},first_seen:Date.now(),last_seen:Date.now(),status:"open",fixed_version:"" }] };
      if (path.includes("/diagnostics/samples")) value = { incidents:[{id:"case_fixture",at:Date.now(),version:"0.10.12",code:"stream_unexpected_eof"}] };
      if (path.includes("/diagnostics/incident")) {
        if (detailGate) await detailGate;
        value={incident:{diagnostic_code:"stream_unexpected_eof"},related:[],reports:[{notes:'<img src=x onerror="window.executed=true">',attachments:[{kind:"text",content:"reproduce empty stream"}]}]};
      }
      return {ok:true,status:200,json:async()=>value};
    };
  }});
  return {dom,requests,delayDetail:gate=>{detailGate=gate;}};
}

test("admin filters drill down safely, save status, and fence delayed evidence after logout", async () => {
  const {dom,requests,delayDetail}=dashboard();
  try {
    const {document,Event}=dom.window; await tick();
    document.querySelector("#failureVersion").value="0.10.12";
    document.querySelector("#failureFilters").dispatchEvent(new Event("submit",{cancelable:true})); await tick();
    assert(requests.some(r=>r.path.includes("app_version=0.10.12")));
    document.querySelector("[data-failure-group]").click(); await tick();
    assert(requests.some(r=>r.path.includes("/diagnostics/samples")&&r.path.includes("app_version=0.10.12")));
    document.querySelector("[data-failure-incident]").click(); await tick();
    assert.match(document.querySelector("#failureEvidence").textContent, /reproduce empty stream/);
    assert.equal(document.querySelector("#failureEvidence img"),null); assert.equal(dom.window.executed,undefined);
    document.querySelector("#failureIssueStatus").value="fixed"; document.querySelector("#failureFixedVersion").value="0.10.13";
    document.querySelector("#failureIssueForm").dispatchEvent(new Event("submit",{cancelable:true})); await tick();
    const update=requests.find(r=>r.path.endsWith("/diagnostics/issue"));
    assert.equal(update.options.headers["X-Pinvou-Diagnostics"],"1"); assert.equal(JSON.parse(update.options.body).issue.fixed_version,"0.10.13");
    document.querySelector("[data-failure-group]").click(); await tick();
    let release; const gate=new Promise(resolve=>{release=resolve;}); delayDetail(gate);
    document.querySelector("[data-failure-incident]").click(); await tick(); document.querySelector("#logout").click(); await tick(); release(); await tick();
    assert.equal(document.querySelector("#failureEvidence").textContent,"");
    assert.equal(document.querySelector("#failureRows").children.length,0);
    assert.equal(document.querySelector("#failureAdmin").classList.contains("hidden"),true);
  } finally {dom.window.close();}
});

test("analysts receive aggregates and cannot trigger admin evidence through a forged button",async()=>{
  const {dom,requests}=dashboard("behavior_analyst");
  try {
    const {document,Event}=dom.window; await tick();
    document.querySelector("#failureFilters").dispatchEvent(new Event("submit",{cancelable:true})); await tick();
    assert.equal(document.querySelector("[data-failure-group]"),null);
    const button=document.createElement("button");button.dataset.failureGroup=fingerprint;document.querySelector("#failureRows").append(button);button.click();await tick();
    assert.equal(requests.some(r=>r.path.includes("/diagnostics/")),false);
    assert.equal(document.querySelector("#failureAdmin").classList.contains("hidden"),true);
  }finally{dom.window.close();}
});
