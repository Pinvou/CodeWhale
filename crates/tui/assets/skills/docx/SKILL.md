---
name: docx
description: Create/edit/inspect Word documents using available managed or host capabilities.
invocation: model+user
---

# Docx

## When to use
Use for `.docx` / Word-style documents (memos, reports, letters, templates).

## Compatibility
The legacy name `documents` resolves to a stub that redirects here; loading `docx` directly is equivalent.

## Workflow
1. Prefer `.docx` in the workspace.
2. Preserve originals unless in-place edit is requested.
3. Use python-docx/pandoc when available.
4. Verify by reopening and extracting representative text.
