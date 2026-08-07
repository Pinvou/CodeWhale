# Tool surface

This document describes the current model-facing tool contract. The v0.9.1
cutover that produced it is recorded in `docs/RUNTIME_SIMPLIFICATION_DESIGN.md`;
read the workspace version from `Cargo.toml`, not from this line. The registry
remains larger than the first-turn catalog so
saved transcripts can replay and uncommon capabilities can be loaded on demand.
The model should learn one canonical name for each common operation.

Implementation sources:

- `crates/tui/src/core/engine/tool_catalog.rs` owns the eager/deferred catalog.
- `crates/tui/src/tools/registry.rs` registers canonical tools and hidden aliases.
- `crates/tui/src/tools/{file_tool,git_tool,run_tool,web_tool,shell}.rs` own the
  canonical action schemas.
- `docs/RUNTIME_SIMPLIFICATION_DESIGN.md` records the v0.9.1 cutover and receipt.

## Default-active contract

The default-active policy contains exactly these nine names:

1. `Bash`
2. `File`
3. `Git`
4. `Run`
5. `agent`
6. `load_skill`
7. `remember`
8. `tasks`
9. `todo_write`
10. `tool_search`

The first eight are `DEFAULT_ACTIVE_NATIVE_TOOLS` in
`crates/tui/src/core/engine/tool_catalog.rs`. `tool_search` is synthetic rather
than registry-backed and is always active.

`remember` is registered only when the user enables the built-in memory path;
once present, it stays eager so a model can capture a durable preference without
first discovering the tool. A memory-disabled runtime omits that registration and
therefore exposes eight of the nine policy names.

`update_plan` is **hidden from the model**. It is registered
(`crates/tui/src/tools/plan.rs:401`) but `model_visible()` returns `false`
(`plan.rs:408-413`), and `build_api_tools` filters on that (`registry.rs:235`),
so it never enters the API tool list — which is what `tool_search` indexes.
`tool_search` cannot surface it either. Its own description calls it a "Legacy
compatibility tool for loading older Plan artifacts", and
`update_plan_is_hidden_replay_compatibility` (`plan.rs:598-605`) pins that.

Plan mode narrows the active set: `Bash` and `Run` drop out, leaving `File`,
`Git`, `agent`, `load_skill`, `tasks`, `todo_write`, `tool_search`, and — when memory is
enabled — `remember` (`should_register_remember_tool`,
`crates/tui/src/core/engine/tool_setup.rs:113-118`).

The surface is action-based. A model calls one stable tool name and selects the
operation through its `action` field instead of choosing among many synonymous
single-purpose tools.

### Core action tools

| Tool | Actions | Purpose |
|---|---|---|
| `Bash` | `run`, `wait`, `interact`, `cancel` | Run bounded commands, continue background work, send input, and cancel processes. |
| `File` | `read`, `list`, `search_name`, `search_content`, `write`, `edit`, `patch` | Read, find, and modify workspace files with structured, workspace-aware results. |
| `Git` | `status`, `diff`, `log`, `show`, `blame` | Inspect repository state and history without parsing shell output. |
| `Run` | `tests`, `verifiers` | Run project tests or independent verifier gates with structured results. |

`Bash` appears only when the active session/profile permits shell use. Plan
keeps it unavailable. In Act and Operate, the active permission posture,
sandbox, command policy, trusted paths, repository law, and managed policy still
apply. Full Access removes ordinary approval prompts; it does not bypass hard
safety or repository-policy holds.

`File` is capability-filtered by mode. Plan advertises its read-only actions;
write/edit actions require Act or Operate, and `patch` also requires the
apply-patch feature. The same read-before-edit, workspace, and policy checks used
by the former spellings remain in force.

### Coordination tools

| Tool | Purpose |
|---|---|
| `agent` | Dispatch one focused sub-agent run and return an id, compact receipt, and transcript handle. |
| `remember` | Append one terse durable preference or convention when the user has enabled built-in memory. |
| `tasks` | Create, list, read, cancel, gate, and inspect durable task work through one action family. |
| `update_plan` | Registered but not model-visible; replays older Plan artifacts only. New work uses `todo_write` plus a normal Plan-mode response. |
| `todo_write` | Replace the concrete To-do / Work progress projection for the active thread or durable task. |
| `tool_search` | Discover and load a deferred tool only when the current turn needs it. |

