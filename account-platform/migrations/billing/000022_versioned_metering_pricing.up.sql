CREATE TABLE metering_unit_definitions (
    id UUID PRIMARY KEY,
    unit_code TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    minimum_precision BIGINT NOT NULL CHECK (minimum_precision > 0),
    price_denominator BIGINT NOT NULL CHECK (price_denominator > 0),
    rounding_rule TEXT NOT NULL CHECK (rounding_rule IN ('CEILING','FLOOR','HALF_UP')),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE','INACTIVE')),
    created_at TIMESTAMPTZ NOT NULL,
    deactivated_at TIMESTAMPTZ,
    UNIQUE (unit_code,version),
    CHECK ((status='ACTIVE')=(deactivated_at IS NULL))
);

INSERT INTO metering_unit_definitions(id,unit_code,version,minimum_precision,price_denominator,rounding_rule,status,created_at) VALUES
('00000000-0000-7000-8100-000000000001','INPUT_TOKEN',1,1,1000000,'CEILING','ACTIVE',now()),
('00000000-0000-7000-8100-000000000002','CACHED_INPUT_TOKEN',1,1,1000000,'CEILING','ACTIVE',now()),
('00000000-0000-7000-8100-000000000003','OUTPUT_TOKEN',1,1,1000000,'CEILING','ACTIVE',now()),
('00000000-0000-7000-8100-000000000004','IMAGE',1,1,1,'CEILING','ACTIVE',now()),
('00000000-0000-7000-8100-000000000005','REQUEST',1,1,1,'CEILING','ACTIVE',now()),
('00000000-0000-7000-8100-000000000006','DOCUMENT',1,1,1,'CEILING','ACTIVE',now());

CREATE TABLE billing_protocol_formula_bindings (
    formula_version_id UUID PRIMARY KEY,
    protocol_code TEXT NOT NULL,
    required_unit_definition_ids UUID[] NOT NULL,
    formula_snapshot JSONB NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    CHECK (cardinality(required_unit_definition_ids) > 0)
);

INSERT INTO billing_protocol_formula_bindings(formula_version_id,protocol_code,required_unit_definition_ids,formula_snapshot,published_at) VALUES
('00000000-0000-7000-8200-000000000001','CHAT',ARRAY[
 '00000000-0000-7000-8100-000000000001'::uuid,
 '00000000-0000-7000-8100-000000000002'::uuid,
 '00000000-0000-7000-8100-000000000003'::uuid
],'{"evaluator_version":"SAFE_PARAMETER_MAP_V1","required_unit_versions":{"INPUT_TOKEN":"00000000-0000-7000-8100-000000000001","CACHED_INPUT_TOKEN":"00000000-0000-7000-8100-000000000002","OUTPUT_TOKEN":"00000000-0000-7000-8100-000000000003"},"expression":{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens","OUTPUT_TOKEN":"max_output_tokens"}}',now());

DROP INDEX pricing_versions_active_model_idx;

ALTER TABLE pricing_versions
    ADD COLUMN scope_type TEXT NOT NULL DEFAULT 'MODEL' CHECK (scope_type IN ('GLOBAL','MODEL')),
    ADD COLUMN scope_id TEXT,
    ADD COLUMN protocol_code TEXT NOT NULL DEFAULT 'CHAT',
    ADD COLUMN formula_version_id UUID NOT NULL DEFAULT '00000000-0000-7000-8200-000000000001',
    ADD COLUMN credit_unit TEXT NOT NULL DEFAULT 'MICROCREDIT',
    ADD COLUMN lifecycle_status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (lifecycle_status IN ('DRAFT','PUBLISHED','ACTIVE','ENDED','CANCELLED')),
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN space_etag BIGINT NOT NULL DEFAULT 1 CHECK (space_etag > 0),
    ADD COLUMN published_at TIMESTAMPTZ,
    ADD COLUMN cancelled_at TIMESTAMPTZ,
    ADD COLUMN ended_at TIMESTAMPTZ,
    ADD COLUMN created_by UUID,
    ADD COLUMN request_fingerprint TEXT,
    ADD COLUMN publish_idempotency_key TEXT,
    ADD COLUMN lifecycle_reason TEXT;

