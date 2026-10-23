// Synthetic faults and a separate answer key. No user data or external services.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer, request } from "node:http";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

export const taxonomy = {
  auth_key_revoked: "The provider rejected a revoked API key (not an exhausted quota).",
  rate_limit: "The provider throttled request frequency; retry after the indicated interval.",
  quota_exhausted: "The account has insufficient credit/quota; waiting alone will not fix it.",
  context_overflow: "The request exceeds the model context limit.",
  provider_unavailable: "The provider upstream is temporarily unavailable.",
  stream_incomplete:
    "The response ended before the announced stream was complete; the failing layer is not established.",
  proxy_reset: "The proxy reset the upstream connection while forwarding the response.",
  response_timeout: "The client deadline expired waiting for response headers.",
  tool_process_failure: "The renderer tool process exited with a nonzero exit code.",
  file_access_denied: "A file write was rejected by access permissions.",
  file_missing: "A required configuration file does not exist.",
  remote_receipt_loss:
    "The controlled task succeeded, but delivery of its terminal result to the controller failed.",
  permission_configuration_missing:
    "Remote execution was rejected because its task permission mode was not configured.",
  unknown:
    "Evidence does not establish a specific root cause. State the observable failure and a useful next check.",
};
const fingerprint = (text) => createHash("sha256").update(text).digest("hex");

async function observeHttpFaults() {
  const responses = {
    auth: [401, { error: { code: "key_revoked", message: "The supplied key has been revoked" } }],
    rate: [429, { error: { code: "rate_limit_exceeded", retry_after_ms: 15000 } }],
    quota: [429, { error: { code: "insufficient_quota", billing_enabled: false } }],
    context: [400, { error: { code: "context_length_exceeded", limit: 8192, input_tokens: 9050 } }],
    unavailable: [503, { error: { code: "upstream_unavailable" } }],
  };
  const server = createServer((req, res) => {
    const key = req.url.slice(1);
    if (key === "timeout") return; // The client destroys this socket at its real deadline.
    if (key === "reset") {
      req.socket.destroy();
      return;
    }
    if (key === "truncated") {
      res.writeHead(200, { "content-type": "text/event-stream", "content-length": "2000" });
      res.flushHeaders();
      res.write("data: part1\n\ndata: part2\n\n");
      setTimeout(() => res.destroy(), 20);
      return;
    }
    const [status, body] = responses[key];
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = server.address().port;
  async function call(route, timeout = 2000) {
    return new Promise((resolve, reject) => {
      const req = request({ host: "127.0.0.1", port, path: `/${route}` }, (res) => {
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () =>
          resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString() }),
        );
        res.on("aborted", () =>
          resolve({
            status: res.statusCode,
            error: "stream_incomplete",
            received_frames: 2,
            expected_frames: 5,
          }),
        );
        res.on("error", (error) => {
          if (error.code !== "ECONNRESET") reject(error);
        });
      });
      req.setTimeout(timeout, () =>
        req.destroy(Object.assign(new Error("deadline_exceeded"), { code: "DEADLINE_EXCEEDED" })),
      );
      req.on("error", (error) => resolve({ error: error.code }));
      req.end();
    });
  }
  try {
    const observed = {};
    for (const route of Object.keys(responses)) observed[route] = await call(route);
    observed.truncated = await call("truncated");
    observed.reset = await call("reset");
    observed.timeout = await call("timeout", 25);
    assert.equal(observed.auth.status, 401);
    assert.equal(observed.rate.status, 429);
    assert.equal(observed.quota.status, 429);
    assert.equal(observed.truncated.error, "stream_incomplete");
    assert.equal(observed.reset.error, "ECONNRESET");
    assert.equal(observed.timeout.error, "DEADLINE_EXCEEDED");
    return observed;
  } finally {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
}

