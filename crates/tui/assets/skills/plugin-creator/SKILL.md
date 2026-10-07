---
name: plugin-creator
description: Scaffold a local Codewhale plugin bundle with a versioned manifest, namespaced Skills, and an explicit trust review.
---

# Plugin Creator

Use this skill when a user wants a local Codewhale plugin bundle. The
plugin loader is deliberately bounded: trusted and enabled bundles may add
declarative Skills and MCP servers through the existing engines. Other
component kinds are inventory-only. `plugin.json` is the native Agent
Plugins manifest (`plugin.toml` is the legacy Codewhale format and stays
readable); distribution goes through `/plugin marketplace add|install`, not
a bundle-carried downloader.

## Workflow

1. Pick a Codewhale-owned location:
   - User bundle: `~/.codewhale/plugins/<plugin-name>/`
   - Workspace bundle: `<workspace>/.codewhale/plugins/<plugin-name>/`
2. Normalize the bundle name to lowercase hyphen-case.
3. Create `plugin.json` (the native Agent Plugins manifest):

```json
{
  "$schema": "https://agent-plugins.org/schemas/plugin.json",
  "name": "my-plugin",
  "version": "0.1.0",
  "description": "What this bundle provides",
  "extensions": {
    "net.codewhale": {
      "skills": { "path": "skills" }
    }
  }
}
```

4. Put each Skill under `skills/<skill-name>/SKILL.md`. Codewhale exposes it
   as `my-plugin:<skill-name>`, never as an unqualified command.
5. Add a sibling `mcp.json` (`mcpServers` envelope — see
   `plugins/computer-use/mcp.json` in the engine tree) only when the bundle
   needs an existing MCP engine. Keep stdio commands and paths inside the
   bundle. Map local environment values only as exact `${SOURCE_ENV}`
   references. For remote MCP, use HTTPS (or loopback HTTP), forbid URL user
   information/query/fragment, use only environment-backed headers or bearer
   tokens, and declare the exact normalized endpoint host set in
   `extensions["net.codewhale"].capabilities.network_hosts`. Never place
   credentials in the manifest.
6. Declare commands, agents, hooks, LSP, native extensions, filesystem roots,
   or lifecycle mutation only when inventorying future work. Codewhale shows
   them as inactive and still activates reviewed Skills and MCP from the same
   bundle. A bundle that only declares those unsupported surfaces cannot be
   enabled.
7. Validate and review without executing bundle content:
   - `/plugin validate <plugin-name>`
   - `/plugin show <plugin-name>`
   - `/plugin enable <plugin-name>` to open the content/capability review
   - run the exact `/plugin trust ...` confirmation shown, then enable again
8. Verify `/skills inspect` reports plugin provenance and `/plugin list`
   reports the expected trust and activation state. Trust stages the reviewed
   content but does not activate it; enablement rebuilds the current
   workspace's Skill/MCP catalogue immediately.

Every user and workspace bundle starts untrusted and disabled. A bundle
must not carry its own downloader, updater, compatibility scan, executable
extension runtime, or automatic trust flow — discovery and installation are
the engine's job (`/plugin marketplace ...`, `/plugin install|update|
uninstall`), and trust stays a user decision.
