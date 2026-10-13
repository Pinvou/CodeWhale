//! Plugin acceptance at both compatibility and v0.9.1 bundle boundaries.
//!
//! Exercises the plugin surface from the binary level:
//! - The distributed binary still loads after the plugin module migration
//! - A sealed real PTY exercises plugin.toml review/trust/enable/revoke,
//!   reviewed Skill dispatch, and hermetic reviewed stdio MCP execution

use std::path::PathBuf;
use std::process::Command;

#[cfg(all(unix, feature = "long-running-tests"))]
use super::qa_harness;

#[cfg(all(unix, feature = "long-running-tests"))]
use qa_harness::harness::{Harness, make_sealed_workspace};
#[cfg(all(unix, feature = "long-running-tests"))]
use qa_harness::keys;

// ---------------------------------------------------------------------------
// Binary smoke test
// ---------------------------------------------------------------------------

/// Prove the binary still loads after the plugin module extraction.
#[tokio::test(flavor = "current_thread")]
async fn plugin_module_does_not_break_binary_load() {
    let output = Command::new(codewhale_tui_binary())
        .arg("--version")
        .output()
        .expect("codewhale-tui --version should start");

    assert!(
        output.status.success(),
        "codewhale-tui --version failed\nstderr:\n{}",
        String::from_utf8_lossy(&output.stderr)
    );

    let version = String::from_utf8_lossy(&output.stdout);
    assert!(
        version.contains("codewhale"),
        "version output should mention codewhale, got: {version}"
    );
}

// ---------------------------------------------------------------------------
// Real plugin.toml binary/TUI acceptance
// ---------------------------------------------------------------------------

#[cfg(all(unix, feature = "long-running-tests"))]
const BINARY_ACCEPTANCE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(20);

#[cfg(all(unix, feature = "long-running-tests"))]
fn write_reviewed_bundle_fixture(workspace: &std::path::Path) -> PathBuf {
    use std::os::unix::fs::PermissionsExt as _;

    let bundle = workspace.join(".codewhale/plugins/demo");
    std::fs::create_dir_all(bundle.join("skills/review")).expect("plugin fixture directories");
    std::fs::write(
        bundle.join("plugin.toml"),
        r#"schema_version = 1
[plugin]
name = "demo"
version = "1.0.0"
description = "Hermetic binary acceptance fixture"

[skills]
path = "skills"

[mcp_servers.local]
command = "./server.py"
connect_timeout = 5
execute_timeout = 30
read_timeout = 30

[mcp_servers.local.env]
ACCEPTANCE_LOG = "${PLUGIN_ACCEPTANCE_LOG}"
"#,
    )
    .expect("plugin manifest");
    std::fs::write(
        bundle.join("skills/review/SKILL.md"),
        "---\nname: review\ndescription: reviewed binary acceptance Skill\n---\n\nUse the reviewed fixture.\n",
    )
    .expect("plugin Skill");
    let server = bundle.join("server.py");
    std::fs::write(
        &server,
        r#"#!/usr/bin/env python3
import json
import os
import signal
import sys
import time

log_path = os.environ["ACCEPTANCE_LOG"]

def record(event):
    with open(log_path, "a", encoding="utf-8") as handle:
        handle.write(event + "\n")
        handle.flush()

def stop(signum, _frame):
    record("signal:" + str(signum))
    raise SystemExit(0)

signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
record("started")
record("api-key-present:" + str("DEEPSEEK_API_KEY" in os.environ).lower())

for raw in sys.stdin:
    message = json.loads(raw)
    method = message.get("method")
    request_id = message.get("id")
    if method == "initialize":
        result = {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "plugin-acceptance", "version": "1.0.0"},
        }
    elif method == "tools/list":
        record("tools:list")
        result = {"tools": [{
            "name": "echo",
            "description": "Hermetic plugin echo",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "text": {"type": "string"},
                    "hang": {"type": "boolean"},
                },
            },
        }]}
    elif method == "tools/call":
        args = message.get("params", {}).get("arguments", {})
        if args.get("hang"):
            record("call:hang")
            while True:
                time.sleep(0.05)
        record("call:echo")
        result = {"content": [{
            "type": "text",
            "text": "plugin-echo:" + str(args.get("text", "")),
        }]}
    else:
        if request_id is None:
            continue
        result = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": request_id, "result": result}) + "\n")
    sys.stdout.flush()
