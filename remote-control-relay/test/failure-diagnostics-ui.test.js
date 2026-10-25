import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { JSDOM } from "jsdom";

const html = readFileSync(new URL("../web/stats.html", import.meta.url), "utf8");
const tick = () => new Promise(resolve => setTimeout(resolve, 35));
const fingerprint = "a".repeat(64);
function dashboard(role = "operations_admin", timelineContext) {
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
        if(timelineContext)value.timeline_context=timelineContext;
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

test("incident evidence explains a bounded timeline and clears coverage on logout", async () => {
  const {dom}=dashboard("operations_admin",{total_events:120,returned_events:32,truncated:true,
    incident_included:true,task_terminal_included:true});
  try {
    const {document}=dom.window; await tick();
    document.querySelector('[data-view="diagnostics"]').click();await tick();
    document.querySelector('[data-failure-group]').click();await tick();
    document.querySelector('[data-failure-incident]').click();await tick();
    assert.match(document.querySelector('#failureTimelineStatus').textContent,/32 \/ 120/);
    assert.match(document.querySelector('#failureTimelineStatus').textContent,/已截断/);
    assert.match(document.querySelector('#failureTimelineStatus').textContent,/包含任务终态/);
    assert.match(document.querySelector('#failureEvidence').textContent,/timeline_context/);
    document.querySelector('#logout').click();await tick();
    assert.equal(document.querySelector('#failureTimelineStatus').textContent,'');
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

for (const role of ["operations_admin", "behavior_analyst"]) {
  test(`${role} can open diagnostics as a peer tab and refresh with its filters`, async () => {
    const {dom,requests}=dashboard(role);
    try {
      const {document}=dom.window; await tick();
      const tab=document.querySelector('[data-view="diagnostics"]');
      assert.equal(tab.hidden,false);
      assert.equal(document.querySelector('#behaviorView #failurePanel'),null);
      assert.equal(document.querySelector('#diagnosticsView #failurePanel')?.id,'failurePanel');
      assert.equal(requests.some(r=>r.path.includes('/behavior/failures')),false);
      tab.click(); await tick();
      assert.equal(tab.getAttribute('aria-selected'),'true');
      assert.equal(document.querySelector('#diagnosticsView').classList.contains('hidden'),false);
      assert.equal(document.querySelector('#behaviorView').classList.contains('hidden'),true);
      assert.equal(document.querySelector('#devicesView').classList.contains('hidden'),true);
      assert.equal(document.querySelector('#pageTitle').textContent,'故障诊断');
      assert.equal(dom.window.getComputedStyle(document.querySelector('#failureAdmin')).display,'none');
      assert.equal(requests.filter(r=>r.path.includes('/behavior/failures')).length,1);
      document.querySelector('#failureVersion').value='0.10.12';
      document.querySelector('#refreshButton').click(); await tick();
      assert.equal(tab.getAttribute('aria-selected'),'true');
      assert(requests.some(r=>r.path.includes('/behavior/failures')&&r.path.includes('app_version=0.10.12')));
      assert.equal(document.querySelector('#failurePanel').getAttribute('aria-busy'),'false');
      document.querySelector('[data-view="behavior"]').click(); await tick();
      assert.equal(document.querySelector('#diagnosticsView').classList.contains('hidden'),true);
      assert.equal(document.querySelector('#behaviorView').classList.contains('hidden'),false);
    } finally {dom.window.close();}
  });
}

test("diagnostics refresh reports its own service failure and clears busy state", async () => {
  const {dom}=dashboard();
  try {
    const {document}=dom.window; await tick();
    const originalFetch=dom.window.fetch;
    dom.window.fetch=(url,options)=>String(url).includes('/behavior/failures')
      ? Promise.resolve({ok:false,status:503}) : originalFetch(url,options);
    document.querySelector('[data-view="diagnostics"]').click(); await tick();
    assert.match(document.querySelector('#failureStatus').textContent,/加载失败/);
    document.querySelector('#refreshButton').click(); await tick();
    assert.match(document.querySelector('#refreshButton').textContent,/部分失败/);
    assert.equal(document.querySelector('#failurePanel').getAttribute('aria-busy'),'false');
    assert.equal(document.querySelector('#failureRows').children.length,0);
  } finally {dom.window.close();}
});

test("superseded manual refresh cannot label a newer filter result as failed", async () => {
  const {dom}=dashboard();
  try {
    const {document,Event}=dom.window;await tick();
    document.querySelector('[data-view="diagnostics"]').click();await tick();
    const original=dom.window.fetch;let release;let count=0;
    dom.window.fetch=async(url,options)=>{
      if(String(url).includes('/behavior/failures')&&++count===1)await new Promise(resolve=>{release=resolve;});
      return original(url,options);
    };
    document.querySelector('#refreshButton').textContent='↻ 部分失败，重试';
    document.querySelector('#refreshButton').click();await tick();assert.equal(typeof release,'function');
    document.querySelector('#failureVersion').value='0.10.14';
    document.querySelector('#failureFilters').dispatchEvent(new Event('submit',{cancelable:true}));await tick();
    assert(document.querySelector('[data-failure-group]'));release();await tick();
    assert.doesNotMatch(document.querySelector('#refreshButton').textContent,/部分失败/);
    assert.equal(document.querySelector('#refreshButton').textContent,'↻ 刷新');
    assert(document.querySelector('[data-failure-group]'));
  } finally {dom.window.close();}
});

test("old sample 403 is fenced while current sample 403 still clears privileged evidence", async () => {
  const {dom}=dashboard();
  try {
    const {document}=dom.window;await tick();
    document.querySelector('[data-view="diagnostics"]').click();await tick();
    const original=dom.window.fetch;let release;let count=0;
    dom.window.fetch=async(url,options)=>{
      if(String(url).includes('/diagnostics/samples')) {
        count++;
        if(count===1){await new Promise(resolve=>{release=resolve;});return {ok:false,status:403};}
        if(count===3)return {ok:false,status:403};
      }
      return original(url,options);
    };
    document.querySelector('[data-failure-group]').click();await tick();
    document.querySelector('[data-failure-group]').click();await tick();
    assert(document.querySelector('[data-failure-incident]'));
    release();await tick();assert(document.querySelector('[data-failure-incident]'));
    document.querySelector('[data-failure-group]').click();await tick();
    assert.equal(document.querySelector('#failureRows').children.length,0);
    assert.equal(document.querySelector('#failureAdmin').classList.contains('hidden'),true);
    assert.match(document.querySelector('#failureStatus').textContent,/权限已改变/);
  } finally {dom.window.close();}
});

test("old query 401 cannot hide newer data while current query 401 returns to login", async () => {
  const {dom}=dashboard();
  try {
    const {document,Event}=dom.window;await tick();
    const original=dom.window.fetch;let release;let count=0;
    dom.window.fetch=async(url,options)=>{
      if(String(url).includes('/behavior/failures')) {
        count++;
        if(count===1){await new Promise(resolve=>{release=resolve;});return {ok:false,status:401};}
        if(count===3)return {ok:false,status:401};
      }
      return original(url,options);
    };
    const query=()=>document.querySelector('#failureFilters').dispatchEvent(new Event('submit',{cancelable:true}));
    document.querySelector('[data-view="diagnostics"]').click();await tick();query();await tick();
    release();await tick();assert(document.querySelector('[data-failure-group]'));
    assert.equal(document.querySelector('#loginLayer').classList.contains('hidden'),true);
    query();await tick();assert.equal(document.querySelector('#loginLayer').classList.contains('hidden'),false);
    assert.equal(document.querySelector('#failureRows').children.length,0);
  } finally {dom.window.close();}
});

for (const role of ["operations_admin", "behavior_analyst"]) {
  test(`${role} diagnostics refresh is independent of other dashboard endpoints`, async () => {
    const {dom,requests}=dashboard(role);
    try {
      const {document}=dom.window;await tick();
      document.querySelector('[data-view="diagnostics"]').click();await tick();
      const original=dom.window.fetch;
      dom.window.fetch=(url,options)=>String(url).includes('/behavior/failures')||String(url).endsWith('/session')
        ? original(url,options) : Promise.resolve({ok:false,status:503});
      const before=requests.filter(row=>row.path.includes('/behavior/failures')).length;
      document.querySelector('#refreshButton').click();await tick();
      assert.equal(requests.filter(row=>row.path.includes('/behavior/failures')).length,before+1);
      assert.equal(document.querySelector('#refreshButton').textContent,'✓ 已更新');
      assert(document.querySelector('#failureRows').children.length>0);
    } finally {dom.window.close();}
  });

  test(`${role} resumes the active diagnostics query after session expiry and login`, async () => {
    const {dom,requests}=dashboard(role);
    try {
      const {document,Event}=dom.window;await tick();
      const original=dom.window.fetch;let expired=true;
      dom.window.fetch=(url,options)=>String(url).includes('/behavior/failures')&&expired
        ? Promise.resolve({ok:false,status:401}) : original(url,options);
      document.querySelector('#failureVersion').value='0.10.12';
      document.querySelector('[data-view="diagnostics"]').click();await tick();
      assert.equal(document.querySelector('#loginLayer').classList.contains('hidden'),false);
      assert.doesNotMatch(document.querySelector('#failureStatus').textContent,/加载中/);
      assert.equal(document.querySelector('#failurePanel').getAttribute('aria-busy'),'false');
      expired=false;
      document.querySelector('#password').value='fixture-password';
      document.querySelector('#loginForm').dispatchEvent(new Event('submit',{cancelable:true}));await tick();
      assert.equal(document.querySelector('#loginLayer').classList.contains('hidden'),true);
      assert.equal(document.querySelector('[data-view="diagnostics"]').getAttribute('aria-selected'),'true');
      assert.equal(document.querySelector('#failureVersion').value,'0.10.12');
      assert.equal(requests.filter(r=>r.path.includes('/behavior/failures')&&r.path.includes('app_version=0.10.12')).length,1);
      assert(document.querySelector('#failureRows').children.length>0);
      assert.doesNotMatch(document.querySelector('#failureStatus').textContent,/加载中/);
      assert.equal(document.querySelector('#failurePanel').getAttribute('aria-busy'),'false');
      assert.equal(Boolean(document.querySelector('[data-failure-group]')),role==='operations_admin');
    } finally {dom.window.close();}
  });
}
