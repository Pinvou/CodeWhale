import { AppError } from "./errors.js";
import { exactKeys, text } from "./validation.js";

const ARTIFACT_ROLES = new Set(["input", "intermediate", "final", "evidence"]);

function defaultArtifactRole(kind) {
  if (kind === "assign" || kind === "request_changes") return "input";
  if (kind === "final_deliver" || kind === "accept") return "final";
  if (kind === "note" || kind === "cancel" || kind.endsWith("_transfer")) return "evidence";
  return "intermediate";
}

function safeHttpUrl(value, name) {
  const raw = text(value, name, { minimum: 1, maximum: 2_000 }).trim();
  let parsed;
  try {
    parsed = new URL(raw);
  } catch {
    throw new AppError(400, "invalid_request", `${name} must be an HTTP(S) URL.`);
  }
  if (!new Set(["http:", "https:"]).has(parsed.protocol)
      || parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new AppError(400, "invalid_request", `${name} must be an HTTP(S) URL without credentials, query or fragment.`);
  }
  return parsed.toString();
}

function stringArray(value, name, maximum = 20) {
  if (!Array.isArray(value) || value.length > maximum) {
    throw new AppError(400, "invalid_request", `${name} must be an array with at most ${maximum} items.`);
  }
  const result = value.map((item, index) => text(item, `${name}[${index}]`, { minimum: 1, maximum: 500 }).trim());
  if (new Set(result).size !== result.length) {
    throw new AppError(400, "invalid_request", `${name} must be unique.`);
  }
  return result;
}

export function normalizeDelivery(value, kind) {
  if (value === undefined || value === null) {
    return { schema_version: 1, artifact_role: defaultArtifactRole(kind), expected_next: "", open_questions: "", git_references: [] };
  }
  exactKeys(value, ["schema_version", "artifact_role", "expected_next", "open_questions", "git_references"]);
  if (value.schema_version !== 1) throw new AppError(400, "invalid_request", "delivery.schema_version must be 1.");
  const artifactRole = text(value.artifact_role, "delivery.artifact_role", { minimum: 1, maximum: 32 });
  if (!ARTIFACT_ROLES.has(artifactRole)) throw new AppError(400, "invalid_request", "delivery.artifact_role is invalid.");
  if (!Array.isArray(value.git_references) || value.git_references.length > 5) {
    throw new AppError(400, "invalid_request", "delivery.git_references must contain at most 5 items.");
  }
  const gitReferences = value.git_references.map((reference, index) => {
    exactKeys(reference, ["repository_url", "commit_sha", "branch", "pull_request_url", "paths", "note"]);
    const commitSha = text(reference.commit_sha, `delivery.git_references[${index}].commit_sha`, { maximum: 64 }).trim().toLowerCase();
    if (commitSha && !/^[0-9a-f]{7,64}$/.test(commitSha)) {
      throw new AppError(400, "invalid_request", `delivery.git_references[${index}].commit_sha is invalid.`);
    }
    return {
      repository_url: safeHttpUrl(reference.repository_url, `delivery.git_references[${index}].repository_url`),
      commit_sha: commitSha,
      branch: text(reference.branch, `delivery.git_references[${index}].branch`, { maximum: 255 }).trim(),
      pull_request_url: reference.pull_request_url
        ? safeHttpUrl(reference.pull_request_url, `delivery.git_references[${index}].pull_request_url`)
        : "",
      paths: stringArray(reference.paths, `delivery.git_references[${index}].paths`),
      note: text(reference.note, `delivery.git_references[${index}].note`, { maximum: 2_000 }).trim(),
    };
  });
  return {
    schema_version: 1,
    artifact_role: artifactRole,
    expected_next: text(value.expected_next, "delivery.expected_next", { maximum: 2_000 }).trim(),
    open_questions: text(value.open_questions, "delivery.open_questions", { maximum: 2_000 }).trim(),
    git_references: gitReferences,
  };
}
