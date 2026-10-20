---
name: xlsx
description: Create/edit/analyze/verify spreadsheet workbooks including XLSX/CSV/TSV.
invocation: model+user
---

# Xlsx

## When to use
Use for spreadsheets, tabular analysis, formulas, and charts in workbooks.

## Compatibility
The legacy name `spreadsheets` resolves to a stub that redirects here; loading `xlsx` directly is equivalent.

## Workflow
1. Prefer structured tables with clear headers.
2. Keep formulas auditable.
3. Verify by reopening and checking key cells/aggregates. Script-written workbooks carry no cached formula values — formula cells may read empty until a spreadsheet app recalculates; verify literal cells and audit formulas by inspection.
