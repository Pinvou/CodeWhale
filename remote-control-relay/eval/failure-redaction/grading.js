// Minimum causal facts are fixed independently from the blind reviewers' answers.
const causalProof = {
  auth_key_revoked: ["key_revoked"],
  rate_limit: ["rate_limit_exceeded"],
  quota_exhausted: ["insufficient_quota"],
  context_overflow: ["context_length_exceeded"],
  provider_unavailable: ["upstream_unavailable"],
  stream_incomplete: ["stream_incomplete"],
  proxy_reset: ["proxy", "upstream_reset"],
  response_timeout: ["deadline_exceeded", "response_headers"],
  tool_process_failure: ["renderer", "17"],
  file_access_denied: ["EACCES", "write"],
  file_missing: ["ENOENT", "model_config"],
  remote_receipt_loss: ["succeeded", "result_delivery_rejected"],
  permission_configuration_missing: ["task_permission_mode_required"],
};

export function gradeAnswer(expected, packet, answer) {
  const material = `${packet.notes}\n${packet.log}`;
  const citations = answer.evidence || [];
  const literal =
    citations.length > 0 &&
    citations.every(
      (quote) => typeof quote === "string" && quote.length > 0 && material.includes(quote),
    );
  const safe = citations.every(
    (quote) => !expected.sensitive.some((value) => quote.includes(value)),
  );
  const supported =
    expected.root === "unknown" ||
    (causalProof[expected.root] || []).every((atom) =>
      citations.some((quote) => quote.includes(atom)),
    );
  return {
    correct: answer.root_cause === expected.root,
    citations_literal: literal,
    citations_sensitive: !safe,
    supported_correct: answer.root_cause === expected.root && literal && safe && supported,
  };
}