UPDATE pricing_versions SET scope_id=model,published_at=created_at;

ALTER TABLE pricing_versions
    ADD CONSTRAINT pricing_versions_scope_check CHECK ((scope_type='GLOBAL' AND scope_id IS NULL) OR (scope_type='MODEL' AND scope_id IS NOT NULL)),
    ADD CONSTRAINT pricing_versions_lifecycle_check CHECK (
        (lifecycle_status='DRAFT' AND published_at IS NULL)
        OR (lifecycle_status IN ('PUBLISHED','ACTIVE','ENDED','CANCELLED') AND published_at IS NOT NULL)
    );

CREATE TABLE pricing_version_unit_configs (
    price_version_id UUID NOT NULL REFERENCES pricing_versions(id),
    unit_definition_id UUID NOT NULL REFERENCES metering_unit_definitions(id),
    config_type TEXT NOT NULL CHECK (config_type IN ('FIXED_PRICE','INHERIT_GLOBAL')),
    fixed_price_per_denominator BIGINT CHECK (fixed_price_per_denominator >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (price_version_id,unit_definition_id),
    CHECK ((config_type='FIXED_PRICE')=(fixed_price_per_denominator IS NOT NULL))
);

INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at)
SELECT price.id,unit.id,'FIXED_PRICE',
       CASE unit.unit_code WHEN 'INPUT_TOKEN' THEN price.input_per_million WHEN 'CACHED_INPUT_TOKEN' THEN price.cached_input_per_million ELSE price.output_per_million END,
       price.created_at
FROM pricing_versions price
JOIN metering_unit_definitions unit ON unit.unit_code IN ('INPUT_TOKEN','CACHED_INPUT_TOKEN','OUTPUT_TOKEN') AND unit.version=1;

CREATE TABLE pricing_space_versions (
    scope_type TEXT NOT NULL CHECK (scope_type IN ('GLOBAL','MODEL')),
    scope_key TEXT NOT NULL,
    protocol_code TEXT NOT NULL,
    formula_version_id UUID NOT NULL,
    credit_unit TEXT NOT NULL,
    etag BIGINT NOT NULL CHECK (etag > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope_type,scope_key,protocol_code,formula_version_id,credit_unit)
);

INSERT INTO pricing_space_versions(scope_type,scope_key,protocol_code,formula_version_id,credit_unit,etag,updated_at)
SELECT 'MODEL',model,'CHAT','00000000-0000-7000-8200-000000000001','MICROCREDIT',1,max(created_at)
FROM pricing_versions GROUP BY model;

CREATE TABLE pricing_publish_commands (
    actor_user_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    request_fingerprint TEXT NOT NULL CHECK (char_length(request_fingerprint)=64),
    price_version_id UUID NOT NULL REFERENCES pricing_versions(id),
    response_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_user_id,idempotency_key)
);

CREATE TABLE pricing_lifecycle_commands (
    actor_user_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
    request_fingerprint TEXT NOT NULL CHECK (char_length(request_fingerprint)=64),
    price_version_id UUID NOT NULL REFERENCES pricing_versions(id),
    response_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (actor_user_id,idempotency_key)
);

