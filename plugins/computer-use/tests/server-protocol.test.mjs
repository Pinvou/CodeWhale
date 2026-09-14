// Server protocol tests: real MCP server process over stdio, isolated state.
// The ssh/scp shims stand in for a remote machine, proving the full remote
// agent loop (install -> platform probe -> tool dispatch) without real ssh.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import url from "node:url";

const __dirname = path.dirname(url.fileURLToPath(import.meta.url));
const ROOT = path.resolve(__dirname, "..");

const stateDir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-proto-state-"));
const recDir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-proto-rec-"));
const fakeHome = fs.mkdtempSync(path.join(os.tmpdir(), "cu-proto-home-"));
const binDir = fs.mkdtempSync(path.join(os.tmpdir(), "cu-proto-bin-"));

// Fake remote ssh endpoint: logs every wire payload the server sends, then
// either answers from a canned map (deterministic — no desktop or a11y stack
// needed, so these tests run on headless CI) or falls through to the real
// pushed agent (real behavior on the same host).
fs.writeFileSync(path.join(binDir, "fake-remote.mjs"), `
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";

const [agentPath, b64] = process.argv.slice(2);
let req = {};
try { req = JSON.parse(Buffer.from(b64 ?? "", "base64").toString("utf8")); } catch {}
fs.appendFileSync(path.join(process.env.FAKE_HOME, "cu-wire.log"), JSON.stringify(req) + "\\n");

const CANNED = {
  get_app_state: () => ({
    found: true, name: "Fake App", truncated: false,
    elements: [{ index: 0, path: [0, 1], role: "button", label: "OK", value: "", position: { x: 10, y: 10 }, size: { w: 40, h: 20 }, actions: ["press"] }],
  }),
  screenshot: () => ({
    file: "/remote/shot-1.png", bytes: 2048, scale: 2,
    points: { x: 10, y: 20, w: 1280, h: 800 }, pixels: { w: 2560, h: 1600 },
    capturedAt: "2026-09-14T00:00:00.000Z",
  }),
  // region[0] === 999 is the sentinel for "the agent cropped a stale raster".
  zoom: (r) => ({ file: "/remote/zoom-1.png", bytes: 512, region: r.args?.region ?? null, source: r.args?.region?.[0] === 999 ? "/remote/stale.png" : (r.args?.source ?? null) }),
  left_click: () => ({ clicked: true }),
  left_click_drag: () => ({ dragged: true }),
  set_value: () => ({ set: true }),
};

if (process.env.FAKE_AGENT_CANNED === "1" && CANNED[req.tool]) {
  console.log(JSON.stringify({ ok: true, platform: process.platform, tool: req.tool, data: CANNED[req.tool](req) }));
  process.exit(0);
}
const r = spawnSync(process.execPath, [agentPath, b64], { stdio: "inherit" });
process.exit(r.status ?? 0);
`);
// Fake ssh: rebuild the remote command after user@host, then either run it
// through the fake remote endpoint or emulate the one remote command the
// installer needs (mkdir -p).
fs.writeFileSync(path.join(binDir, "ssh"), `#!/bin/bash
CMD=()
FOUND=0
for a in "$@"; do
  if [ "$FOUND" -eq 1 ]; then CMD+=("$a"); fi
  case "$a" in *@*) [ "$FOUND" -eq 0 ] && FOUND=1 ;; esac
done
SUB="\${CMD[0]}"
if [ "$SUB" = "node" ]; then
  exec node "${binDir}/fake-remote.mjs" "$FAKE_HOME/\${CMD[1]}" "\${CMD[2]}"
fi
if [ "$SUB" = "mkdir" ]; then
  LAST="\${CMD[\${#CMD[@]}-1]}"
  mkdir -p "$FAKE_HOME/$LAST"
  exit 0
fi
exit 0
`);
// Fake scp: copies <src> to <user@host:dest> under FAKE_HOME.
fs.writeFileSync(path.join(binDir, "scp"), `#!/bin/bash
SRC="$(printf '%s\\n' "$@" | tail -n 2 | head -n 1)"
DEST="$(printf '%s\\n' "$@" | tail -n 1)"
DEST="$FAKE_HOME/\${DEST#*:}"
mkdir -p "$(dirname "$DEST")"
cp "$SRC" "$DEST"
`);
fs.chmodSync(path.join(binDir, "ssh"), 0o755);
fs.chmodSync(path.join(binDir, "scp"), 0o755);