"#,
    )
    .expect("stdio MCP fixture");
    std::fs::set_permissions(&server, std::fs::Permissions::from_mode(0o755))
        .expect("executable MCP fixture");
    bundle
}

#[cfg(unix)]
fn sse_line(value: serde_json::Value) -> String {
    format!(
        "data: {}\n\n",
        serde_json::to_string(&value).expect("SSE JSON")
    )
}

#[cfg(unix)]
fn text_sse(text: &str) -> String {
    [
        sse_line(serde_json::json!({
            "id": "chatcmpl-plugin-acceptance",
            "object": "chat.completion.chunk",
            "model": "deepseek-v4-pro",
            "choices": [{"index": 0, "delta": {"content": text}, "finish_reason": null}]
        })),
        sse_line(serde_json::json!({
            "id": "chatcmpl-plugin-acceptance",
            "object": "chat.completion.chunk",
            "model": "deepseek-v4-pro",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12}
        })),
        "data: [DONE]\n\n".to_string(),
    ]
    .join("")
}

#[cfg(unix)]
fn tool_call_sse(hang: bool) -> String {
    let call_id = if hang {
        "call_plugin_hang"
    } else {
        "call_plugin_echo"
    };
    let arguments = serde_json::to_string(&serde_json::json!({
        "text": "acceptance",
        "hang": hang,
    }))
    .expect("tool args");
    [
        sse_line(serde_json::json!({
            "id": "chatcmpl-plugin-tool",
            "object": "chat.completion.chunk",
            "model": "deepseek-v4-pro",
            "choices": [{
                "index": 0,
                "delta": {"tool_calls": [{
                    "index": 0,
                    "id": call_id,
                    "type": "function",
                    "function": {
                        "name": "mcp_plugin-4-demo-local_echo",
                        "arguments": arguments
                    }
                }]},
                "finish_reason": null
            }]
        })),
        sse_line(serde_json::json!({
            "id": "chatcmpl-plugin-tool",
            "object": "chat.completion.chunk",
            "model": "deepseek-v4-pro",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 6, "total_tokens": 16}
        })),
        "data: [DONE]\n\n".to_string(),
    ]
    .join("")
}

/// Read exactly `Content-Length` bytes. `read_to_string` waits for EOF, so an
/// HTTP/1.1 keep-alive client (reqwest on macOS CI) never finishes the body
/// and this single-threaded fixture never answers.
#[cfg(unix)]
fn read_limited_http_body(
    reader: &mut (impl std::io::Read + ?Sized),
    content_length: Option<usize>,
) -> String {
    const MAX: usize = 64 * 1024;
    let limit = content_length.unwrap_or(0).min(MAX);
    if limit == 0 {
        return String::new();
    }
    let mut buf = vec![0u8; limit];
    let mut filled = 0usize;
    while filled < buf.len() {
        match reader.read(&mut buf[filled..]) {
            Ok(0) => break,
            Ok(n) => filled += n,
            Err(err) if err.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => break,
        }
    }
    String::from_utf8_lossy(&buf[..filled]).into_owned()
}

