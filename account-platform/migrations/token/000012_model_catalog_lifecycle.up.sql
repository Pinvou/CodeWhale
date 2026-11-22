CREATE TABLE model_identities (
    id UUID PRIMARY KEY,
    provider_id TEXT NOT NULL,
    adapter_version TEXT NOT NULL,
    upstream_model_ref TEXT NOT NULL,
    upstream_model_ref_digest TEXT NOT NULL CHECK (upstream_model_ref_digest ~ '^[0-9a-f]{64}$'),
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(provider_id,upstream_model_ref_digest)
);

CREATE TABLE model_capability_versions (
    id UUID PRIMARY KEY,
    model_id UUID NOT NULL REFERENCES model_identities(id),
    version BIGINT NOT NULL CHECK (version > 0),
    protocol_definition_id UUID NOT NULL REFERENCES protocol_definitions(id),
    context_length BIGINT NOT NULL CHECK (context_length > 0),
    maximum_output_tokens BIGINT NOT NULL CHECK (maximum_output_tokens >= 0 AND maximum_output_tokens <= context_length),
    supported_modalities JSONB NOT NULL,
    tokenizer_version TEXT NOT NULL,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(model_id,version)
);

CREATE TABLE model_catalog_versions (
    version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id UUID NOT NULL UNIQUE,
    rollback_source_version BIGINT REFERENCES model_catalog_versions(version),
    reason TEXT NOT NULL,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE model_catalog_entries (
    catalog_version BIGINT NOT NULL REFERENCES model_catalog_versions(version),
    model_id UUID NOT NULL REFERENCES model_identities(id),
    display_name TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    aliases TEXT[] NOT NULL DEFAULT '{}',
    lifecycle_status TEXT NOT NULL CHECK (lifecycle_status IN ('PUBLISHED','DEPRECATED','REMOVED')),
    model_capability_version_id UUID NOT NULL REFERENCES model_capability_versions(id),
    protocol_definition_id UUID NOT NULL REFERENCES protocol_definitions(id),
    source_available BOOLEAN NOT NULL,
    source_missing_since TIMESTAMPTZ,
    source_missing_confirmations INTEGER NOT NULL DEFAULT 0 CHECK (source_missing_confirmations >= 0),
    deprecation_effective_at TIMESTAMPTZ,
    replacement_model_id UUID REFERENCES model_identities(id),
    new_session_cutoff_at TIMESTAMPTZ,
    continuation_cutoff_at TIMESTAMPTZ,
    force_cancel_inflight BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY(catalog_version,model_id),
    UNIQUE(catalog_version,normalized_name),
    CHECK ((lifecycle_status='PUBLISHED' AND deprecation_effective_at IS NULL) OR lifecycle_status<>'PUBLISHED'),
    CHECK (replacement_model_id IS NULL OR replacement_model_id<>model_id)
);

CREATE TABLE model_catalog_drafts (
    model_id UUID PRIMARY KEY REFERENCES model_identities(id),
    base_catalog_version BIGINT NOT NULL REFERENCES model_catalog_versions(version),
    draft_version BIGINT NOT NULL CHECK (draft_version > 0),
    display_name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    aliases TEXT[] NOT NULL DEFAULT '{}',
    protocol_code TEXT NOT NULL,
    protocol_version BIGINT NOT NULL CHECK (protocol_version > 0),
    context_length BIGINT NOT NULL CHECK (context_length > 0),
    maximum_output_tokens BIGINT NOT NULL CHECK (maximum_output_tokens >= 0 AND maximum_output_tokens <= context_length),
    supported_modalities JSONB NOT NULL,
    tokenizer_version TEXT NOT NULL,
    updated_by UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE model_catalog_commands (
    idempotency_key TEXT PRIMARY KEY,
    command_type TEXT NOT NULL,
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    result_catalog_version BIGINT REFERENCES model_catalog_versions(version),
    result_model_id UUID REFERENCES model_identities(id),
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE model_catalog_audit (
    id UUID PRIMARY KEY,
    action TEXT NOT NULL,
    model_id UUID REFERENCES model_identities(id),
    before_status TEXT,
    after_status TEXT,
    catalog_version BIGINT REFERENCES model_catalog_versions(version),
    actor_id UUID NOT NULL,
    reason TEXT NOT NULL,
    evidence JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX model_catalog_entries_status_idx ON model_catalog_entries(catalog_version,lifecycle_status,model_id);
CREATE INDEX model_catalog_audit_model_idx ON model_catalog_audit(model_id,created_at DESC);

CREATE OR REPLACE FUNCTION reject_model_catalog_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'published model catalog history is immutable';
END $$;

CREATE TRIGGER model_identity_immutable BEFORE UPDATE OR DELETE ON model_identities FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();
CREATE TRIGGER model_capability_immutable BEFORE UPDATE OR DELETE ON model_capability_versions FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();
CREATE TRIGGER model_catalog_version_immutable BEFORE UPDATE OR DELETE ON model_catalog_versions FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();
CREATE TRIGGER model_catalog_entry_immutable BEFORE UPDATE OR DELETE ON model_catalog_entries FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();
CREATE TRIGGER model_catalog_audit_immutable BEFORE UPDATE OR DELETE ON model_catalog_audit FOR EACH ROW EXECUTE FUNCTION reject_model_catalog_history_mutation();

ALTER TABLE routing_policies ADD COLUMN route_generation BIGINT NOT NULL DEFAULT 1 CHECK (route_generation > 0);
ALTER TABLE routing_policies ALTER COLUMN route_generation DROP DEFAULT;

ALTER TABLE usage_requests
    ADD COLUMN model_id UUID REFERENCES model_identities(id),
    ADD COLUMN catalog_version BIGINT REFERENCES model_catalog_versions(version),
    ADD COLUMN model_capability_version_id UUID REFERENCES model_capability_versions(id),
    ADD COLUMN protocol_definition_id UUID REFERENCES protocol_definitions(id),
    ADD COLUMN route_generation BIGINT CHECK (route_generation IS NULL OR route_generation > 0),
    ADD COLUMN model_snapshot JSONB;

ALTER TABLE usage_requests ADD CONSTRAINT usage_requests_model_version_snapshot_complete CHECK (
    (model_id IS NULL AND catalog_version IS NULL AND model_capability_version_id IS NULL AND protocol_definition_id IS NULL AND route_generation IS NULL AND model_snapshot IS NULL)
    OR
    (model_id IS NOT NULL AND catalog_version IS NOT NULL AND model_capability_version_id IS NOT NULL AND protocol_definition_id IS NOT NULL AND route_generation IS NOT NULL AND model_snapshot IS NOT NULL)
);