CREATE TABLE pricing_blocks (
    id UUID PRIMARY KEY,
    model_id TEXT NOT NULL,
    protocol_code TEXT NOT NULL,
    formula_version_id UUID NOT NULL,
    credit_unit TEXT NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL,
    effective_to TIMESTAMPTZ,
    reason TEXT NOT NULL,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE TABLE pricing_audits (
    id UUID PRIMARY KEY,
    price_version_id UUID REFERENCES pricing_versions(id),
    action TEXT NOT NULL,
    actor_user_id UUID NOT NULL,
    reason TEXT,
    before_snapshot JSONB,
    after_snapshot JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE usage_holds
    ADD COLUMN model_id TEXT,
    ADD COLUMN protocol_code TEXT,
    ADD COLUMN formula_version_id UUID,
    ADD COLUMN pricing_snapshot JSONB,
    ADD COLUMN request_snapshot JSONB;

ALTER TABLE usage_holds DROP CONSTRAINT usage_holds_max_microcredits_check;
ALTER TABLE usage_holds ADD CONSTRAINT usage_holds_max_microcredits_check CHECK (max_microcredits >= 0);

CREATE INDEX pricing_versions_match_idx ON pricing_versions(scope_type,scope_id,protocol_code,formula_version_id,credit_unit,effective_from,effective_to,lifecycle_status);
CREATE INDEX pricing_blocks_match_idx ON pricing_blocks(model_id,protocol_code,formula_version_id,credit_unit,effective_from,effective_to);

CREATE OR REPLACE FUNCTION reject_immutable_metering_unit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'metering unit definitions cannot be deleted';
    END IF;
    IF OLD.status = 'ACTIVE' AND NEW.status = 'INACTIVE'
       AND NEW.deactivated_at IS NOT NULL
       AND NEW.id = OLD.id AND NEW.unit_code = OLD.unit_code AND NEW.version = OLD.version
       AND NEW.minimum_precision = OLD.minimum_precision
       AND NEW.price_denominator = OLD.price_denominator
       AND NEW.rounding_rule = OLD.rounding_rule
       AND NEW.created_at = OLD.created_at THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'metering unit definition facts are immutable';
END $$;

CREATE TRIGGER metering_unit_immutable_update BEFORE UPDATE OR DELETE ON metering_unit_definitions
FOR EACH ROW EXECUTE FUNCTION reject_immutable_metering_unit();

CREATE OR REPLACE FUNCTION reject_published_price_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.lifecycle_status <> 'DRAFT' AND (
        NEW.scope_type IS DISTINCT FROM OLD.scope_type OR NEW.scope_id IS DISTINCT FROM OLD.scope_id
        OR NEW.protocol_code IS DISTINCT FROM OLD.protocol_code OR NEW.formula_version_id IS DISTINCT FROM OLD.formula_version_id
        OR NEW.credit_unit IS DISTINCT FROM OLD.credit_unit OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.input_per_million IS DISTINCT FROM OLD.input_per_million OR NEW.output_per_million IS DISTINCT FROM OLD.output_per_million
        OR NEW.cached_input_per_million IS DISTINCT FROM OLD.cached_input_per_million OR NEW.created_at IS DISTINCT FROM OLD.created_at
    ) THEN
        RAISE EXCEPTION 'published price facts are immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER pricing_version_published_immutable BEFORE UPDATE ON pricing_versions
FOR EACH ROW EXECUTE FUNCTION reject_published_price_mutation();

CREATE OR REPLACE FUNCTION reject_published_price_unit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT lifecycle_status INTO parent_status
    FROM pricing_versions
    WHERE id = COALESCE(OLD.price_version_id,NEW.price_version_id);
    IF parent_status <> 'DRAFT' THEN
        RAISE EXCEPTION 'published price unit configuration is immutable';
    END IF;
    RETURN COALESCE(NEW,OLD);
END $$;

CREATE TRIGGER pricing_version_unit_published_immutable BEFORE INSERT OR UPDATE OR DELETE ON pricing_version_unit_configs
FOR EACH ROW EXECUTE FUNCTION reject_published_price_unit_mutation();

CREATE OR REPLACE FUNCTION reject_formula_binding_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'published formula bindings are immutable';
END $$;
CREATE TRIGGER formula_binding_immutable BEFORE UPDATE OR DELETE ON billing_protocol_formula_bindings
FOR EACH ROW EXECUTE FUNCTION reject_formula_binding_mutation();

CREATE TABLE pricing_operation_commands (
 actor_user_id UUID NOT NULL,
 idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 16 AND 128),
 operation TEXT NOT NULL CHECK (operation IN ('UNIT_CREATE','PRICE_DRAFT_CREATE','PRICE_DRAFT_UPDATE')),
 request_fingerprint TEXT NOT NULL CHECK (char_length(request_fingerprint)=64),
 response_snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(actor_user_id,idempotency_key)
);
CREATE OR REPLACE FUNCTION reject_pricing_operation_command_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'pricing operation receipts are immutable';
END $$;
CREATE TRIGGER pricing_operation_commands_immutable BEFORE UPDATE OR DELETE ON pricing_operation_commands
FOR EACH ROW EXECUTE FUNCTION reject_pricing_operation_command_mutation();
