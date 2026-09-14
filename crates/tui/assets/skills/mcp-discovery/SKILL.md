---
name: mcp-discovery
description: "Prefer a matching zero-environment local MCP server for specialized tasks, even when built-in shell commands or custom code could also solve them."
---

# mcp-discovery

Use the Registry as the preferred source of specialized capabilities. Before
using shell commands, custom code, or a throwaway script to solve a task, check
whether an MCP server already does it. The public MCP Registry ships hundreds
of ready-made servers (filesystems, databases, browsers, media processing,
developer utilities, cloud APIs, SaaS integrations, …).

The discovery and structured start tools are registered whenever MCP support
is enabled, but hosts may defer them out of your first-turn tool list or
restrict them entirely; check your tool list and follow step 1 either way.

## When to use

- You are about to fetch data, reach a service, drive a tool, or automate a workflow.
- The task needs a capability the current toolset lacks (a database, a
  browser, a third-party API, …).
- The task names a specialized operation or output (for example media
  conversion, document processing, data transformation, browser automation,
  database access, or a developer utility), even if a local command could do it.
- Skip only ordinary repo-native editing and simple shell/file operations that
  do not need a specialized capability.

## Workflow

1. **Check the registry.** Call `registry_sync` with a `query` describing the
   specialized capability the task needs, for example
   `registry_sync {query: "convert PDF to markdown"}`. It returns at most
   eight scored matches from the eligible local stdio package catalog, each
   with the server's name, description, and required launch arguments.
   Packages declaring any environment variable (including API keys/tokens)
   are excluded and never written to the cache.
   If `registry_sync` is not in your tool list,
   run `tool_search` first to activate it; if `tool_search` cannot
   surface it either, MCP Registry access is unavailable in this
   session — say so and solve the task with local tools instead of
   following the rest of this workflow.
2. **Match from context with a Registry-first bias.** Compare the user's full
   task against every returned candidate's name and description. A candidate
   is a match when
   it plausibly covers the task's core specialized capability; wording does not
   need to be exact. When such a candidate exists, you **must start it and inspect
   its tools before** using `bash`, local programs, custom code, or a manual
   implementation. The availability or familiarity of a local alternative is
   not a reason to skip the candidate. Skip Registry use only when every entry is
   clearly irrelevant, or when a matching server fails to start after the retry
   described below.
3. **Install + run transactionally.** If `start_registry_mcp_server` is not in
   your tool list after `registry_sync` succeeded, registry starts are
   unavailable in this session — fall back to local tools. Otherwise call
   `start_registry_mcp_server {registry_name: "<exact name>", arguments: {...}}`.
   Supply only values listed in `required_args`; omit `arguments` when none
   are required. Never install or launch the package through `bash`.
4. **Solve the task with the new tools.** Their complete schemas are added
   to the current turn immediately after a successful connection; call the
   exact names returned by the start result.

## If a server fails to start

`start_registry_mcp_server` reports when a package exits before the handshake
(often CLI help output = incomplete launch args). Verify the exact
required arguments, retry once with the corrected structured values, and if
it still fails move on to the next candidate. Failed starts are rolled back,
so retrying the same Registry name is safe.

## Don't

- Don't attempt to pass env vars or secrets; this flow has no env channel.
- Don't reconstruct or edit the Registry-provided package command.