let server;
let buf = "";
const pending = new Map();
let nextId = 1;

function rpc(method, params, timeoutMs = 90_000) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => { pending.delete(id); reject(new Error(`timeout: ${method}`)); }, timeoutMs);
    pending.set(id, (msg) => { clearTimeout(t); resolve(msg); });
    server.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  });
}

async function tool(name, args = {}) {
  const res = await rpc("tools/call", { name, arguments: args });
  assert.ok(res.result, `${name}: protocol error ${JSON.stringify(res.error ?? {})}`);
  return JSON.parse(res.result.content[0].text);
}

/** Wire payloads the server sent over the fake ssh transport, by tool. */
function wireCalls(toolName) {
  const log = path.join(fakeHome, "cu-wire.log");
  if (!fs.existsSync(log)) return [];
  return fs.readFileSync(log, "utf8").trim().split("\n").filter(Boolean)
    .map((l) => JSON.parse(l))
    .filter((r) => r.tool === toolName);
}

before(async () => {
  server = spawn("node", [path.join(ROOT, "mcp", "server.mjs")], {
    env: {
      ...process.env,
      PATH: `${binDir}:${process.env.PATH}`,
      FAKE_HOME: fakeHome,
      CODEWHALE_CU_STATE_DIR: stateDir,
      CODEWHALE_CU_RECORDINGS_DIR: recDir,
      FAKE_AGENT_CANNED: "1",
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  server.stderr.on("data", (d) => process.stderr.write(`[server] ${d}`));
  server.stdout.setEncoding("utf8");
  server.stdout.on("data", (d) => {
    buf += d;
    let i;
    while ((i = buf.indexOf("\n")) !== -1) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line) continue;
      try {
        const msg = JSON.parse(line);
        if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
      } catch {}
    }
  });
  const init = await rpc("initialize", { protocolVersion: "2025-06-18" });
  assert.equal(init.result.serverInfo.name, "codewhale-cu");
});

after(() => {
  server?.kill("SIGTERM");
  for (const d of [stateDir, recDir, fakeHome]) { try { fs.rmSync(d, { recursive: true, force: true }); } catch {} }
});

test("tools/list exposes the full frontier surface with valid schemas", async () => {
  const res = await rpc("tools/list", {});
  const tools = res.result.tools;
  assert.ok(tools.length >= 38, `${tools.length} tools`);
  for (const t of tools) {
    assert.ok(t.name && t.description && t.inputSchema, `schema incomplete for ${t.name}`);
  }
  const names = new Set(tools.map((t) => t.name));
  for (const required of ["screenshot", "zoom", "left_click", "double_click", "triple_click", "right_click", "middle_click",
    "mouse_move", "left_click_drag", "left_mouse_down", "left_mouse_up", "scroll", "type", "key", "hold_key",
    "set_value", "select_text", "perform_action", "get_app_state", "list_apps", "list_windows", "list_displays",
    "switch_display", "open_application", "read_clipboard", "write_clipboard", "cursor_position", "wait",
    "recording_start", "recording_stop", "recording_status", "recording_list",
    "computer_list", "computer_switch", "computer_register", "computer_remove", "request_access", "stop_computer_control"]) {
    assert.ok(names.has(required), `missing tool ${required}`);
  }
});

test("computer registry round-trip over the protocol", async () => {
  let r = await tool("computer_list");
  assert.equal(r.ok, true);
  assert.equal(r.active, "local");
  r = await tool("computer_register", { computer: "pad", transport: "hdc" });
  assert.equal(r.registered.platform, "harmonyos");
  r = await tool("computer_switch", { computer: "pad" });
  assert.equal(r.active, "pad");
  r = await tool("computer_remove", { computer: "pad" });
  assert.equal(r.active, "local");
});

test("registering an ssh computer installs the agent and probes the platform", async () => {
  const r = await tool("computer_register", { computer: "box", transport: "ssh", host: "box.test", user: "me" });
  assert.equal(r.ok, true, JSON.stringify(r.error ?? {}));
  assert.equal(r.agentInstall.remotePlatform, process.platform, "platform probed via agent");
  assert.ok(fs.existsSync(path.join(fakeHome, ".codewhale-cu", "agent", "agent.mjs")), "agent pushed");
  assert.ok(fs.existsSync(path.join(fakeHome, ".codewhale-cu", "agent", "src", "backends", "darwin.mjs")), "src tree pushed");
  // dispatch a real tool to the "remote" computer. A headless Linux host
  // (CI) has no window manager tooling, so the remote backend fails closed
  // with its named reason; that error still proves the round trip.
  const apps = await tool("list_apps", { computer: "box" });
  if (apps.ok) {
    assert.equal(apps.computer.id, "box");
    assert.ok(Array.isArray(apps.apps) && apps.apps.length > 0, "apps returned over the wire");
  } else {
    assert.equal(process.platform, "linux", JSON.stringify(apps.error ?? {}));
    assert.equal(apps.error.code, "tool_error");
    assert.match(apps.error.message, /wmctrl|swaymsg|hyprctl/u);
  }
});

test("unknown computer fails closed with a named error", async () => {
  const r = await tool("screenshot", { computer: "ghost" });
  assert.equal(r.ok, false);
  assert.equal(r.error.code, "unknown_computer");
});

test("ssh computers keep element state host-side so element targets resolve", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  // The wire layer resolves element targets against host-side state
  // (prepareWireArgs), so get_app_state over ssh must hand back a usable
  // state_id instead of leaving element actions dead-looping. The fake
  // remote answers with a deterministic tree, so this runs on headless CI.
  const st = await tool("get_app_state", { computer: "box", app_ref: { name: "node" } });
  assert.equal(st.ok, true, JSON.stringify(st.error ?? {}));
  assert.match(String(st.state_id), /^s-\d+$/u, "ssh get_app_state must return a host-side state_id");
  assert.equal(st.computer.id, "box");
  assert.equal(st.elements.length, 1);
  // An element action resolves host-side and dispatches the cached app
  // identity + path over the wire.
  const act = await tool("set_value", { computer: "box", target: { type: "element", state_id: st.state_id, index: 0 }, value: "x" });
  assert.equal(act.ok, true, JSON.stringify(act.error ?? {}));
  const sent = wireCalls("set_value").at(-1);
  assert.deepEqual(sent.args.target.path, [0, 1]);
  assert.deepEqual(sent.args.target.app_ref, { name: "node" });
  assert.equal(sent.args.target.windowIndex, 0);
  // A state observed on "box" must be rejected when aimed at "local".
  const cross = await tool("set_value", { computer: "local", target: { type: "element", state_id: st.state_id, index: 0 }, value: "x" });
  assert.equal(cross.ok, false);
  assert.equal(cross.error.code, "state_wrong_computer");
});

