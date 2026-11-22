CREATE TABLE payment_stop_loss_policies (
    policy_version TEXT PRIMARY KEY,
    policy_snapshot JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO payment_stop_loss_policies(policy_version,policy_snapshot) VALUES(
    'payment-stop-loss/v1',
    '{"consecutive":{"window_seconds":300,"failures":5,"distinct_orders":3},"failure_rate":{"window_seconds":60,"minimum_samples":20,"minimum_failures":10,"distinct_orders":5},"cross_product":{"window_seconds":900,"distinct_products":2,"distinct_orders":3,"distinct_subjects":2}}'::jsonb
);

CREATE TABLE payment_provider_attempts (
    id UUID PRIMARY KEY,
    provider_attempt_id TEXT NOT NULL UNIQUE,
    request_fingerprint TEXT NOT NULL CHECK (request_fingerprint ~ '^[0-9a-f]{64}$'),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    capability TEXT NOT NULL,
    channel TEXT NOT NULL,
    route TEXT NOT NULL,
    logical_order_id UUID REFERENCES payment_orders(id),
    product_id TEXT,
    subject_hash TEXT CHECK (subject_hash IS NULL OR subject_hash ~ '^[0-9a-f]{64}$'),
    trusted_probe BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL DEFAULT 'INTENT' CHECK (status IN ('INTENT','RESULT_RECORDED')),
    created_at TIMESTAMPTZ NOT NULL,
    result_recorded_at TIMESTAMPTZ
);
CREATE INDEX payment_provider_attempts_open_idx ON payment_provider_attempts(provider_id,provider_version,capability,channel,route,created_at,id) WHERE status='INTENT';

CREATE TABLE payment_provider_health_observations (
    id UUID PRIMARY KEY,
    provider_attempt_id TEXT NOT NULL UNIQUE REFERENCES payment_provider_attempts(provider_attempt_id),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    capability TEXT NOT NULL,
    channel TEXT NOT NULL,
    route TEXT NOT NULL,
    logical_order_id UUID REFERENCES payment_orders(id),
    product_id TEXT,
    subject_hash TEXT CHECK (subject_hash IS NULL OR subject_hash ~ '^[0-9a-f]{64}$'),
    trusted_probe BOOLEAN NOT NULL DEFAULT FALSE,
    outcome TEXT NOT NULL CHECK (outcome IN ('SUCCESS','INFRASTRUCTURE_FAILURE','UNCERTAIN_RESULT','BUSINESS_REJECTION')),
    classification_rule_version TEXT NOT NULL,
    evidence_digest TEXT NOT NULL CHECK (evidence_digest ~ '^[0-9a-f]{64}$'),
    received_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX payment_provider_health_window_idx ON payment_provider_health_observations(provider_id,provider_version,capability,channel,route,received_at,id);

CREATE VIEW payment_observation_gaps AS
SELECT provider_attempt_id,provider_id,provider_version,capability,channel,route,logical_order_id,product_id,created_at
  FROM payment_provider_attempts
 WHERE status='INTENT' AND created_at<=now()-interval '1 minute';

CREATE TABLE payment_stop_loss_evaluations (
    id UUID PRIMARY KEY,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('PRODUCT','PROVIDER')),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    product_id TEXT,
    trigger_type TEXT NOT NULL,
    policy_version TEXT NOT NULL REFERENCES payment_stop_loss_policies(policy_version),
    control_version_before BIGINT NOT NULL CHECK (control_version_before>=0),
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    observation_high_watermark UUID,
    metrics JSONB NOT NULL,
    dedupe_key TEXT NOT NULL UNIQUE,
    action_applied BOOLEAN NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL,
    CHECK (window_start<=window_end),
    CHECK ((scope_type='PRODUCT')=(product_id IS NOT NULL))
);

CREATE TABLE payment_stop_loss_evaluation_members (
    id UUID PRIMARY KEY,
    evaluation_id UUID NOT NULL REFERENCES payment_stop_loss_evaluations(id),
    observation_id UUID REFERENCES payment_provider_health_observations(id),
    case_id UUID REFERENCES manual_review_cases(id),
    CHECK ((observation_id IS NOT NULL)::integer + (case_id IS NOT NULL)::integer = 1)
);
CREATE UNIQUE INDEX payment_stop_loss_observation_member_idx ON payment_stop_loss_evaluation_members(evaluation_id,observation_id) WHERE observation_id IS NOT NULL;
CREATE UNIQUE INDEX payment_stop_loss_case_member_idx ON payment_stop_loss_evaluation_members(evaluation_id,case_id) WHERE case_id IS NOT NULL;

CREATE TABLE payment_legacy_stop_loss_reviews (
    id UUID PRIMARY KEY,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('PRODUCT','PROVIDER')),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    product_id TEXT,
    control_version BIGINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    observation_high_watermark UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    CHECK ((scope_type='PRODUCT')=(product_id IS NOT NULL))
);
CREATE UNIQUE INDEX payment_legacy_stop_loss_reviews_scope_idx ON payment_legacy_stop_loss_reviews(scope_type,provider_id,provider_version,COALESCE(product_id,''));

ALTER TABLE payment_provider_controls ADD COLUMN reason_code TEXT NOT NULL DEFAULT 'AVAILABLE' CHECK (reason_code IN ('AVAILABLE','AUTOMATIC_STOP_LOSS','MANUAL_SAFETY_PAUSE','PROVIDER_BLIND','LEGACY_UNCLASSIFIED'));

UPDATE payment_provider_controls SET reason_code='LEGACY_UNCLASSIFIED' WHERE NOT new_orders_enabled;

INSERT INTO payment_legacy_stop_loss_reviews(id,scope_type,provider_id,provider_version,control_version,observation_high_watermark)
SELECT gen_random_uuid(),'PROVIDER',control.provider_id,control.provider_version,control.version,
       (SELECT observation.id FROM payment_provider_health_observations observation ORDER BY observation.received_at DESC,observation.id DESC LIMIT 1)
  FROM payment_provider_controls control WHERE NOT control.new_orders_enabled
ON CONFLICT DO NOTHING;

INSERT INTO payment_legacy_stop_loss_reviews(id,scope_type,provider_id,provider_version,product_id,control_version,observation_high_watermark)
SELECT gen_random_uuid(),'PRODUCT',control.provider_id,control.provider_version,control.product_id,control.version,
       (SELECT observation.id FROM payment_provider_health_observations observation ORDER BY observation.received_at DESC,observation.id DESC LIMIT 1)
  FROM payment_product_controls control WHERE NOT control.new_orders_enabled
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION reject_payment_stop_loss_fact_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment stop-loss policies, observations and evaluations are immutable';
END $$;
CREATE TRIGGER payment_stop_loss_policies_immutable BEFORE UPDATE OR DELETE ON payment_stop_loss_policies FOR EACH ROW EXECUTE FUNCTION reject_payment_stop_loss_fact_mutation();
CREATE TRIGGER payment_provider_health_observations_immutable BEFORE UPDATE OR DELETE ON payment_provider_health_observations FOR EACH ROW EXECUTE FUNCTION reject_payment_stop_loss_fact_mutation();
CREATE TRIGGER payment_stop_loss_evaluations_immutable BEFORE UPDATE OR DELETE ON payment_stop_loss_evaluations FOR EACH ROW EXECUTE FUNCTION reject_payment_stop_loss_fact_mutation();
CREATE TRIGGER payment_stop_loss_evaluation_members_immutable BEFORE UPDATE OR DELETE ON payment_stop_loss_evaluation_members FOR EACH ROW EXECUTE FUNCTION reject_payment_stop_loss_fact_mutation();
