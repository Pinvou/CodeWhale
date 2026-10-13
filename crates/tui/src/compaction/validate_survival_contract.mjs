#!/usr/bin/env node
// Language-invariant coverage floor for the compaction survival contract.
// Later TypeScript strategies should keep this check; Rust remains the B1
// enforcement path. Run: node validate_survival_contract.mjs
//
// Checkpoint recognition mirrors the Rust `CheckpointEra` (see
// `history_recognition.rs`): a history that carries a provenance-stamped
// carrier is read post-provenance, where only the stamped two-block form is
// a checkpoint and a user turn pasting the bare summary header is content;
// a history without one keeps the loose single-block reading. The mirror
// stays scoped to the fixture matrix: `isPlainUserText` deliberately skips
// the rest of Rust's `classify_user_turn_prompt_in_era` conjuncts
// (tool-result-bearing messages, runtime-owned turn metadata), which the
// matrix never contains.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const MARKERS = [
  "Another language model started to solve this problem",
  "Conversation Summary (Auto-Generated)",
];
const PROVENANCE_STAMP = "<!-- codewhale.compaction-checkpoint.v1 -->";

const root = dirname(fileURLToPath(import.meta.url));
const matrix = JSON.parse(
  readFileSync(join(root, "fixtures/matrix.json"), "utf8"),
);

function userTextOf(message) {
  if (message.role !== "user") return null;
  const text = (message.content ?? [])
    .filter((block) => block.type === "text")
    .map((block) => block.text)
    .join("\n")
    .trim();
  return text || null;
}

// Structural match of the Rust `is_compaction_checkpoint_message`: a user
// message whose first block is a text block beginning with a summary header
// and whose remaining blocks are either empty or exactly the engine-written
// provenance marker.
function isLooseCheckpoint(message) {
  if (message.role !== "user") return false;
  const content = message.content ?? [];
  const [first, ...rest] = content;
  if (!first || first.type !== "text" || first.cache_control != null) {
    return false;
  }
  if (!MARKERS.some((marker) => first.text.startsWith(marker))) return false;
  if (rest.length === 0) return true;
  return (
    rest.length === 1 &&
    rest[0].type === "text" &&
    rest[0].cache_control == null &&
    rest[0].text === PROVENANCE_STAMP
  );
}

// The provenance-stamped form — exactly [text header, text stamp]. Typed
// input cannot produce it; the engine's `compaction_checkpoint_message` is
// the only writer.
function isStampedCheckpoint(message) {
  if (!isLooseCheckpoint(message)) return false;
  const content = message.content ?? [];
  return content.length === 2 && content[1].text === PROVENANCE_STAMP;
}

function eraOf(messages) {
  return { postProvenance: messages.some(isStampedCheckpoint) };
}

function isCheckpoint(message, era) {
  return era.postProvenance ? isStampedCheckpoint(message) : isLooseCheckpoint(message);
}

function isPlainUserText(message, era) {
  return !isCheckpoint(message, era) && Boolean(userTextOf(message));
}

function lastPlainUserIndex(messages, end, era) {
  for (let idx = end - 1; idx >= 0; idx -= 1) {
    if (isPlainUserText(messages[idx], era)) return idx;
  }
  return null;
}

function sliceHasToolResult(messages, start) {
  return messages.slice(start).some((message) =>
    (message.content ?? []).some((block) => block.type === "tool_result"),
  );
}

export function lastRoundStart(messages) {
  const era = eraOf(messages);
  const lastUser = lastPlainUserIndex(messages, messages.length, era);
  if (lastUser === null) return 0;
  if (sliceHasToolResult(messages, lastUser)) return lastUser;
  let candidate = lastUser;
  for (;;) {
    const prev = lastPlainUserIndex(messages, candidate, era);
    if (prev === null) return lastUser;
    if (sliceHasToolResult(messages, prev)) return prev;
    candidate = prev;
  }
}

function toolResultIds(message) {
  return (message.content ?? [])
    .filter((block) => block.type === "tool_result")
    .map((block) => block.tool_use_id);
}

function toolUseIds(message) {
  return (message.content ?? [])
    .filter((block) => block.type === "tool_use")
    .map((block) => block.id);
}

function isAssistantLike(message) {
  return message.role === "assistant" || message.role === "assistant_interrupted";
}

function assistantTextOf(message) {
  if (!isAssistantLike(message)) return null;
  const text = (message.content ?? [])
    .filter((block) => block.type === "text")
    .map((block) => block.text)
    .join("\n")
    .trim();
  return text || null;
}