test("ssh zoom rebinds the raster so child pixels aim at the crop region", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  // Without a bound raster the server fails closed with its named reason
  // before going over the wire.
  const early = await tool("zoom", { computer: "box", region: [100, 50, 300, 200] });
  assert.equal(early.ok, false);
  assert.equal(early.error.code, "no_raster");

  const shot = await tool("screenshot", { computer: "box" });
  assert.equal(shot.ok, true, JSON.stringify(shot.error ?? {}));
  assert.match(String(shot.note ?? ""), /scp/u);

  const zoom = await tool("zoom", { computer: "box", region: [100, 50, 300, 200] });
  assert.equal(zoom.ok, true, JSON.stringify(zoom.error ?? {}));
  assert.equal(zoom.file, "/remote/zoom-1.png");
  assert.match(String(zoom.note ?? ""), /scp/u);
  // The host tells the remote which raster "latest" means.
  assert.equal(wireCalls("zoom").at(-1).args.source, "/remote/shot-1.png");

  // Child pixel (60,45) must resolve against the rebound frame: origin
  // {10,20} + region [100,50] at scale 2, then (60,45)/2 -> screen (90,68).
  const click = await tool("left_click", { computer: "box", target: { type: "coordinate", x: 60, y: 45 } });
  assert.equal(click.ok, true, JSON.stringify(click.error ?? {}));
  const clickSent = wireCalls("left_click").at(-1);
  assert.deepEqual({ x: clickSent.args.target.x, y: clickSent.args.target.y }, { x: 90, y: 68 });
});

