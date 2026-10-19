---
name: presentations
description: Compatibility stub owning the legacy name presentations; the pptx skill provides the workflow.
aliases-for: pptx
---

# presentations (compatibility stub)

This stub exists so the legacy name `presentations` keeps resolving. It contains no
workflow of its own.

If the `pptx` skill is installed and enabled, load_skill("pptx") returns
it directly. If loading `pptx` returns this stub again, `pptx` is not
installed or is disabled — tell the user, and continue with generic guidance
for this file type instead of retrying.
