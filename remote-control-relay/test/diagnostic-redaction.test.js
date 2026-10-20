import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { diagnosticRedactor } from "../diagnostic-redaction.js";
import { FailureReportStore } from "../failure-diagnostics.js";
import { nativeRedact } from "../eval/failure-redaction/native-runner.mjs";

// New formats and canaries, not the prototype's 13-family layout set.
const cases = [
  {
    name: "conflicting normalized aliases do not select a diagnosis by key order",
    text: '{"http_status":401,"httpStatus":"PRIVATE_HELDOUT_STATUS"}',
    keep: [],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "duplicate normalized plain fields are ambiguous and stop continuation",
    text: "http_status=401 http-status=403\nAlice Person",
    keep: [],
    drop: ["401", "403", "Alice Person"],
  },
  {
    name: "JSON exact integer exponent and decimal notation agree across languages",
    text: '{"http_status":4.01e2,"input_tokens":1e4,"exit_code":17.0,"duration_ms":2.5}',
    keep: ["401", "10000", "17"],
    drop: ["2.5"],
  },
  {
    name: "pretty JSON with nested credentials and quoted message",
    text: JSON.stringify(
      {
        http_status: 403,
        error: { code: "forbidden", message: 'unrelated PRIVATE_HELDOUT_MESSAGE "quoted"' },
        headers: {
          "Proxy-Authorization": "Basic PRIVATE_HELDOUT_BASIC",
          "Set-Cookie": ["PRIVATE_HELDOUT_COOKIE"],
        },
      },
      null,
      2,
    ),
    keep: ["403", "forbidden"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "escaped credential key and unknown private key names",
    text: '{"http_status":401,"error_code":"key_revoked","api\\u005fkey":"PRIVATE_HELDOUT_ESCAPED","PRIVATE_HELDOUT_KEYNAME":{"code":"PRIVATE_HELDOUT_CHILD"}}',
    keep: ["401", "key_revoked"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "root array and credential parent wins over nested code",
    text: JSON.stringify([
      {
        error: { code: "rate_limit_exceeded" },
        retry_after_ms: 17000,
        credentials: { code: "PRIVATE_HELDOUT_IN_CREDENTIALS" },
      },
      { http_status: 429, messages: [{ code: "PRIVATE_HELDOUT_IN_MESSAGES" }] },
    ]),
    keep: ["429", "17000", "rate_limit_exceeded"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "safe fields cannot carry private strings or fake numeric values",
    text: JSON.stringify({
      http_status: "PRIVATE_HELDOUT_STATUS",
      exit_code: 2147483648,
      input_tokens: "PRIVATE_HELDOUT_COUNT",
      component: "PRIVATE_HELDOUT_COMPONENT",
      code: "aHR0cHM6Ly9leGFtcGxlLmludmFsaWQvUFJJVkFURV9IRUxET1VU",
      request_id: "",
    }),
    keep: [],
    drop: ["PRIVATE_HELDOUT", "2147483648", "aHR0cHM6"],
  },
  {
    name: "technical counts survive despite token being a credential word",
    text: 'HTTP_STATUS=400 ERROR_CODE=context_length_exceeded input_tokens=12001 limit=12000 access_token="PRIVATE_HELDOUT_ACCESS"',
    keep: ["400", "context_length_exceeded", "12001", "12000"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "unframed content never resumes on safe-looking next lines",
    text: "error_code=insufficient_quota\nprompt=PRIVATE_HELDOUT_FIRST\n\nhttp_status=401\nPRIVATE_HELDOUT_LAST",
    keep: ["insufficient_quota"],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "malformed JSON never salvages fields inside body",
    text: '{"prompt":"PRIVATE_HELDOUT_START\nhttp_status=401\nPRIVATE_HELDOUT_END',
    keep: [],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "unclosed key value quote never salvages another record",
    text: 'code="PRIVATE_HELDOUT_UNCLOSED\nhttp_status=401',
    keep: [],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "folded authorization blocks later lines",
    text: "Authorization: Basic\n PRIVATE_HELDOUT_FOLDED\nhttp_status=401",
    keep: [],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "PEM block does not expose body between headers",
    text: "-----BEGIN PRIVATE KEY-----\nUFJJVkFURV9IRUxET1VUX1NFTlRJTIRV\n-----END PRIVATE KEY-----\nhttp_status=401",
    keep: [],
    drop: ["UFJJVk", "401"],
  },
  {
    name: "NDJSON with final newline keeps fault and removes unknown strings",
    text: '{"http_status":502,"error_code":"provider_unavailable"}\n{"output_text":"PRIVATE_HELDOUT_OUTPUT"}\n',
    keep: ["502", "provider_unavailable"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "quoted credential containing spaces cannot turn into a second record",
    text: 'http_status=401 api_key="PRIVATE_HELDOUT_WITH_SPACES and symbols" error_code=key_revoked',
    keep: ["401", "key_revoked"],
    drop: ["PRIVATE_HELDOUT", "symbols"],
  },
  {
    name: "duplicate exact key follows parser semantics without exposing secret",
    text: '{"code":"PRIVATE_HELDOUT_FIRST","code":"EACCES","op":"write","content":{"code":"PRIVATE_HELDOUT_SECOND"}}',
    keep: ["EACCES", "write"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "free text addresses, paths, encodings and controls fail closed",
    text: "contact somebody@heldout.invalid C:\\Users\\PRIVATE_HELDOUT\\config.json\nhttp_status=401",
    keep: [],
    drop: ["somebody", "PRIVATE_HELDOUT", "401"],
  },
  {
    name: "root JSON strings are content, not diagnostics",
    text: '"PRIVATE_HELDOUT_ROOT_STRING"',
    keep: [],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "unknown unframed fields also suppress continuation bodies",
    text: "custom_payload=PRIVATE_HELDOUT_START\nAlice Person\nhttp_status=401",
    keep: [],
    drop: ["PRIVATE_HELDOUT", "Alice Person", "401"],
  },
  {
    name: "invalid typed field cannot introduce unframed continuation content",
    text: "input_tokens=first\nAlice Person\nhttp_status=401",
    keep: [],
    drop: ["first", "Alice Person", "401"],
  },
  {
    name: "unknown code aliases do not permit multiline body recovery",
    text: "code=custom_private_code\nAlice Person\nhttp_status=401",
    keep: [],
    drop: ["custom_private_code", "Alice Person", "401"],
  },
  {
    name: "markdown blocks are not reproduction prose",
    text: "```\nAlice Person\nhttp_status=401\n```",
    keep: [],
    drop: ["Alice Person", "401"],
  },
  {
    name: "unlabelled AWS-shaped fixture access key is removed",
    text: "AKIAABCDEFGHIJKLMNOP\nhttp_status=401",
    keep: [],
    drop: ["AKIA", "401"],
  },
  {
    name: "opaque IDs agree across native and server implementations",
    text: "request_id=PRIVATE_HELDOUT_ID task_id=PRIVATE_HELDOUT_TASK\nrequest_id=PRIVATE_HELDOUT_ID task_id=PRIVATE_HELDOUT_TASK",
    keep: ["request_id", "task_id"],
    drop: ["PRIVATE_HELDOUT"],
  },
  {
    name: "unclosed root string is an ambiguous content block",
    text: '"PRIVATE_HELDOUT_ROOT\nhttp_status=401',
    keep: [],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "unicode directional controls cannot hide credentials in prose",
    text: "plain\u202ePRIVATE_HELDOUT_CONTROL\nhttp_status=401",
    keep: [],
    drop: ["PRIVATE_HELDOUT", "401"],
  },
  {
    name: "unknown nested fields cannot smuggle paths or private values",
    text: JSON.stringify({
      error: { code: "ENOENT", file_role: "model_config" },
      context: {
        url: "https://private-heldout.invalid?token=PRIVATE_HELDOUT",
        path: "/srv/PRIVATE_HELDOUT",
        email: "heldout@example.invalid",
      },
    }),
    keep: ["ENOENT", "model_config"],
    drop: ["PRIVATE_HELDOUT", "private-heldout", "heldout@example"],
  },
];

test("Relay field redaction preserves supported facts and removes all held-out canaries", () => {
  for (const item of cases) {
    const clean = diagnosticRedactor("heldout-private-scope")(item.text);
    for (const fact of item.keep) assert(clean.includes(fact), `${item.name}: lost ${fact}`);
    for (const secret of item.drop)
      assert(!clean.includes(secret), `${item.name}: retained a canary`);
  }
});

test("production Rust and Relay preserve supported facts and remove held-out sensitive material", {
  skip: process.env.PINVOU_TEST_NATIVE_REDACTION !== "1",
  timeout: 180000,
}, () => {
  const base = fileURLToPath(new URL("../../.cache/failure-redaction-comparison", import.meta.url));
  mkdirSync(base, { recursive: true });
  const work = mkdtempSync(join(base, "boundary-"));
  const native = nativeRedact(
    cases.map((item, index) => ({
      id: String(index),
      section: "log",
      scope: "heldout-private-scope",
      text: item.text,
    })),
    work,
  );
  for (const [index, item] of cases.entries()) {
    const js = diagnosticRedactor("heldout-private-scope")(item.text),
      rust = native.get(`${index}:log`);
    for (const [stage, text] of [
      ["server", js],
      ["client", rust],
      ["pipeline", diagnosticRedactor("server-report-scope")(rust)],
    ]) {
      for (const fact of item.keep)
        assert(text.includes(fact), `${item.name}: ${stage} lost ${fact}`);
      for (const secret of item.drop)
        assert(!text.includes(secret), `${item.name}: ${stage} retained a canary`);
    }
    // Key ordering differs across languages; compare the parsed records.
    const parse = (text) =>
      text.split("\n").map((line) => {
        try {
          return JSON.parse(line);
        } catch {
          return line;
        }
      });
    assert.deepEqual(parse(rust), parse(js), item.name);
  }
});

test("report-scoped pseudonyms keep correlation across files but change across reports and admission layers", () => {
  const raw =
    "request_id=PRIVATE_HELDOUT_REQUEST task_id=PRIVATE_HELDOUT_TASK\nrequest_id=PRIVATE_HELDOUT_REQUEST task_id=PRIVATE_HELDOUT_TASK";
  const first = diagnosticRedactor("private-report-one")(raw);
  const second = diagnosticRedactor("private-report-two")(raw);
  const aliases = first.match(/\[id:[a-f0-9]{24}\]/g);
  assert.equal(aliases.length, 4);
  assert.equal(aliases[0], aliases[2]);
  assert.equal(aliases[1], aliases[3]);
  assert.notEqual(first, second);
  assert(!first.includes("PRIVATE_HELDOUT"));
  assert.notEqual(diagnosticRedactor("server-scope")(first), first);
  // Never trust even a well-formed client alias as an authenticated identity.
  assert.notEqual(
    diagnosticRedactor("server-scope-two")(first),
    diagnosticRedactor("server-scope-one")(first),
  );
});

test("large and deep input is bounded and never leaks nested content", () => {
  const deep = '{"error":'.repeat(300) + '{"prompt":"PRIVATE_HELDOUT_DEEP"}' + "}".repeat(300);
  assert(!diagnosticRedactor()(deep).includes("PRIVATE_HELDOUT"));
  assert.equal(diagnosticRedactor()("x".repeat(65537)), "[unparsed material removed]");
  const many = JSON.stringify(Array.from({ length: 300 }, () => ({ code: "EACCES" })));
  assert.equal(JSON.parse(diagnosticRedactor()(many)).length, 128);
});

const device = {
  device_id: "heldout-device",
  behavior_collection: { status: "active", collection_period_id: "heldout_collection_period" },
};
const incident = {
  device_id: device.device_id,
  diagnostic_id: "heldout_incident_01",
  collection_period_id: "heldout_collection_period",
  diagnostic_code: "tool_failed",
  diagnostic_component: "tool",
  stage: "tool",
};
const packet = (extra = {}) => ({
  report_id: "heldout_report_01",
  incident_id: incident.diagnostic_id,
  collection_period_id: incident.collection_period_id,
  notice_version: "failure-diagnostics-v1",
  confirmed_at: Date.now(),
  notes: "request_id=PRIVATE_HELDOUT_SCOPE",
  attachments: [
    { kind: "text", content: "request_id=PRIVATE_HELDOUT_SCOPE error_code=EACCES op=write" },
  ],
  ...extra,
});

test("server private scope survives audit, issue, prune, restart and credential-period renewal", (t) => {
  const directory = temporary(t, "pinvou-redaction-scope-");
  const store = new FailureReportStore(directory),
    input = packet();
  store.append(device, input, incident);
  const key = store.state.redaction_key;
  const detail = store.detail(incident, "reviewer");
  const original = detail.reports[0].notes;
  assert.equal(
    original.match(/\[id:[a-f0-9]{24}\]/)[0],
    detail.reports[0].attachments[0].content.match(/\[id:[a-f0-9]{24}\]/)[0],
  );
  assert(!JSON.stringify(detail).includes(key));
  store.update(
    store.state.reports[0].fingerprint,
    { status: "open", fixed_version: "" },
    "reviewer",
  );
  store.prune(new Map([[device.device_id, device]]));
  const restored = new FailureReportStore(directory);
  assert.equal(restored.state.redaction_key, key);
  assert.equal(restored.append(device, input, incident).status, "received");
  const renewed = {
    ...device,
    behavior_collection: { status: "active", collection_period_id: "heldout_renewed_period" },
  };
  assert.equal(
    restored.append(
      renewed,
      {
        ...input,
        collection_period_id: renewed.behavior_collection.collection_period_id,
        incident_period_id: incident.collection_period_id,
      },
      incident,
    ).status,
    "received",
  );
  assert.equal(restored.detail(incident, "reviewer").reports[0].notes, original);
  restored.append(device, { ...input, report_id: "heldout_report_02" }, incident);
  assert.notEqual(restored.state.reports[0].notes, restored.state.reports[1].notes);
});

test("legacy state migrates material without changing its retry digest or allowing changed evidence", (t) => {
  const directory = temporary(t, "pinvou-redaction-legacy-");
  const input = packet({
    notes: "http_status=401\nprompt=PRIVATE_HELDOUT_LEGACY",
    attachments: [],
  });
  // This pre-upgrade input was unchanged by the previous server regex redactor.
  const hash = (value) => createHash("sha256").update(value).digest("hex");
  const digest = hash(
    JSON.stringify([
      input.report_id,
      input.incident_id,
      input.collection_period_id,
      input.notice_version,
      input.confirmed_at,
      input.notes,
      [],
    ]),
  );
  const fingerprint = hash("tool|tool|tool_failed");
  writeFileSync(
    join(directory, "failure-diagnostic-reports.json"),
    JSON.stringify({
      reports: [
        { ...input, device_id: device.device_id, fingerprint, received_at: Date.now(), digest },
      ],
      issues: {},
      audit: [],
    }),
  );
  const store = new FailureReportStore(directory);
  assert.equal(store.healthy, true);
  assert.equal(store.state.reports[0].redaction_version, 1);
  const onDisk = readFileSync(join(directory, "failure-diagnostic-reports.json"), "utf8");
  assert(!onDisk.includes("PRIVATE_HELDOUT_LEGACY"));
  assert(onDisk.includes("redaction_key"));
  assert.equal(store.append(device, input, incident).status, "received");
  assert.throws(
    () =>
      store.append(
        device,
        { ...input, notes: "http_status=403\nprompt=PRIVATE_HELDOUT_LEGACY" },
        incident,
      ),
    /report_conflict/,
  );
  assert.equal(
    new FailureReportStore(directory).append(device, input, incident).status,
    "received",
  );
});

function temporary(t, prefix) {
  const directory = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => {
    assert.equal(dirname(resolve(directory)), resolve(tmpdir()));
    assert(directory.startsWith(join(tmpdir(), prefix)));
    rmSync(directory, { recursive: true, force: true });
  });
  return directory;
}
