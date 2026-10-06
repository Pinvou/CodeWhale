CREATE TABLE protocol_definitions (
    id UUID PRIMARY KEY,
    protocol_code TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    status TEXT NOT NULL CHECK (status IN ('DRAFT','PUBLISHED','DEPRECATED')),
    request_schema JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    UNIQUE (protocol_code,version)
);

CREATE TABLE protocol_formula_versions (
    id UUID PRIMARY KEY,
    protocol_definition_id UUID NOT NULL REFERENCES protocol_definitions(id),
    version BIGINT NOT NULL CHECK (version > 0),
    status TEXT NOT NULL CHECK (status IN ('DRAFT','PUBLISHED','DEPRECATED')),
    required_unit_versions JSONB NOT NULL,
    expression JSONB NOT NULL,
    evaluator_version TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    UNIQUE (protocol_definition_id,version)
);

CREATE TABLE token_governance_commands (
    actor_id UUID NOT NULL,
    operation_kind TEXT NOT NULL CHECK (operation_kind IN ('PROTOCOL_CREATE','FORMULA_CREATE','PROTOCOL_PUBLISH','FORMULA_PUBLISH')),
    logical_id UUID NOT NULL,
    request_snapshot JSONB NOT NULL,
    response_snapshot JSONB,
    started_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_id, operation_kind, logical_id)
);

CREATE TABLE token_formula_publication_intents (
    formula_version_id UUID PRIMARY KEY REFERENCES protocol_formula_versions(id),
    formula_snapshot JSONB NOT NULL,
    started_at TIMESTAMPTZ NOT NULL
);

INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at,published_at) VALUES
('00000000-0000-7000-8200-000000000000','CHAT',1,'PUBLISHED','{"parameters":["estimated_input_tokens","cached_input_tokens","max_output_tokens","image_count","request_count"]}',now(),now());

INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at,published_at) VALUES
('00000000-0000-7000-8200-000000000001','00000000-0000-7000-8200-000000000000',1,'PUBLISHED',
 '{"INPUT_TOKEN":"00000000-0000-7000-8100-000000000001","CACHED_INPUT_TOKEN":"00000000-0000-7000-8100-000000000002","OUTPUT_TOKEN":"00000000-0000-7000-8100-000000000003"}',
 '{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens","OUTPUT_TOKEN":"max_output_tokens"}',
 'SAFE_PARAMETER_MAP_V1',now(),now());

CREATE OR REPLACE FUNCTION reject_published_protocol_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'DRAFT' THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'PUBLISHED' AND NEW.status = 'DEPRECATED'
       AND (to_jsonb(NEW) - 'status') = (to_jsonb(OLD) - 'status') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'published protocol facts are immutable';
END $$;

CREATE TRIGGER protocol_definition_immutable BEFORE UPDATE OR DELETE ON protocol_definitions FOR EACH ROW EXECUTE FUNCTION reject_published_protocol_mutation();
CREATE TRIGGER protocol_formula_immutable BEFORE UPDATE OR DELETE ON protocol_formula_versions FOR EACH ROW EXECUTE FUNCTION reject_published_protocol_mutation();

ALTER TABLE usage_requests DROP CONSTRAINT usage_requests_maximum_microcredits_check;
ALTER TABLE usage_requests ADD CONSTRAINT usage_requests_maximum_microcredits_check CHECK (maximum_microcredits >= 0);
