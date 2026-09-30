#!/usr/bin/env node
import { createHash } from "node:crypto";
import { readFile, unlink, writeFile } from "node:fs/promises";
import { resolve } from "node:path";

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

function argument(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

const browserURL = argument("--cdp-url");
const output = argument("--output");
const phase = argument("--phase") || "settings";
const coordinationId = argument("--coordination-id");
const phases = new Set(["settings", "controlled-panel", "connected-chat", "approval-allow", "approval-deny", "attachment", "artifact"]);
if (!browserURL || !output || !phases.has(phase)) fail("ui_smoke_arguments_missing");
const coordinatedPhase = new Set(["approval-allow", "approval-deny", "attachment"]).has(phase);
if ((coordinatedPhase && !/^[a-f0-9]{64}$/u.test(coordinationId || "")) || (!coordinatedPhase && coordinationId)) fail("ui_smoke_coordination_invalid");
const parsed = new URL(browserURL);
if (parsed.protocol !== "http:" || !/^(?:127\.0\.0\.1|\[::1\]|localhost)$/u.test(parsed.hostname)) fail("ui_smoke_cdp_not_loopback");

const targetsResponse = await fetch(new URL("/json/list", parsed));
if (!targetsResponse.ok) fail("ui_smoke_cdp_targets_unavailable");
const targets = await targetsResponse.json();
const target = targets.find((candidate) => candidate.type === "page" && (
  candidate.url?.startsWith("tauri://")
  || candidate.url?.startsWith("http://tauri.localhost")
  || candidate.url?.startsWith("https://tauri.localhost")
));
if (!target?.webSocketDebuggerUrl) fail("ui_smoke_tauri_page_missing");

const socket = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((resolvePromise, reject) => {
  const timeout = setTimeout(() => reject(Object.assign(new Error("ui_smoke_cdp_connect_timeout"), { code: "ui_smoke_cdp_connect_timeout" })), 10_000);
  socket.addEventListener("open", () => { clearTimeout(timeout); resolvePromise(); }, { once: true });
  socket.addEventListener("error", () => { clearTimeout(timeout); reject(Object.assign(new Error("ui_smoke_cdp_connect_failed"), { code: "ui_smoke_cdp_connect_failed" })); }, { once: true });
});

let nextId = 1;
const pending = new Map();
socket.addEventListener("message", (event) => {
  const message = JSON.parse(String(event.data));
  if (!message.id || !pending.has(message.id)) return;
  const { resolve: resolvePromise, reject } = pending.get(message.id);
  pending.delete(message.id);
  if (message.error) reject(Object.assign(new Error("ui_smoke_cdp_command_failed"), { code: "ui_smoke_cdp_command_failed" }));
  else resolvePromise(message.result);
});

function command(method, params = {}) {
  const id = nextId;
  nextId += 1;
  return new Promise((resolvePromise, reject) => {
    const timeout = setTimeout(() => {
      pending.delete(id);
      reject(Object.assign(new Error("ui_smoke_cdp_command_timeout"), { code: "ui_smoke_cdp_command_timeout" }));
    }, 15_000);
    pending.set(id, {
      resolve(value) { clearTimeout(timeout); resolvePromise(value); },
      reject(error) { clearTimeout(timeout); reject(error); },
    });
    socket.send(JSON.stringify({ id, method, params }));
  });
}

async function evaluate(expression) {
  const result = await command("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) fail("ui_smoke_page_evaluation_failed");
  return result.result?.value;
}

async function waitVisible(selector, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const visible = await evaluate(`(() => { const e = document.querySelector(${JSON.stringify(selector)}); if (!e) return false; const s = getComputedStyle(e); const r = e.getBoundingClientRect(); return s.display !== 'none' && s.visibility !== 'hidden' && r.width > 0 && r.height > 0; })()`);
    if (visible) return;
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
  }
  fail("ui_smoke_required_surface_missing");
}

async function waitFor(predicate, code, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
  }
  fail(code);
}

async function click(selector) {
  const point = await evaluate(`(() => { const e = document.querySelector(${JSON.stringify(selector)}); if (!e || e.disabled) return null; const r=e.getBoundingClientRect(); if (r.width<=0 || r.height<=0) return null; return {x:r.left+r.width/2,y:r.top+r.height/2}; })()`);
  if (!point) fail("ui_smoke_action_unavailable");
  await command("Input.dispatchMouseEvent", { type: "mouseMoved", x: point.x, y: point.y });
  await command("Input.dispatchMouseEvent", { type: "mousePressed", x: point.x, y: point.y, button: "left", clickCount: 1 });
  await command("Input.dispatchMouseEvent", { type: "mouseReleased", x: point.x, y: point.y, button: "left", clickCount: 1 });
}

