---
name: spreadsheets
description: Compatibility stub owning the legacy name xlsx; the xlsx skill provides the workflow.
aliases-for: xlsx
---

# spreadsheets (compatibility stub)

This stub exists so the legacy name `xlsx` keeps resolving. It contains no
workflow of its own.

If the `xlsx` skill is installed and enabled, load_skill("xlsx") returns
it directly. If loading `xlsx` returns this stub again, `xlsx` is not
installed or is disabled — tell the user, and continue with generic guidance
for this file type instead of retrying.
