# Frozen measured evaluator copies

These files are immutable historical inputs for the V2.4.5 measurement and its
post-run repair controls. Their defects and measured hashes are preserved.
Current implementations live under `scripts/`; R5 fixes presentation pairing and
numeric matching and records a grader revision on new rows. Do not use the frozen
copies for new runs or edit their bytes to import current fixes. See
[verification](../review-r5-verification.json) for the exact boundary.

> 本仓差异（pinvou3）: `scripts/` above refers to the community repository
> [Pinvou/pinvou-agent](https://github.com/Pinvou/pinvou-agent); this flattened
> copy omits the sibling modules the live layout provides, so
> `test_prompt_eval_repair.py` cannot run in place — it passes (9/9) only after
> the frozen files are overlaid onto the upstream `scripts/` + `scripts/tests/`
> layout. Do not treat an in-place `ModuleNotFoundError` as a defect in the
> frozen logic.