`todo_write` writes the **sole canonical Work ledger**. `update_plan` is
conversational reasoning — strategy, constraints, and route notes that help a
reader understand the approach. It is not a second Work surface, and plan-only
state never becomes model-facing Work grounding.

That distinction is enforced at the request boundary (#3983): the current To-do
snapshot is rendered by one bounded renderer
(`crates/tui/src/work_grounding.rs`) and appended to each parent turn-loop and
sub-agent step request as a transient `<codewhale:work_state>` block. Forked
sub-agents and `/relay` handoffs embed the byte-identical body. An empty To-do
emits no block at all.

## Deferred and dynamic tools

`Web` is a conditional, deferred action tool with `search`, `fetch`, and `wait`
actions. It is discoverable through `tool_search` only when the active network
policy and runtime backend permit it; it is not one of the ten default-active
names.

The durable `github`, `automation`, and `rlm` action families are also deferred
by default. `rlm` owns `open`, `eval`, `configure`, and `close` actions for a
persistent sandboxed Python session. Feature-gated native tools may be added to
the active or deferred catalog only when their implementation and host
dependencies are available.

MCP tools are dynamic. Successfully connected servers register names such as
`mcp_<server>_<tool>` from `~/.codewhale/mcp.json`; a failed or disabled server
must not be presented as an available model tool.

## Inspect the model-client request tool payload

Run `/tools` after a model turn to inspect a bounded projection of the exact
tool field in the latest prepared model-client request. `/tools json` emits the
same evidence as bounded machine-readable JSON. Both formats open in a pager;
they are not copied into transcript history. `/tool-studio` remains a human-
command compatibility alias; it is not a model tool.

The snapshot distinguishes an absent tool field from a present empty array. It
reports the exact model-client tool JSON byte count and SHA-256 digest only when
measurement fits the one-MiB inspection bound; larger payloads stay unavailable.
Provider adapters may transform, sanitize, or omit those fields while building
a provider-specific wire body, so `/tools` marks provider delivery and the wire
payload unavailable. Capture and rendering are bounded: retained schemas,
descriptions, caller lists, catalog rows, turn IDs, and payload measurement all
carry explicit truncation, omission, or unavailable receipts. The snapshot stays
in memory only for the current session and is replaced on each prepared request.

Provider, model, approval, registry provenance, and runtime capability metadata
are not fields in the request tool schema. `/tools` therefore reports them as
unavailable instead of joining against mutable state or inferring values. Use
the separate route and permission receipts for those facts.

## Modes and permission postures

Modes and permission postures are separate controls:

- **Plan** is read-only. It exposes the read-only `File` projection and other
  safe inspection capabilities, but no shell or file mutation.
- **Act** is ordinary interactive execution.
- **Operate** uses the same direct-tool authority as Act while preferring Fleet
  workers for independent, parallel, isolated, background, or long-running work.
- **Ask**, **Auto-Review**, and **Full Access** control approval behavior within
  an action-capable mode. They never widen a Plan turn into write access.

See `docs/MODES.md` for the full mode and posture contract.

## Removed spellings

The per-action single-purpose names below are **not registered**. They were
deleted, not hidden: a call to any of them fails with `tool '<name>' is not
registered`, because `resolve` has deliberately no fuzzy step
(`crates/tui/src/tools/registry.rs:313-316` — "a hallucinated name must fail,
never dispatch"). There is no replay path for them; a transcript that calls one
will not re-execute.

| Removed spelling | Use instead |
|---|---|
| `exec_shell`, `exec_shell_wait`, `exec_wait`, `exec_shell_interact`, `exec_interact`, `exec_shell_cancel` | `Bash`: `run`, `wait`, `interact`, `cancel` |
| `read_file`, `list_dir`, `grep_files`, `file_search`, `write_file`, `edit_file` | `File`: `read`, `list`, `search_content`, `search_name`, `write`, `edit` |
| `git_status`, `git_diff`, `git_log`, `git_show`, `git_blame` | `Git`: matching action |
| `run_tests`, `run_verifiers` | `Run`: `tests`, `verifiers` |
| `web_search`, `fetch_url`, `wait_for_dev_server` | `Web`: `search`, `fetch`, `wait` |

Enforced by `shell_surface_contains_only_the_canonical_bash_tool`
(registry.rs:2290, `"{alias} must be removed"`) and the retired-name loop at
registry.rs:2066-2088 (`"{retired} must stay removed"` /
`"{retired} must not be advertised"`).

## Replay-only aliases

There is exactly one: `apply_patch`.

| Replay-only spelling | Canonical action |
|---|---|
| `apply_patch` | `File`: `patch` (also DeepSeek Responses' one custom tool) |

It is registered as a `FileTool::alias` (registry.rs:831) and hidden from the
advertised catalog — `registry.rs:2092-2093` asserts both halves: `contains`
is true, and no API tool carries the name.

Every other legacy spelling that used to be listed here is **removed, not
hidden**. Calling one hard-errors as an unknown tool; there is no replay
compatibility for them. Tests pin the removals:

| Removed spellings | Use instead | Pinned by |
|---|---|---|
| `task_create`, `task_list`, `task_read`, `task_cancel`, `task_gate_run` | `tasks` | `runtime_task_families_expose_only_canonical_tools`, registry.rs:2337-2371 |
| `pr_attempt_*` | `tasks` | same test |
| `github_issue_context`, `github_pr_context`, `github_comment`, `github_close_issue`, `github_close_pr` | `github` | same test |
| `automation_create/list/read/update/pause/resume/delete/run` | `automation` | same test |
| `rlm_session_objects`, `rlm_open`, `rlm_eval`, `rlm_configure`, `rlm_close` | `rlm` | `rlm_is_the_only_registered_session_surface`, registry.rs:1519-1538 |
| `todo_add/update/list`, `checklist_add/list` (removed); `work_update`, `TodoWrite`, `todo`, `checklist_write/update` (registered hidden aliases) | `todo_write` | registry alias assertions (`registry.rs`) |

This matches the "Removed spellings" section above rather than contradicting
it. Replay compatibility does not make an alias a supported spelling for new
model calls; `apply_patch` execution must stay behaviorally equivalent to
`File`: `patch` and must not be added back to the advertised catalog.

## Long-running work

Use `Bash` with `action: "run"` for bounded commands. Set its background option
for work that may outlive a normal foreground wait, then use `wait`, `interact`,
or `cancel` against the returned process id. Live shell jobs are also visible in
`/jobs`; process-local jobs must be marked stale after restart rather than shown
as reattached processes.

Use `tasks` when the work itself needs a durable lifecycle, structured gates,
artifacts, replayable timelines, or a stable task id. Large tool results should
remain behind bounded handles or artifacts instead of being copied wholesale
into the parent transcript.

## Parallel fan-out

The sub-agent capacity source of truth is
`crates/tui/src/config/subagent_limits.rs`:

- default configured concurrency: **64**;
- maximum configured concurrency: **128**;
- maximum admitted running-plus-queued work: **1024**.

These are capacity ceilings, not advice to dispatch every available slot. A
manager should use the smallest useful fan-out, preserve a single owner for
fan-in, and verify worker receipts before reporting combined completion.

RLM child-query batching is a different, cheaper cost class. Its
`sub_query_batch` helper accepts 1–16 one-shot children inside a live `rlm`
session; it is not a substitute for tool-carrying `agent` workers.

## Human inspection: `/tools` (`/tool-studio`)

`/tools` renders a **read-only, bounded human projection** of the tool field of
the request that was prepared for one `(turn, step)`. It is not a second
registry and not an execution surface.

**The seam.** The snapshot is built in `crates/tui/src/core/engine/turn_loop.rs`
immediately after `MessageRequest` is constructed, from `request.tools` — the
same value the model client is handed. The engine resolves the surrounding
per-turn data once in `engine.rs` (`ToolSurfaceContext`: flattened registry
facts, the MCP pool's own server attribution, the engine-injected catalog names,
and the resolved model client's receipt) and passes it as plain data, so the
per-step seam never re-locks the MCP pool or holds a tool object.

**Turn and step identity.** The tool set can differ between steps of a turn, so
each snapshot is stamped with turn id and step and each seam emits its own. The
TUI keeps only the latest (`SessionState.last_tool_request_snapshot`). Before
the first seam there is no snapshot and `/tools` says so rather than rebuilding
a registry in the UI.

Two kinds of fact are kept apart:

- **Wire facts** come from the prepared request: name, description, schema,
  `defer_loading` / `strict` / `allowed_callers` / `cache_control`, byte
  accounting, and the catalog digest.
- **Surface facts** come from the `ToolSurfaceContext`: provenance
  (`builtin` / `plugin` / `mcp` / `synthetic` / `unknown`), MCP server identity,
  declared capabilities, declared approval requirement, and model visibility.

Contract:

- **One digest.** `active_tool_catalog_sha256`
  (`crates/tui/src/core/engine/preview.rs`) is the single definition of the
  active-tool-catalog hash. The request manifest publishes it as
  `ToolSurfaceFacts::active_tool_catalog_sha256` and `/tools` reports the same
  value for the same prepared request; neither surface keeps a hash of its own.
- **Nothing is guessed.** MCP server identity is shown only when the real pool
  attributed that exact model tool name. `McpPool::mcp_model_tool_name` is the
  single definition shared by the model catalog and the human attribution, and
  an ambiguous name (two servers colliding on one model name) resolves to no
  server. Synthetic provenance comes from
  `default_synthetic_catalog_tool_names`, which is asserted against the engine's
  own `is_synthetic_catalog_tool` predicate. A transmitted tool with no registry
  entry reports `capabilities: unknown`, never "none".
- **Provider availability follows the resolved client.** It comes from
  `Engine::tool_surface_provider_receipt`, never from "a tool registry exists".
  With no client the receipt is `unavailable` even when the registry is full.
- **Unknown shrinks, it does not vanish.** `unavailable_for_this_request` always
  contains `provider_wire_payload`: nothing on this path observes what the
  provider adapter finally transmits. It additionally contains `provider` and
  `model` without a resolved client, and `provenance` / `capabilities` /
  `approval` when no surface context was captured.
- **Absent stays distinct from empty.** A request with no tools field is not a
  request with an empty tools array; an unresolved field is `unknown` with a
  reason, not a default.
- **Bounded.** Rendering is capped by tool count (32), name, description, schema
  bytes, allowed-caller count, and a payload measurement bound, each with an
  explicit truncation or omission receipt. Registered tools that this request
  does *not* carry are reported as a bounded name list plus an exact count
  rather than expanding the projection.
- **Inert.** The snapshot lives beside the transcript, never in
  `session.messages`, so it cannot enter a model request or perturb the
  provider's prefix cache. It never executes a tool, never reads credentials,
  never reorders the catalog, and is never registered as a model-callable tool.
- **Delivery is never claimed.** The capture happens before connection setup, so
  `delivery_status` stays `unknown`.

## Release verification

Do not infer the public surface from handler function names. Verify the model
catalog and alias visibility at the exact candidate SHA:

```bash
python3 scripts/measure-runtime-contract.py
cargo test -p codewhale-tui --lib --locked tools::registry::tests::shell_surface_contains_only_the_canonical_bash_tool -- --exact
cargo test -p codewhale-tui --lib --locked tools::registry::tests::runtime_task_families_expose_only_canonical_tools -- --exact
cargo test --locked -p codewhale-tui --lib core::engine::tests::print_mode_tool_catalog_metrics -- --ignored --exact --nocapture
```

Check the test names against the source before trusting a green run: `cargo test`
exits 0 with "0 passed; N filtered out" when a filter matches nothing, so a
misspelled filter is indistinguishable from a pass. (Three filters printed here
before v0.9.4 named tests that did not exist.)

The provider-free full-policy receipt enables built-in memory and must report the
ten default-active names listed above. A memory-disabled receipt truthfully omits
`remember` and reports nine. A separate repository-wide tool count may include deferred, dynamic,
feature-gated, and replay-only registrations; it is not the number of tools
placed in the first-turn model catalog.
