---
name: plugin-creator
description: Scaffold codewhale local plugin directories and activation notes. Use when the user asks to create, package, or sketch a plugin for codewhale.
---

# Plugin Creator

Use this skill when a user wants a codewhale plugin scaffold or a plan for a
plugin-style extension.

Codewhale recognizes two plugin shapes under `~/.codewhale/plugins/`. A
`plugin.toml` manifest is auto-discovered (gated by its `[when]` rules) and its
`[mcp_servers]` entries merge into MCP config while the plugin is enabled. The
`PLUGIN.md` layout scaffolded by `codewhale setup --plugins` is a packaging
convention only — it becomes active when referenced from a skill, hook, or MCP
server. Be explicit about which shape you are creating.

## Workflow

1. Pick the location:
   - User plugin (auto-discovered): `~/.codewhale/plugins/<plugin-name>/`
   - Workspace folder (packaging only, not auto-discovered):
     `<workspace>/plugins/<plugin-name>/`
2. Normalize names to lower-case hyphen-case.
3. Create `PLUGIN.md` with frontmatter:

```markdown
---
name: my-plugin
description: What this plugin packages or enables.
status: draft
---

# My Plugin

What it does, how to enable it, and any scripts or MCP servers it expects.
```

   For the auto-discovered shape, create `plugin.toml` instead:

```toml
[plugin]
name = "my-plugin"
description = "What this plugin packages or enables."
version = "0.1.0"

[when]
os = ["linux", "macos", "windows"]
```

   Add `[skills] path = "skills"` to bundle skills and `[mcp_servers]` entries
   for MCP servers the plugin provides.

4. Add companion folders only when useful:
   - `skills/` for model instructions
   - `scripts/` for helpers invoked by a skill or hook
   - `mcp/` for an MCP server package or config notes
   - `assets/` for templates, examples, or fixtures
5. Include an activation section that says exactly how the user should turn it
   on today (in the `PLUGIN.md` body, or as `plugin.toml` comments).
6. Validate by listing the created files and checking that the manifest
   (`PLUGIN.md` frontmatter or `plugin.toml` `[plugin]` table) has `name` and
   `description`.

Dropping a `plugin.toml` folder into `~/.codewhale/plugins/` does register the
plugin and merge its enabled MCP servers; a `PLUGIN.md` folder alone changes
nothing until it is wired through a skill, hook, or MCP server. Keep the
scaffold honest about which behavior applies.
