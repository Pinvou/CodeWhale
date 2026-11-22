-- Missing protocol is intentionally not inferred or backfilled from mutable
-- display names, provider names or opaque upstream aliases. The governed draft
-- publisher creates a new immutable capability when an operator supplies it.
ALTER TABLE model_capability_versions
    ADD COLUMN runtime_protocol TEXT NOT NULL DEFAULT ''
    CHECK (runtime_protocol IN ('', 'OPENAI_CHAT_V1', 'DEEPSEEK_V4_CHAT_V1'));

ALTER TABLE model_catalog_drafts
    ADD COLUMN runtime_protocol TEXT NOT NULL DEFAULT ''
    CHECK (runtime_protocol IN ('', 'OPENAI_CHAT_V1', 'DEEPSEEK_V4_CHAT_V1'));