// A retained copy may be truncated, so a prefix either way counts as survival
// -- but nothing weaker does.
function survives(text, replacement, of) {
  return replacement.some((message) => {
    const kept = of(message);
    return (
      kept &&
      (kept === text || text.startsWith(kept) || kept.startsWith(text))
    );
  });
}

export function validateSurvivalContract(original, replacement, anchors) {
  const era = eraOf(original);
  const start = lastRoundStart(original);
  const lastRound = original.slice(start);
  // Every plain user turn in the round, not the first one `find` reaches: the
  // round spans a tool-bearing turn plus the toolless tail after it, so
  // checking one let a rewrite drop the latest turn. Era-aware on both sides,
  // like the Rust `user_prompt_text_of`: in a post-provenance history the
  // pasted-header turn is plain user text and must survive, and the stamped
  // carrier is neither required to survive nor allowed to witness — its
  // joined text starts with the bare marker, so an era-blind witness would
  // let a rewrite drop a round turn and call it survived by the summary.
  for (const text of lastRound
    .filter((message) => isPlainUserText(message, era))
    .map(userTextOf)
    .filter(Boolean)) {
    if (
      !survives(text, replacement, (message) =>
        isPlainUserText(message, era) ? userTextOf(message) : null,
      )
    ) {
      return "a last-round user message was dropped";
    }
  }
  for (const id of lastRound.flatMap(toolResultIds)) {
    const kept = replacement.some((message) =>
      (message.content ?? []).some(
        (block) => block.type === "tool_result" && block.tool_use_id === id,
      ),
    );
    if (!kept) {
      return `last-round tool result ${id} was dropped`;
    }
  }
  // The call, not just its result: a tool_result whose tool_use was summarized
  // away is an orphan providers reject.
  for (const id of lastRound.flatMap(toolUseIds)) {
    const kept = replacement.some((message) =>
      (message.content ?? []).some(
        (block) => block.type === "tool_use" && block.id === id,
      ),
    );
    if (!kept) {
      return `last-round tool call ${id} was dropped`;
    }
  }
  // Match the text: "some assistant message survived" is satisfied by the
  // summary the rewrite itself just wrote.
  for (const text of lastRound.map(assistantTextOf).filter(Boolean)) {
    if (!survives(text, replacement, assistantTextOf)) {
      return "last-round assistant output was dropped";
    }
  }
  if (
    lastRound.some(isAssistantLike) &&
    !replacement.some(isAssistantLike)
  ) {
    return "last-round assistant output was dropped";
  }
  // The receipt count reads the replacement in its own era — except that a
  // post-provenance original demands the stamped receipt: the engine stamps
  // every replacement it builds, so a retained pasted turn must not satisfy
  // the floor loosely.
  const receiptsEra = era.postProvenance
    ? { postProvenance: true }
    : eraOf(replacement);
  const checkpoints = replacement.filter((message) =>
    isCheckpoint(message, receiptsEra),
  ).length;
  if (checkpoints === 0) return "checkpoint receipt was dropped";
  if (checkpoints > 1) return "prior summaries were duplicated";
  if (anchors && !replacement.some((message) =>
    (message.content ?? []).some((block) => {
      if (block.type === "text") return (block.text ?? "").includes(anchors);
      if (block.type === "tool_result") {
        return (block.content ?? "").includes(anchors);
      }
      return false;
    }),
  )) {
    return "pinned /anchor text was dropped";
  }
  return null;
}

function main() {
  if (matrix.schema_version !== 1) {
    throw new Error(`unexpected schema_version ${matrix.schema_version}`);
  }
  let failed = 0;
  for (const fixture of matrix.cases) {
    if (typeof fixture.last_round_start === "number") {
      const start = lastRoundStart(fixture.original);
      if (start !== fixture.last_round_start) {
        failed += 1;
        console.error(
          `${fixture.id}: last_round_start ${start} != ${fixture.last_round_start}`,
        );
      }
    }
    const error = validateSurvivalContract(
      fixture.original,
      fixture.replacement,
      fixture.anchors,
    );
    const passed = error === null;
    if (fixture.expect === "pass" && !passed) {
      failed += 1;
      console.error(`${fixture.id}: expected pass, got ${error}`);
    } else if (fixture.expect === "fail" && passed) {
      failed += 1;
      console.error(`${fixture.id}: expected fail closed`);
    }
  }
  if (failed > 0) {
    console.error(`${failed} fixture(s) failed`);
    process.exit(1);
  }
  console.log(`ok ${matrix.cases.length} survival-contract fixtures`);
}

const entry = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1];
if (entry || process.argv[1]?.endsWith("validate_survival_contract.mjs")) {
  main();
}