test("ssh drag endpoints resolve against the rebound child raster", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  // The bound frame is still the previous test's zoom child (origin {60,45}
  // at scale 2), so child pixels (0,0) and (20,10) must cross the wire as
  // screen points — drag endpoints take the same host-side resolution.
  const drag = await tool("left_click_drag", { computer: "box", from_target: { type: "coordinate", x: 0, y: 0 }, to: { type: "coordinate", x: 20, y: 10 } });
  assert.equal(drag.ok, true, JSON.stringify(drag.error ?? {}));
  const sent = wireCalls("left_click_drag").at(-1);
  assert.deepEqual({ x: sent.args.from_target.x, y: sent.args.from_target.y }, { x: 60, y: 45 });
  assert.deepEqual({ x: sent.args.to.x, y: sent.args.to.y }, { x: 70, y: 50 });
});

test("a zoom that cropped a raster other than the bound one says so and keeps the frame", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  const shot = await tool("screenshot", { computer: "box" });
  assert.equal(shot.ok, true, JSON.stringify(shot.error ?? {}));
  // Region [999,...] is the canned-endpoint sentinel for a foreign source.
  const zoom = await tool("zoom", { computer: "box", region: [999, 0, 10, 10] });
  assert.equal(zoom.ok, true, JSON.stringify(zoom.error ?? {}));
  assert.match(String(zoom.note ?? ""), /other than the bound/u);
  // The binding must be untouched: child pixel (10,10) still resolves
  // against the previous raster (origin {10,20}, scale 2) -> screen (15,25).
  const click = await tool("left_click", { computer: "box", target: { type: "coordinate", x: 10, y: 10 } });
  assert.equal(click.ok, true, JSON.stringify(click.error ?? {}));
  const sent = wireCalls("left_click").at(-1);
  assert.deepEqual({ x: sent.args.target.x, y: sent.args.target.y }, { x: 15, y: 25 });
});

test("computer_remove evicts the removed computer's remembered element states", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  const st = await tool("get_app_state", { computer: "box", app_ref: { name: "node" } });
  assert.equal(st.ok, true, JSON.stringify(st.error ?? {}));
  assert.equal((await tool("computer_remove", { computer: "box" })).ok, true);
  // Re-registering the same id must not resurrect the old observation,
  // even though the state's computerId still matches the id.
  assert.equal((await tool("computer_register", { computer: "box", transport: "ssh", host: "box.test", user: "me" })).ok, true);
  const stale = await tool("set_value", { computer: "box", target: { type: "element", state_id: st.state_id, index: 0 }, value: "x" });
  assert.equal(stale.ok, false);
  assert.equal(stale.error.code, "unknown_state");
  // A fresh observation on the new registration works and gets a new id.
  const fresh = await tool("get_app_state", { computer: "box", app_ref: { name: "node" } });
  assert.equal(fresh.ok, true, JSON.stringify(fresh.error ?? {}));
  assert.notEqual(fresh.state_id, st.state_id);
});