async function exactCoordinatedSelector(testId) {
  const selector = `[data-testid="${testId}"][data-coordination-id="${coordinationId}"]`;
  const counts = await evaluate(`(() => ({ exact: document.querySelectorAll(${JSON.stringify(selector)}).length, pending: document.querySelectorAll(${JSON.stringify(`[data-testid="${testId}"][data-coordination-id]`)}).length }))()`);
  if (counts?.exact !== 1 || counts.pending !== 1) fail("ui_smoke_coordination_ambiguous");
  return selector;
}

async function press(key, code = key) {
  await command("Input.dispatchKeyEvent", { type: "keyDown", key, code });
  await command("Input.dispatchKeyEvent", { type: "keyUp", key, code });
}

const checks = ["real_tauri_page", `phase_${phase.replaceAll("-", "_")}`];
const actions = [];
let attachmentFixture;
try {
  await command("Runtime.enable");
  if (phase === "settings") {
    await waitVisible('[data-testid="remote-control-settings-page"]');
    await waitVisible('[data-testid="remote-control-role-controlled"]');
    await waitVisible('[data-testid="remote-control-role-controller"]');
    await evaluate(`document.querySelector('[data-testid="remote-control-role-controlled"]').focus()`);
    await press("ArrowRight");
    const selected = await evaluate(`(() => { const e = document.querySelector('[data-testid="remote-control-role-controller"]'); return { selected: e?.getAttribute('aria-selected'), focused: document.activeElement === e }; })()`);
    if (selected?.selected !== "true" || !selected.focused) fail("ui_smoke_tab_keyboard_failed");
    await waitVisible('[data-testid="remote-controller-connect-form"]');
    await evaluate(`document.querySelector('[data-testid="remote-controller-code-input"]').focus()`);
    await command("Input.insertText", { text: "INVALID!" });
    await press("Enter");
    const state = await evaluate(`(() => { const f = document.querySelector('[data-testid="remote-controller-connect-form"]'); const i = document.querySelector('[data-testid="remote-controller-code-input"]'); return { state: f?.getAttribute('data-ui-state'), invalid: i?.getAttribute('aria-invalid'), describedBy: i?.getAttribute('aria-describedby'), focused: document.activeElement === i, live: [...document.querySelectorAll('[aria-live]')].some(e => { const s=getComputedStyle(e); return s.display !== 'none' && s.visibility !== 'hidden'; }) }; })()`);
    if (state?.state !== "format_invalid" || state.invalid !== "true" || !state.describedBy || !state.focused || !state.live) fail("ui_smoke_invalid_input_a11y_failed");
    checks.push("role_tab_keyboard", "invalid_input_focus", "aria_live");
    actions.push("switch_controller_tab", "submit_invalid_pairing_code", "assert_accessible_error");
  } else if (phase === "controlled-panel") {
    await waitVisible('[data-testid="controlled-device-panel"]');
    await waitVisible('[data-testid="controlled-device-refresh"]');
    const before = await evaluate(`document.querySelector('[data-testid="controlled-device-panel"]')?.getAttribute('data-ui-state')`);
    await click('[data-testid="controlled-device-refresh"]');
    await waitFor(async () => (await evaluate(`document.querySelector('[data-testid="controlled-device-panel"]')?.getAttribute('data-ui-state')`)) !== "retiring_generation", "ui_smoke_controlled_refresh_stuck");
    const after = await evaluate(`document.querySelector('[data-testid="controlled-device-panel"]')?.getAttribute('data-ui-state')`);
    if (!before || !after) fail("ui_smoke_controlled_state_missing");
    checks.push("controlled_generation_refresh"); actions.push("refresh_controlled_generation", "assert_controlled_state");
  } else {
    await waitVisible('[data-testid="chat-scroll"]');
    checks.push("unified_chat_view");
    if (phase === "connected-chat") {
      await waitVisible('[data-testid="chat-remote-status"]');
      await waitVisible('[data-testid="chat-composer-input"]');
      await evaluate(`document.querySelector('[data-testid="chat-composer-input"]').focus()`);
      await command("Input.insertText", { text: "ui-smoke" });
      const value = await evaluate(`document.querySelector('[data-testid="chat-composer-input"]')?.value`);
      if (value !== "ui-smoke") fail("ui_smoke_connected_chat_input_failed");
      await evaluate(`(() => { const e=document.querySelector('[data-testid="chat-composer-input"]'); const setter=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set; setter.call(e,''); e.dispatchEvent(new Event('input',{bubbles:true})); })()`);
      actions.push("focus_unified_chat_composer", "type_connected_draft", "assert_connected_draft");
    } else if (phase === "approval-allow" || phase === "approval-deny") {
      await waitVisible('[data-testid="chat-remote-task-actions"]');
      const decision = phase === "approval-allow" ? "allow" : "deny";
      const selector = await exactCoordinatedSelector(`chat-remote-permission-${decision}`);
      await waitVisible(selector);
      const before = await evaluate(`document.querySelectorAll(${JSON.stringify(selector)}).length`);
      await click(selector);
      await waitFor(async () => (await evaluate(`document.querySelectorAll(${JSON.stringify(selector)}).length`)) < before, "ui_smoke_permission_decision_not_applied");
      actions.push(`click_permission_${decision}`, `assert_permission_${decision}_applied`);
    } else if (phase === "attachment") {
      const attachmentSelector = await exactCoordinatedSelector("chat-remote-attachment-input");
      await waitFor(async () => Boolean(await evaluate(`document.querySelector(${JSON.stringify(attachmentSelector)})`)), "ui_smoke_attachment_input_missing");
      attachmentFixture = resolve(`${output}.attachment.txt`);
      await writeFile(attachmentFixture, "pinvou remote-control UI smoke attachment\n", { encoding: "utf8", flag: "wx" });
      await command("DOM.enable");
      const { root } = await command("DOM.getDocument", { depth: -1, pierce: true });
      const { nodeId } = await command("DOM.querySelector", { nodeId: root.nodeId, selector: attachmentSelector });
      if (!nodeId) fail("ui_smoke_attachment_input_missing");
      await command("DOM.setFileInputFiles", { nodeId, files: [attachmentFixture] });
      await waitVisible('[data-testid="chat-remote-attachments"]');
      await waitVisible('[data-testid="chat-composer-send"]');
      await click('[data-testid="chat-composer-send"]');
      await waitFor(async () => !(await evaluate(`Boolean(document.querySelector('[data-testid="chat-remote-attachments"]'))`)), "ui_smoke_attachment_submit_failed");
      actions.push("set_attachment_via_cdp", "assert_attachment_selected", "submit_attachment_via_cdp", "assert_attachment_submitted");
    } else if (phase === "artifact") {
      await waitVisible('[data-testid="chat-remote-artifacts"]');
      await waitVisible('[data-testid="chat-remote-artifact-preview"]');
      await click('[data-testid="chat-remote-artifact-preview"]');
      await waitVisible('[data-testid="chat-remote-artifacts"] [role="dialog"]');
      await click('[data-testid="chat-remote-artifact-preview-close"]');
      await waitFor(async () => !(await evaluate(`Boolean(document.querySelector('[data-testid="chat-remote-artifacts"] [role="dialog"]'))`)), "ui_smoke_artifact_preview_close_failed");
      await waitVisible('[data-testid="chat-remote-artifact-download"]');
      await click('[data-testid="chat-remote-artifact-download"]');
      await waitVisible('[data-testid="chat-remote-action-error"]');
      await waitFor(async () => Boolean(await evaluate(`document.querySelector('[data-testid="chat-remote-artifact-download"]:not(:disabled)')`)), "ui_smoke_artifact_download_stuck");
      await click('[data-testid="chat-remote-artifact-download"]');
      await waitFor(async () => !(await evaluate(`Boolean(document.querySelector('[data-testid="chat-remote-action-error"]')`)), "ui_smoke_artifact_error_recovery_failed");
      actions.push("open_artifact_preview", "assert_artifact_preview", "close_artifact_preview", "download_artifact_expected_failure", "retry_artifact_download", "assert_artifact_recovery_ready");
    }
  }
  if (actions.length < 2) fail("ui_smoke_actions_incomplete");
  const result = { schema: "pinvou-remote-control-ui-smoke", version: 1, phase, result: "pass", checks, actions };
  if (coordinatedPhase) result.coordination_digest = createHash("sha256").update(coordinationId, "utf8").digest("hex");
  if (phase === "attachment") result.attachment_sha256 = createHash("sha256").update(await readFile(attachmentFixture)).digest("hex");
  await writeFile(resolve(output), `${JSON.stringify(result)}\n`, { encoding: "utf8", flag: "wx" });
  process.stdout.write(`${JSON.stringify({ ok: true, phase, checks: checks.length })}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "ui_smoke_failed" })}\n`);
  process.exitCode = 1;
} finally {
  if (attachmentFixture) await unlink(attachmentFixture).catch(() => {});
  socket.close();
}
