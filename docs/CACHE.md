# Prompt-cache stability (the pinned prefix)

Provider prompt caches (DeepSeek KV cache, Anthropic `cache_control`) only pay
off when the **byte prefix** of a request matches the previous one: the system
prompt, then the tool catalog, then `messages[0..n-1]`. Any change to those
bytes invalidates the cache for every token after the first difference.

## The invariant

**After session start, the system prompt and tool catalog are frozen bytes.
History only grows. A cache miss is allowed only when we can name why.**

Concretely:

- The **header** (system prompt + tools) is composed once at session start and
  re-composed **only** on an explicit, logged header-change op. The tool loop
  performs **no** mid-loop system-prompt refresh, so an agent writing a file
  (which changes the project-context pack, a directory listing, a skills scan)
  cannot move the pinned prefix under the model's feet mid-turn.
- **History only grows.** Volatile facts the model must see (LSP diagnostics,
  steer input, subagent completions, `<recommended_plugins>` on a matching
  user turn) are appended to the message list, never spliced into the frozen
  prefix. Workspace drift is delivered the same way:
  at the start of each **new user turn** (never mid-tool-loop) the engine
  recomposes the volatile contributors and, if anything differs from what the
  model last saw, appends **one** `<context_update>` user-role message with a
  bounded `+`/`-` line delta (new files in the project pack, edited AGENTS.md
  lines, added skills, memory entries, goal text) *before* the user's message.
  The header bytes stay pinned; the update is a normal append, so the prefix
  still extends. The pinned system prompt tells the model once that updates
  arrive this way. Each delta is delivered exactly once (`/cache stats` shows
  `Context updates: N`).
- Every miss is **attributable**. `PrefixStabilityManager` (`prefix_cache.rs`)
  records each change with a reason and reports it through `/cache stats`.

## What counts as a declared header change

These re-pin the prefix under a logged `change:<what>` reason (an expected,
one-request miss):

| Op | Reason |
| --- | --- |
| `/model` (SetModel) | `change:model` |
| Mode change (agent/plan/operate/yolo) | `change:mode` |
| Goal set / pause / resume / clear / status | `change:goal` |
| Mid-turn tool-surface change (deferred-tool admission/eviction, tool-search activation, runtime MCP tool arrival) | `change:tool_surface` |
| Session sync / restore (SyncSession) | `resume` |
| Session construction | `initial` |

History resets that legitimately invalidate the tail (not the header) are
logged as `reset:<what>` — `reset:compaction`, `reset:clear`.

Anything else that changes the header bytes with **no** declared reason is
**drift**: it is logged as `drift:<component>`, the original pin is **kept**
(so the same undeclared prefix keeps counting as a miss instead of quietly
becoming the new baseline), and `/cache stats` shows a `WARNING`. After the
mid-loop-refresh removal, drift should stay at zero in normal operation; a
non-zero drift count is a real bug to investigate.

## Attribution vs. the old behavior

Two earlier behaviors are rejected, matching the DeepSeek Harness design:

- **Detect-and-report + re-pin on drift.** The manager used to re-pin to the
  new prefix on every change, so `/cache stats` looked "stable" again after one
  bad step while the provider cache was already dead. It now keeps the original
  pin on undeclared drift.
- **Recompose the system prompt from disk on every tool step.** The turn loop
  used to call `refresh_system_prompt()` before every model request, including
  mid-tool-loop. That is removed. Header refreshes happen only at the declared
  edges above.

Tool-result redaction (`prepare_model_bound_request`) is content-preserving
when no secret is configured (the common case), so it does not move the prefix.
When a configured secret appears in a tool result, redacting it is a security
requirement that correctly overrides cache stability for that one message.

## Verifying the fix

`/cache stats` reports prefix stability, the pin reason, the last miss reason,
the undeclared-drift count, and the aggregate provider cache hit rate. In a
coding session, expect the first turn to be a write and every later step —
including steps after the agent writes files — to hit.