test("computer_remove alone evicts remembered element states, before any re-register", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  const st = await tool("get_app_state", { computer: "box", app_ref: { name: "node" } });
  assert.equal(st.ok, true, JSON.stringify(st.error ?? {}));
  assert.equal((await tool("computer_remove", { computer: "box" })).ok, true);
  // Probe through a different computer id: if remove had kept the state, it
  // would resolve far enough to fail with state_wrong_computer — eviction
  // surfaces earlier as unknown_state. (Re-registering "box" would evict too,
  // so this is the only vantage that distinguishes the two paths.)
  assert.equal((await tool("computer_register", { computer: "box2", transport: "ssh", host: "box2.test", user: "me" })).ok, true);
  const probe = await tool("set_value", { computer: "box2", target: { type: "element", state_id: st.state_id, index: 0 }, value: "x" });
  assert.equal(probe.ok, false);
  assert.equal(probe.error.code, "unknown_state");
  // Leave the registry as later tests expect it: box registered, box2 gone.
  assert.equal((await tool("computer_remove", { computer: "box2" })).ok, true);
  assert.equal((await tool("computer_register", { computer: "box", transport: "ssh", host: "box.test", user: "me" })).ok, true);
});

test("ssh recording fails closed with the ssh reason instead of stranding the model", { skip: process.platform === "win32" && "needs a registered ssh computer, which the shim cannot provide on windows" }, async () => {
  const start = await tool("recording_start", { computer: "box" });
  assert.equal(start.ok, false);
  assert.equal(start.error.code, "persistent_session_required");

  const stop = await tool("recording_stop", { computer: "box", id: "nope" });
  assert.equal(stop.ok, false);
  assert.equal(stop.error.code, "persistent_session_required");

  const status = await tool("recording_status", { computer: "box", id: "nope" });
  assert.equal(status.ok, false);
  assert.equal(status.error.code, "persistent_session_required");
});

test("ssh press-and-hold fails closed so a press cannot outlive its release", { skip: process.platform === "win32" && "needs a registered ssh computer, which the shim cannot provide on windows" }, async () => {
  const down = await tool("left_mouse_down", { computer: "box", target: { type: "coordinate", x: 1, y: 1 } });
  assert.equal(down.ok, false);
  assert.equal(down.error.code, "persistent_session_required");
});

test("re-registering an id in place forgets the old host's runtime state", { skip: process.platform === "win32" && "ssh shim tests need a POSIX ssh shim (see the registering-ssh test)" }, async () => {
  const st = await tool("get_app_state", { computer: "box", app_ref: { name: "node" } });
  assert.equal(st.ok, true, JSON.stringify(st.error ?? {}));
  // Update the registration in place: same id, different host.
  assert.equal((await tool("computer_register", { computer: "box", transport: "ssh", host: "other.test", user: "me" })).ok, true);
  // The observation from box.test must not dispatch onto other.test.
  const stale = await tool("set_value", { computer: "box", target: { type: "element", state_id: st.state_id, index: 0 }, value: "x" });
  assert.equal(stale.ok, false);
  assert.equal(stale.error.code, "unknown_state");
  // The raster binding from the old host is gone too: zoom fails closed
  // instead of cropping the old host's file.
  const zoom = await tool("zoom", { computer: "box", region: [0, 0, 10, 10] });
  assert.equal(zoom.ok, false);
  assert.equal(zoom.error.code, "no_raster");
});

test("kill switch refuses mutating tools but keeps read-only probes", async () => {
  let r = await tool("stop_computer_control", { reason: "protocol-test" });
  assert.equal(r.stopped, true);
  r = await tool("screenshot");
  assert.equal(r.error.code, "control_stopped");
  r = await tool("computer_list");
  assert.equal(r.ok, true);
});
