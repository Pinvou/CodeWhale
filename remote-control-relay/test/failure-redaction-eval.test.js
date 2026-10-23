import assert from "node:assert/strict";
import test from "node:test";
import { fieldRedactor } from "../eval/failure-redaction/candidate.js";
import { gradeAnswer } from "../eval/failure-redaction/grading.js";

test("offline field prototype preserves error evidence and token counts without preserving credentials", () => {
  const clean = fieldRedactor("fixture-only-salt");
  const input =
    "http_status=429 error_code=rate_limit_exceeded input_tokens=9050 api_key=SIMULATED_FIXTURE_CREDENTIAL";
  const output = clean(input);
  assert.match(output, /http_status=429/);
  assert.match(output, /rate_limit_exceeded/);
  assert.match(output, /input_tokens=9050/);
  assert(!output.includes("SIMULATED_FIXTURE_CREDENTIAL"));
  assert.equal(clean(output), output);
  const content = clean(
    "prompt=CONFIDENTIAL_FIXTURE_TEXT response_body=CONFIDENTIAL_FIXTURE_OUTPUT",
  );
  assert(!content.includes("CONFIDENTIAL"));
  assert.equal(clean(content), content);
});

test("blind grading requires causal evidence and rejects invented citations or private-value quotes", () => {
  const expected = { root: "quota_exhausted", sensitive: ["SIMULATED_FIXTURE_CREDENTIAL"] };
  const packet = { notes: "", log: "http_status=429 code=insufficient_quota" };
  const grade = (evidence) =>
    gradeAnswer(expected, packet, { root_cause: "quota_exhausted", evidence });
  assert.equal(grade(["429"]).supported_correct, false);
  assert.equal(grade(["insufficient_quota"]).supported_correct, true);
  assert.equal(grade(["The provider confirmed insufficient_quota"]).supported_correct, false);
  assert.equal(
    gradeAnswer(
      expected,
      { notes: "", log: "insufficient_quota SIMULATED_FIXTURE_CREDENTIAL" },
      {
        root_cause: "quota_exhausted",
        evidence: ["insufficient_quota SIMULATED_FIXTURE_CREDENTIAL"],
      },
    ).supported_correct,
    false,
  );
});
test("offline field prototype aliases correlation IDs per report and strips nested content values", () => {
  const raw =
    '{"request_id":"rq_fixture_opaque","headers":{"authorization":"Bearer SIMULATED_FIXTURE_CREDENTIAL"},"error":{"code":"context_length_exceeded"},"prompt":"CONFIDENTIAL_FIXTURE_TEXT"}\nrequest_id=rq_fixture_opaque';
  const first = fieldRedactor("fixture-report-1")(raw),
    second = fieldRedactor("fixture-report-2")(raw);
  assert(!first.includes("rq_fixture_opaque"));
  assert(!first.includes("SIMULATED_FIXTURE_CREDENTIAL"));
  assert(!first.includes("CONFIDENTIAL_FIXTURE_TEXT"));
  assert.match(first, /context_length_exceeded/);
  const aliases = first.match(/\[id:[a-f0-9]{12}\]/g);
  assert.equal(aliases.length, 2);
  assert.equal(aliases[0], aliases[1]);
  assert.notEqual(first, second);
  assert.equal(fieldRedactor("fixture-report-1")(first), first);
});
