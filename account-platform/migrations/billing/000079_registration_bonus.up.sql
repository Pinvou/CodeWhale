SET ROLE billing_service;

CREATE TABLE registration_bonus_policy_versions (
    revision BIGINT PRIMARY KEY CHECK (revision > 0),
    enabled BOOLEAN NOT NULL,
    amount_microcredits BIGINT NOT NULL CHECK (amount_microcredits >= 0 AND amount_microcredits % 10000 = 0),
    validity_days INTEGER NOT NULL CHECK (validity_days BETWEEN 1 AND 3650),
    actor_user_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 10 AND 500),
    created_at TIMESTAMPTZ NOT NULL
);

INSERT INTO registration_bonus_policy_versions(revision,enabled,amount_microcredits,validity_days,actor_user_id,reason,created_at)
VALUES(1,FALSE,0,30,'00000000-0000-0000-0000-000000000000','System bootstrap policy: disabled by default.',transaction_timestamp());

CREATE TABLE registration_bonus_policy_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    active_revision BIGINT NOT NULL REFERENCES registration_bonus_policy_versions(revision),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (active_revision = revision)
);
INSERT INTO registration_bonus_policy_state(singleton,revision,active_revision) VALUES(TRUE,1,1);

CREATE TABLE registration_bonus_policy_commands (
    actor_user_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 16 AND 128),
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    resulting_revision BIGINT NOT NULL REFERENCES registration_bonus_policy_versions(revision),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(actor_user_id,idempotency_key)
);

CREATE TABLE registration_bonus_policy_audit (
    id UUID PRIMARY KEY,
    actor_user_id UUID NOT NULL,
    action TEXT NOT NULL CHECK (action = 'POLICY_UPDATED'),
    reason TEXT NOT NULL,
    before_snapshot JSONB NOT NULL,
    after_snapshot JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE registration_bonus_claims (
    id UUID PRIMARY KEY,
    fingerprint_key_id TEXT NOT NULL CHECK (length(fingerprint_key_id) BETWEEN 1 AND 100),
    mobile_fingerprint BYTEA NOT NULL UNIQUE CHECK (octet_length(mobile_fingerprint) = 32),
    first_registration_operation_id UUID NOT NULL UNIQUE,
    first_user_id UUID NOT NULL,
    policy_revision BIGINT NOT NULL REFERENCES registration_bonus_policy_versions(revision),
    outcome TEXT NOT NULL CHECK (outcome IN ('GRANTED','NOT_GRANTED_DISABLED','NOT_GRANTED_ZERO')),
    granted_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (granted_microcredits >= 0),
    entitlement_package_id UUID REFERENCES entitlement_packages(id) DEFERRABLE INITIALLY DEFERRED,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK ((outcome = 'GRANTED') = (granted_microcredits > 0 AND entitlement_package_id IS NOT NULL))
);

CREATE TABLE registration_bonus_receipts (
    registration_operation_id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    claim_id UUID NOT NULL REFERENCES registration_bonus_claims(id),
    policy_revision BIGINT NOT NULL REFERENCES registration_bonus_policy_versions(revision),
    outcome TEXT NOT NULL CHECK (outcome IN ('GRANTED','NOT_GRANTED_DISABLED','NOT_GRANTED_ZERO','ALREADY_CLAIMED')),
    granted_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (granted_microcredits >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    CHECK ((outcome = 'GRANTED') = (granted_microcredits > 0))
);

CREATE TRIGGER registration_bonus_policy_versions_immutable BEFORE UPDATE OR DELETE ON registration_bonus_policy_versions
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_policy_commands_immutable BEFORE UPDATE OR DELETE ON registration_bonus_policy_commands
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_policy_audit_immutable BEFORE UPDATE OR DELETE ON registration_bonus_policy_audit
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_claims_immutable BEFORE UPDATE OR DELETE ON registration_bonus_claims
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_receipts_immutable BEFORE UPDATE OR DELETE ON registration_bonus_receipts
FOR EACH ROW EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_policy_versions_no_truncate BEFORE TRUNCATE ON registration_bonus_policy_versions
FOR EACH STATEMENT EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_policy_commands_no_truncate BEFORE TRUNCATE ON registration_bonus_policy_commands
FOR EACH STATEMENT EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_policy_audit_no_truncate BEFORE TRUNCATE ON registration_bonus_policy_audit
FOR EACH STATEMENT EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_claims_no_truncate BEFORE TRUNCATE ON registration_bonus_claims
FOR EACH STATEMENT EXECUTE FUNCTION prevent_ledger_mutation();
CREATE TRIGGER registration_bonus_receipts_no_truncate BEFORE TRUNCATE ON registration_bonus_receipts
FOR EACH STATEMENT EXECUTE FUNCTION prevent_ledger_mutation();

COMMENT ON TABLE registration_bonus_claims IS
    'Lifetime mobile eligibility facts. Claims are retained after account closure and are inserted even when the active policy is disabled or zero.';