export async function makeCases(missingPath) {
  const observed = await observeHttpFaults();
  let exitCode;
  try {
    execFileSync(process.execPath, ["-e", "process.exit(17)"], { stdio: "pipe" });
  } catch (error) {
    exitCode = error.status;
  }
  assert.equal(exitCode, 17);
  let fileCode;
  try {
    readFileSync(missingPath);
  } catch (error) {
    fileCode = error.code;
  }
  assert.equal(fileCode, "ENOENT");
  const definitions = [
    [
      "auth_key_revoked",
      "local HTTP 401 response",
      {
        component: "model",
        http_status: observed.auth.status,
        error_code: JSON.parse(observed.auth.body).error.code,
      },
      ["401", "key_revoked"],
    ],
    [
      "rate_limit",
      "local HTTP 429 response",
      {
        component: "model",
        http_status: observed.rate.status,
        ...JSON.parse(observed.rate.body).error,
      },
      ["429", "rate_limit_exceeded"],
    ],
    [
      "quota_exhausted",
      "local HTTP 429 response",
      {
        component: "model",
        http_status: observed.quota.status,
        ...JSON.parse(observed.quota.body).error,
      },
      ["429", "insufficient_quota"],
    ],
    [
      "context_overflow",
      "local HTTP 400 response",
      {
        component: "model",
        http_status: observed.context.status,
        ...JSON.parse(observed.context.body).error,
      },
      ["400", "context_length_exceeded", "9050"],
    ],
    [
      "provider_unavailable",
      "local HTTP 503 response",
      {
        component: "model",
        http_status: observed.unavailable.status,
        ...JSON.parse(observed.unavailable.body).error,
      },
      ["503", "upstream_unavailable"],
    ],
    [
      "stream_incomplete",
      "actual truncated local HTTP response",
      {
        component: "model",
        http_status: 200,
        error_code: observed.truncated.error,
        received_frames: 2,
        expected_frames: 5,
      },
      ["stream_incomplete", "received_frames", "expected_frames"],
    ],
    [
      "proxy_reset",
      "reset socket plus simulated proxy attribution",
      {
        component: "proxy",
        error_code: observed.reset.error,
        event: "upstream_reset",
        phase: "forward_response",
      },
      ["proxy", "upstream_reset", "ECONNRESET"],
    ],
    [
      "response_timeout",
      "actual local request deadline",
      {
        component: "model",
        error_code: "deadline_exceeded",
        transport_code: observed.timeout.error,
        phase: "response_headers",
        deadline_ms: 25,
      },
      ["deadline_exceeded", "response_headers"],
    ],
    [
      "tool_process_failure",
      "actual subprocess exit",
      {
        component: "tool",
        tool: "renderer",
        exit_code: exitCode,
        stderr_type: "render_worker_failed",
      },
      ["renderer", "exit_code", "17"],
    ],
    [
      "file_access_denied",
      "injected adapter permission result",
      { component: "filesystem", error_code: "EACCES", op: "write", errno: 13 },
      ["EACCES", "write"],
    ],
    [
      "file_missing",
      "actual nonexistent local file",
      { component: "configuration", error_code: fileCode, op: "read", file_role: "model_config" },
      ["ENOENT", "model_config"],
    ],
    [
      "remote_receipt_loss",
      "simulated two-device event journal",
      {
        component: "remote_control",
        b_task_status: "succeeded",
        a_task_status: "waiting",
        event: "result_delivery_rejected",
        relay_reason: "endpoint_not_live",
      },
      ["succeeded", "result_delivery_rejected", "endpoint_not_live"],
    ],
    [
      "permission_configuration_missing",
      "injected remote admission result",
      {
        component: "remote_control",
        error_code: "task_permission_mode_required",
        remote_task_permissions: null,
        execution_started: false,
      },
      ["task_permission_mode_required", "remote_task_permissions", "null"],
    ],
  ];
  const cases = [];
  for (const [root, source, fault, proof] of definitions)
    for (let layout = 0; layout < 3; layout++) {
      const id = fingerprint(`redaction-v1|${root}|${layout}`).slice(0, 16);
      const privateValues = {
        api_key: `SIMULATED_KEY_${id}`,
        cookie: `SIMULATED_COOKIE_${id}`,
        prompt: `CONFIDENTIAL_PROMPT_${id}`,
        response_body: `CONFIDENTIAL_OUTPUT_${id}`,
        email: `person_${id}@example.invalid`,
        url: `https://tenant-${id}.example.invalid/v1/chat/completions?api_key=SIMULATED_QUERY_${id}`,
        path: `C:\\Users\\person_${id}\\private_project\\budget_${id}.txt`,
      };
      const requestId = `rq_${id}`,
        taskId = `task_${id}`;
      const meta = { request_id: requestId, task_id: taskId };
      const encode = (object) =>
        Object.entries(object)
          .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
          .join(" ");
      const context = {
        email: privateValues.email,
        url: privateValues.url,
        path: privateValues.path,
        prompt: privateValues.prompt,
        response_body: privateValues.response_body,
      };
      let log;
      if (layout === 0)
        log = [
          encode(meta),
          encode({ ...fault, ...meta }),
          encode({ api_key: privateValues.api_key, cookie: privateValues.cookie }),
          encode(context),
        ].join("\n");
      if (layout === 1)
        log = [
          encode(meta),
          encode({ ...fault, ...meta, api_key: privateValues.api_key }),
          encode(context),
        ].join("\n");
      if (layout === 2)
        log = [
          JSON.stringify({ event: "request_started", ...meta }),
          JSON.stringify({
            ...fault,
            ...meta,
            headers: {
              authorization: `Bearer ${privateValues.api_key}`,
              cookie: privateValues.cookie,
            },
            ...context,
          }),
        ].join("\n");
      cases.push({
        id,
        root,
        source,
        layout,
        proof,
        notes: "An operation failed. Please determine what the supplied evidence supports.",
        log,
        linkage: [requestId, taskId],
        sensitive: [
          ...Object.values(privateValues),
          `SIMULATED_QUERY_${id}`,
          `person_${id}`,
          `budget_${id}.txt`,
        ],
      });
    }
  for (const [index, log] of [
    "component=transport error_code=ECONNRESET phase=response_read upstream_attribution=unavailable",
    "component=model http_status=429 response_body_unavailable=true retry_after_unavailable=true",
    "component=app diagnostic_code=shutdown_unconfirmed inferred=true previous_task_status=interrupted shutdown_reason=unavailable",
  ].entries())
    cases.push({
      id: fingerprint(`unknown|${index}`).slice(0, 16),
      root: "unknown",
      source: "intentional insufficient-evidence control",
      layout: 3,
      proof: [],
      notes: "Determine only what the evidence supports; do not guess.",
      log,
      linkage: [],
      sensitive: [],
    });
  return { cases, observations: observed, known_families: definitions.length };
}