### Live end-to-end check (manual, key-gated)

With a real `DEEPSEEK_API_KEY`, run a session that makes the agent take at
least three tool steps in one turn, then open `/cache inspect`. Every request
after the first should report `prompt_cache_hit_tokens > 0`; the base static
prefix hash and the tool-catalog hash must not move between steps. If the hit
drops mid-turn, the pin reason / drift count name the cause.

## KV-cache effect note (for contributors)

Any new contributor to the session context must state its **KV-cache effect**:
does it belong in the frozen prefix (system + tools) or in append-only history?
Never splice a volatile fact (time, an instruction edit, a skill-catalog
change, a project-file change) into the prefix — append it as a user-role
message instead. A later request must be `previous ⊕ suffix` unless a logged
header change or a history reset explains the difference.

DeepSeek strict-mode tool shaping (`prepare_tools_for_strict_mode`) states its
effect here per that rule: it is **frozen-prefix-compatible**. The removal
note appended to a tool's description is a deterministic pure function of the
pinned catalog and the route — the note names are sorted and deduplicated,
the counts are order-independent, and the per-request pair-judge budget
resets at every preparation over a stable catalog order — so on a stable
strict route the tools bytes are identical every step after the first
preparation; the shaping runs on a per-request clone and never mutates the
registry, and the pair-judge budget pools per tool (round 28) so no
catalog order can change a later tool's verdict. One boundary is
deliberate (round 25): the pattern-compile memo persists across requests
while the per-request compile budget resets (the memo is entry-bounded at
512 sources ≈ 8 MB of keys per thread, refuses rather than evicts at the
bound, and the cached automata are the unaccounted part), so a
pattern that abstained on a budget-refused first pass can be judged on a
later pass and flip its tool's verdict — the bytes then differ between the
first preparation and a warm one without a declared change. (The memo is
per OS thread, so the boundary is per thread: two preparations on
different threads hold independent memos; every stability claim above
holds per thread.) Catalogs within
the compile budget (the default catalog is far below it) never see the
boundary. The engine's declared change reasons are `model`, `mode`,
`goal`, `system`, `resume`, and `tool_surface` (plus the
`mcp-session-boot` install edge) — there is no `provider` or `route`
declared reason, and no translation or verbosity field. A provider
retype or a re-planned route does not flip the gate in place: either
change respawns the engine, and the new session re-pins its prefix as
`initial` (round 29 — this paragraph previously invented
`change:provider`/`change:route` fields the tree has never had, and
described provider flips as in-place re-declarations). A gate flip a
declared reason genuinely accompanies — a model switch that moves the
route with it — re-declares through `model`; the one transition with no
declared reason is a config-file endpoint edit between sessions, which
the respawn absorbs. A flip inside the same scope (the canonical beta ↔
non-beta path) leaves the shaped bytes identical and produces no drift
at all.

## Permissions fragment budget compatibility

Configured `instructions = [...]` files have an existing 100 KiB per-file
contract. An embedding host that supplies the same reviewed instructions
inline must not lose their tail when they enter the `Permissions` WorldState
fragment. That fragment therefore retains the 100 KiB instruction ceiling;
all other WorldState fragments keep the shared 40 KiB / 10K-token ceiling.

This remains upstreaming debt: the identity-specific budget currently lives in
the TUI WorldState implementation even though `codewhale-core` owns the shared
fragment constants and still applies its generic ceiling. The reusable fix is
to make the core fragment policy own the `Permissions` exception and have the
TUI delegate to it, without raising the global fragment cap.

## Deferred: full reconstructability (Layer 3)

DeepSeek Harness derives every request from an append-only session log via a
pure `deriveMessages()` projection, so prefix-extension is emergent rather than
managed. Codewhale now pins the header and delivers drift as `<context_update>`
appends; the remaining step is to make the session log the single source of
truth with a pure projection (and to persist the context-update baseline with
it). That is a follow-up lane, not part of this change.