#[cfg(unix)]
fn spawn_hermetic_model_server() -> (
    String,
    std::sync::mpsc::Sender<()>,
    std::thread::JoinHandle<()>,
) {
    use tiny_http::{Header, Method, Response, Server};

    let server = Server::http("127.0.0.1:0").expect("loopback model server");
    let base_url = format!(
        "http://{}/v1",
        server.server_addr().to_ip().expect("loopback address")
    );
    let (shutdown_tx, shutdown_rx) = std::sync::mpsc::channel();
    let handle = std::thread::spawn(move || {
        loop {
            let request = match server.recv_timeout(std::time::Duration::from_millis(100)) {
                Ok(Some(request)) => request,
                Ok(None) => {
                    if shutdown_rx.try_recv().is_ok() {
                        break;
                    }
                    continue;
                }
                Err(_) => break,
            };
            let mut request = request;
            let url = request.url().to_string();
            if request.method() == &Method::Get && url.ends_with("/models") {
                let response = Response::from_string(
                    r#"{"object":"list","data":[{"id":"deepseek-v4-pro","object":"model"}]}"#,
                )
                .with_header(
                    Header::from_bytes("content-type", "application/json").expect("JSON header"),
                )
                .with_header(Header::from_bytes("connection", "close").expect("close header"));
                let _ = request.respond(response);
                continue;
            }
            let content_length = request.body_length();
            let body = read_limited_http_body(request.as_reader(), content_length);
            let current_user = serde_json::from_str::<serde_json::Value>(&body)
                .ok()
                .and_then(|request| request.get("messages")?.as_array().cloned())
                .and_then(|messages| {
                    messages.into_iter().rev().find_map(|message| {
                        (message.get("role")?.as_str()? == "user")
                            .then(|| message.get("content")?.as_str().map(str::to_owned))?
                    })
                })
                .unwrap_or_default();
            let stream = if current_user.contains("hang plugin call") {
                tool_call_sse(true)
            } else if body.contains("plugin-echo:acceptance") {
                text_sse("binary plugin call complete")
            } else if body.contains("call plugin echo") {
                tool_call_sse(false)
            } else {
                text_sse("binary fixture acknowledged")
            };
            let response = Response::from_string(stream)
                .with_header(
                    Header::from_bytes("content-type", "text/event-stream").expect("SSE header"),
                )
                .with_header(Header::from_bytes("connection", "close").expect("close header"));
            let _ = request.respond(response);
        }
    });
    (base_url, shutdown_tx, handle)
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn submit_tui_command(tui: &mut Harness, text: &str) {
    tui.send(keys::key::text(text)).expect("type TUI command");
    if tui
        .wait_for_text(text, std::time::Duration::from_secs(3))
        .is_err()
    {
        panic!(
            "typed command not visible: {text:?}\n{}",
            short_diagnostics(tui, None)
        );
    }
    std::thread::sleep(std::time::Duration::from_millis(180));
    tui.pump();
    tui.send(keys::key::enter()).expect("submit TUI command");
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn visible_review_confirmation(tui: &mut Harness) -> Option<String> {
    tui.pump();
    review_confirmation_in_text(&tui.frame().text())
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn review_confirmation_in_text(text: &str) -> Option<String> {
    // The confirmation is `/plugin trust demo <64-hex>.<64-hex>` (129 chars).
    // Transcript cards wrap well before that, so a single rendered line no
    // longer holds the token. Join trimmed lines and recover the two digests.
    // The transcript may also retain an earlier partial command, so scan every
    // marker instead of rejecting after the first malformed candidate.
    let joined: String = text.lines().map(str::trim).collect();
    let marker = "/plugin trust demo ";
    joined.match_indices(marker).find_map(|(start, _)| {
        let token: String = joined[start + marker.len()..]
            .chars()
            .take_while(|ch| ch.is_ascii_hexdigit() || *ch == '.')
            .collect();
        let (content, capability) = token.split_once('.')?;
        (content.len() == 64
            && capability.len() == 64
            && content.chars().all(|ch| ch.is_ascii_hexdigit())
            && capability.chars().all(|ch| ch.is_ascii_hexdigit()))
        .then(|| format!("{marker}{content}.{capability}"))
    })
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn sanitize_diag_line(line: &str) -> String {
    line.chars()
        .map(|ch| {
            if ch.is_control() && ch != '\t' {
                ' '
            } else {
                ch
            }
        })
        .take(120)
        .collect()
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn short_diagnostics(tui: &mut Harness, log_path: Option<&std::path::Path>) -> String {
    tui.pump();
    let mut out = String::new();
    if let Some(pid) = tui.pid() {
        let alive = std::process::Command::new("kill")
            .args(["-0", &pid.to_string()])
            .status()
            .is_ok_and(|status| status.success());
        out.push_str(&format!("tui pid={pid} alive={alive}\n"));
    } else {
        out.push_str("tui pid=none\n");
    }
    if let Some(path) = log_path {
        match std::fs::read_to_string(path) {
            Ok(log) => out.push_str(&format!("mcp log ({} bytes):\n{log}\n", log.len())),
            Err(err) => out.push_str(&format!("mcp log unreadable: {err}\n")),
        }
    }
    let lines: Vec<String> = tui.frame().text().lines().map(str::to_owned).collect();
    out.push_str("visible head:\n");
    for (index, line) in lines.iter().take(12).enumerate() {
        out.push_str(&format!("{index:>3} | {}\n", sanitize_diag_line(line)));
    }
    if lines.len() > 12 {
        out.push_str("visible tail:\n");
        let start = lines.len().saturating_sub(12);
        for (index, line) in lines.iter().skip(start).enumerate() {
            out.push_str(&format!(
                "{:>3} | {}\n",
                start + index,
                sanitize_diag_line(line)
            ));
        }
    }
    out
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn expect_visible(tui: &mut Harness, needle: &str, label: &str) {
    if tui
        .wait_for_text(needle, BINARY_ACCEPTANCE_TIMEOUT)
        .is_err()
    {
        panic!(
            "{label}: {needle:?} not visible within {:?}\n{}",
            qa_harness::harness::ci_scaled(BINARY_ACCEPTANCE_TIMEOUT),
            short_diagnostics(tui, None)
        );
    }
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn wait_for_composer_ready(tui: &mut Harness) {
    if tui
        .wait_for(
            |frame| {
                let (row, _) = frame.cursor();
                // The composer's cursor row sits above its bottom rule, the
                // posture row, and the info line — four rows from the end.
                frame.any_visible_text() && row >= frame.rows().saturating_sub(4)
            },
            BINARY_ACCEPTANCE_TIMEOUT,
        )
        .is_err()
    {
        panic!(
            "TUI did not paint a focused composer within {:?}\n{}",
            qa_harness::harness::ci_scaled(BINARY_ACCEPTANCE_TIMEOUT),
            short_diagnostics(tui, None)
        );
    }
}

/// This acceptance starts as a fresh interactive launch. Select the real
/// Startup "New session" action before testing commands that belong to a
/// live conversation; a focused pre-session composer is not itself a session.
#[cfg(all(unix, feature = "long-running-tests"))]
fn begin_new_session_from_startup(tui: &mut Harness) {
    expect_visible(tui, "New session", "show the launch card");
    // Typing goes straight to the composer; Enter sends the first message
    // and the session begins (the card dissolved on the first keystroke).
    tui.send("start the session")
        .expect("type the first prompt");
    tui.send(keys::key::enter()).expect("send the first prompt");
    if tui
        .wait_for(
            |frame| !frame.text().contains('\u{2442}'),
            BINARY_ACCEPTANCE_TIMEOUT,
        )
        .is_err()
    {
        panic!(
            "the first prompt did not enter the live shell within {:?}\n{}",
            qa_harness::harness::ci_scaled(BINARY_ACCEPTANCE_TIMEOUT),
            short_diagnostics(tui, None)
        );
    }
}

#[cfg(all(unix, feature = "long-running-tests"))]
fn wait_for_log(tui: &mut Harness, path: &std::path::Path, needle: &str) {
    let budget = qa_harness::harness::ci_scaled(BINARY_ACCEPTANCE_TIMEOUT);
    let deadline = std::time::Instant::now() + budget;
    loop {
        tui.pump();
        if std::fs::read_to_string(path).is_ok_and(|body| body.contains(needle)) {
            return;
        }
        if std::time::Instant::now() >= deadline {
            panic!(
                "plugin MCP log did not contain {needle:?} within {budget:?}\n{}",
                short_diagnostics(tui, Some(path))
            );
        }
        std::thread::sleep(std::time::Duration::from_millis(40));
    }
}

/// A focused composer and a streamed answer can both appear before the turn
/// settles. Use the runtime's terminal receipt, not either paint, to admit the
/// next prompt; otherwise this fixture exercises the busy-turn queue by accident.
#[cfg(all(unix, feature = "long-running-tests"))]
fn wait_for_turn_receipt(tui: &mut Harness, outbox: &std::path::Path, count: usize, kind: &str) {
    let receipt = || {
        std::fs::read_to_string(outbox)
            .unwrap_or_default()
            .lines()
            .filter_map(|line| serde_json::from_str::<serde_json::Value>(line).ok())
            .filter(|event| event["event"] == "turn_end")
            .collect::<Vec<_>>()
    };
    if tui
        .wait_for(|_| receipt().len() >= count, BINARY_ACCEPTANCE_TIMEOUT)
        .is_err()
    {
        panic!(
            "terminal turn receipt {count} not observed within {:?}\n{}",
            qa_harness::harness::ci_scaled(BINARY_ACCEPTANCE_TIMEOUT),
            short_diagnostics(tui, Some(outbox)),
        );
    }
    let events = receipt();
    assert_eq!(
        events.len(),
        count,
        "one terminal receipt per submitted turn"
    );
    assert_eq!(events[count - 1]["kind"], kind, "terminal turn outcome");
}

/// Exercise the distributed binary through a real PTY and a sealed home. The
/// only socket is a test-owned loopback model endpoint; plugin execution is
/// stdio-only and receives no real credentials or ambient secret environment.
#[cfg(all(unix, feature = "long-running-tests"))]
#[tokio::test(flavor = "current_thread")]
async fn plugin_toml_binary_lifecycle_skill_and_stdio_mcp_acceptance() {
    static ACCEPTANCE_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    let _serial = ACCEPTANCE_LOCK
        .lock()
        .unwrap_or_else(|lock| lock.into_inner());
    let workspace = make_sealed_workspace().expect("sealed workspace");
    let bundle = write_reviewed_bundle_fixture(workspace.workspace());
    let mcp_log = workspace.home().join(".codewhale/plugin-acceptance.log");
    let outbox = workspace.home().join(".codewhale/lifecycle-outbox.jsonl");
    let config_path = workspace.home().join(".codewhale/config.toml");
    let mut config = std::fs::read_to_string(&config_path).expect("sealed config");
    config.push_str(&format!(
        "\n[lifecycle_outbox]\npath = {}\n",
        serde_json::json!(outbox.to_string_lossy()),
    ));
    std::fs::write(config_path, config).expect("configure sealed lifecycle receipt");
    let (base_url, shutdown_tx, model_thread) = spawn_hermetic_model_server();
    let mut tui = Harness::builder(Harness::cargo_bin("codewhale-tui"))
        .cwd(workspace.workspace())
        .clear_env()
        .seal_home(workspace.home())
        .env("DEEPSEEK_API_KEY", "sealed-plugin-acceptance-key")
        .env("DEEPSEEK_BASE_URL", &base_url)
        .env("DEEPSEEK_MODEL", "deepseek-v4-pro")
        .env("PLUGIN_ACCEPTANCE_LOG", mcp_log.to_string_lossy())
        .env("CODEWHALE_DISABLE_MODELS_DEV_FETCH", "1")
        .env("NO_ANIMATIONS", "1")
        .env("RUST_LOG", "warn")
        .args([
            "--workspace",
            workspace.workspace().to_str().expect("workspace UTF-8"),
            "--no-project-config",
            "--skip-onboarding",
            "--fresh",
        ])
        .size(52, 200)
        .spawn()
        .expect("start distributed TUI binary");

    // Readiness is a painted, focused composer—not localized placeholder copy.
    // The binary begins at Tideline Startup, so choose its real New Session
    // action before exercising the existing-session plugin contract.
    begin_new_session_from_startup(&mut tui);
    wait_for_turn_receipt(&mut tui, &outbox, 1, "turn.completed");
    wait_for_composer_ready(&mut tui);
    submit_tui_command(&mut tui, "/plugin show demo");
    expect_visible(
        &mut tui,
        "Qualified skills: [demo:review]",
        "show reviewed Skill inventory",
    );
    assert!(
        !workspace
            .home()
            .join(".codewhale/plugins/state.json")
            .exists(),
        "show must remain read-only"
    );

    submit_tui_command(&mut tui, "/plugin trust demo");
    if tui
        .wait_for(
            |frame| review_confirmation_in_text(&frame.text()).is_some(),
            BINARY_ACCEPTANCE_TIMEOUT,
        )
        .is_err()
    {
        panic!(
            "review confirmation not visible within {:?}\n{}",
            qa_harness::harness::ci_scaled(BINARY_ACCEPTANCE_TIMEOUT),
            short_diagnostics(&mut tui, None)
        );
    }
    let confirmation = visible_review_confirmation(&mut tui).unwrap_or_else(|| {
        panic!(
            "review confirmation not visible\n{}",
            short_diagnostics(&mut tui, None)
        )
    });
    submit_tui_command(&mut tui, &confirmation);
    expect_visible(&mut tui, "Plugin bundle 'demo': trusted.", "trust receipt");

    submit_tui_command(&mut tui, "/plugin enable demo");
    expect_visible(&mut tui, "Plugin bundle 'demo': enabled.", "bundle enabled");
    submit_tui_command(&mut tui, "$demo:review");
    expect_visible(
        &mut tui,
        "Activated skill: demo:review",
        "reviewed Skill dispatch",
    );

    submit_tui_command(&mut tui, "call plugin echo");
    expect_visible(&mut tui, "Do you want to proceed?", "MCP approval prompt");
    tui.send(keys::key::ch('2'))
        .expect("approve this reviewed MCP kind for the sealed session");
    wait_for_log(&mut tui, &mcp_log, "started");
    wait_for_log(&mut tui, &mcp_log, "api-key-present:false");
    wait_for_log(&mut tui, &mcp_log, "tools:list");
    wait_for_log(&mut tui, &mcp_log, "call:echo");
    expect_visible(
        &mut tui,
        "binary plugin call complete",
        "plugin tool result returned to model",
    );
    wait_for_turn_receipt(&mut tui, &outbox, 2, "turn.completed");

    submit_tui_command(&mut tui, "hang plugin call");
    wait_for_log(&mut tui, &mcp_log, "call:hang");
    tui.send([0x03]).expect("interrupt hanging plugin turn");
    wait_for_turn_receipt(&mut tui, &outbox, 3, "turn.interrupted");
    tui.send([0x15])
        .expect("clear the interrupted prompt restored into the composer");
    submit_tui_command(&mut tui, "/plugin revoke demo");
    expect_visible(
        &mut tui,
        "Plugin bundle 'demo': trust-revoked.",
        "bundle trust revoked",
    );
    wait_for_log(&mut tui, &mcp_log, "signal:");

    let state = std::fs::read_to_string(workspace.home().join(".codewhale/plugins/state.json"))
        .expect("durable plugin state");
    assert!(state.contains("\"enabled\": true"));
    assert!(state.contains("\"trust\": null"));
    assert!(bundle.join("server.py").exists(), "source bundle preserved");

    submit_tui_command(&mut tui, "/exit");
    assert_eq!(
        tui.wait_for_exit(BINARY_ACCEPTANCE_TIMEOUT),
        Some(0),
        "the TUI must exit gracefully after revoking the plugin",
    );
    let receipts = std::fs::read_to_string(&outbox).expect("outbox after process exit");
    let final_event: serde_json::Value =
        serde_json::from_str(receipts.lines().last().expect("final receipt"))
            .expect("complete final JSONL event");
    assert_eq!(final_event["event"], "session_end");
    assert_eq!(final_event["kind"], "session.ended");

    let _ = tui.shutdown();
    let _ = shutdown_tx.send(());
    let _ = model_thread.join();
}

#[cfg(all(unix, feature = "long-running-tests"))]
#[test]
fn review_confirmation_skips_partial_candidates_and_survives_transcript_wrap() {
    let content = "a".repeat(64);
    let capability = "b".repeat(64);
    let wrapped = format!(
        "/plugin trust demo partial\n  /plugin trust demo {head}\n  {mid}\n  {tail}\n",
        head = &format!("{content}.{capability}")[..40],
        mid = &format!("{content}.{capability}")[40..90],
        tail = &format!("{content}.{capability}")[90..],
    );
    assert_eq!(
        review_confirmation_in_text(&wrapped),
        Some(format!("/plugin trust demo {content}.{capability}"))
    );
}

#[cfg(unix)]
#[test]
fn limited_http_body_stops_at_content_length() {
    let mut cursor = std::io::Cursor::new(b"{\"ok\":true}trailing-keep-alive");
    let body = read_limited_http_body(&mut cursor, Some(11));
    assert_eq!(body, "{\"ok\":true}");
}

#[cfg(unix)]
#[test]
fn hermetic_model_server_answers_http11_keepalive_post() {
    use std::io::{Read, Write};
    use std::net::TcpStream;
    use std::time::{Duration, Instant};

    let (base_url, shutdown_tx, handle) = spawn_hermetic_model_server();
    let host = base_url
        .strip_prefix("http://")
        .and_then(|rest| rest.strip_suffix("/v1"))
        .expect("loopback /v1 URL");
    let body = r#"{"messages":[{"role":"user","content":"call plugin echo"}]}"#;
    let request = format!(
        "POST /v1/chat/completions HTTP/1.1\r\n\
         Host: {host}\r\n\
         Content-Type: application/json\r\n\
         Content-Length: {}\r\n\
         Connection: keep-alive\r\n\
         \r\n\
         {body}",
        body.len()
    );

    let started = Instant::now();
    let mut stream = TcpStream::connect(host).expect("connect fixture");
    stream
        .set_read_timeout(Some(Duration::from_secs(2)))
        .expect("read timeout");
    stream
        .set_write_timeout(Some(Duration::from_secs(2)))
        .expect("write timeout");
    stream.write_all(request.as_bytes()).expect("write request");
    stream.flush().expect("flush request");

    let mut response = Vec::new();
    let mut buf = [0u8; 4096];
    loop {
        match stream.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                response.extend_from_slice(&buf[..n]);
                let text = String::from_utf8_lossy(&response);
                if text.contains("tool_calls") || text.contains("mcp_plugin") {
                    break;
                }
            }
            Err(err)
                if err.kind() == std::io::ErrorKind::WouldBlock
                    || err.kind() == std::io::ErrorKind::TimedOut =>
            {
                panic!(
                    "keepalive POST hung after {:?}; fixture must read Content-Length, not EOF\n{}",
                    started.elapsed(),
                    String::from_utf8_lossy(&response)
                );
            }
            Err(err) => panic!("read fixture: {err}"),
        }
    }
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "keepalive POST took {:?}",
        started.elapsed()
    );
    let text = String::from_utf8_lossy(&response);
    assert!(
        text.contains("tool_calls") || text.contains("mcp_plugin"),
        "unexpected fixture response: {text}"
    );
    let _ = shutdown_tx.send(());
    let _ = handle.join();
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn codewhale_tui_binary() -> PathBuf {
    if let Some(path) = option_env!("CARGO_BIN_EXE_codewhale-tui") {
        return PathBuf::from(path);
    }
    if let Ok(path) = std::env::var("CARGO_BIN_EXE_codewhale-tui") {
        return PathBuf::from(path);
    }

    let mut path = std::env::current_exe().expect("current test executable path");
    path.pop();
    if path.ends_with("deps") {
        path.pop();
    }
    path.push(format!("codewhale-tui{}", std::env::consts::EXE_SUFFIX));
    path
}
