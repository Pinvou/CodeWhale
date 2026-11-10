# Release evidence artifact identity and reads

An artifact signature binds its path, kind, SHA-256, producer role, source commit
and run nonce. Artifact paths are unique. References carrying a path must match
that exact signed entry, digest, expected kind and orchestrator producer (the
Windows orchestrator copies the installer, driver, Node and legacy binary and
collects build provenance).

A reference carrying only a digest must resolve to exactly one artifact entry
across the entire manifest. Multiple matching entries are rejected with
`evidence_artifact_digest_ambiguous`, regardless of order, kind or producer. This
applies to journeys, gates, provider attestations and nested UI, flag-off and
legacy evidence. Check/trace/gate reuse remains forbidden by the existing claim
rules. A/B may reference the same installer entry, or two explicitly addressed
orchestrator-signed installer paths containing identical bytes; installer
identity is resolved by path, rather than an arbitrary digest match.

Every artifact read, including subsequent JSON parsing, rechecks path containment,
symlinks, regular-file identity and digest. Metadata size is checked before open
and again on the opened file: text is limited to 32 MiB and binary artifacts to
1 GiB, with empty files rejected. Reads use at most 64 KiB per operation and a
one-byte EOF probe; binary hashing does not retain the complete file in memory.
Text accumulation is bounded by the admitted size. Growth, truncation, identity
replacement or observed metadata changes fail validation; no earlier successful
hash authorizes a later unverified read. This is validation of supplied evidence,
not proof that a physical release run or signing ceremony took place.

The Windows orchestrator includes the artifact reader in its source snapshot and
validator dependency hash map, so later validator invocations reject reader
changes during a run. The release-contract CI suite runs through
`scripts/ci-fixture-process.py` with a per-run receipt and preserves its failure
exit code; local test cleanup alone does not establish CI resource retirement.
