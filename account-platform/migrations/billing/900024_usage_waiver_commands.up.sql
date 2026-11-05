-- New modern commands do not rewrite original026 or legacy administrator facts.
CREATE TABLE usage_waiver_commands (
 command_reference UUID PRIMARY KEY,
 hold_id UUID NOT NULL REFERENCES usage_holds(id),
 usage_request_id UUID NOT NULL,
 wallet_id UUID NOT NULL REFERENCES wallets(id),
 maximum_microcredits BIGINT NOT NULL CHECK(maximum_microcredits>=0),
 actor_user_id UUID NOT NULL,
 actor_issuer TEXT NOT NULL CHECK(actor_issuer<>''),
 actor_subject TEXT NOT NULL CHECK(actor_subject<>''),
 authorization_version BIGINT NOT NULL CHECK(authorization_version>0),
 idempotency_key_digest CHAR(64) NOT NULL UNIQUE CHECK(idempotency_key_digest ~ '^[0-9a-f]{64}$'),
 request_fingerprint CHAR(64) NOT NULL CHECK(request_fingerprint ~ '^[0-9a-f]{64}$'),
 command_snapshot JSONB NOT NULL,
 before_status TEXT NOT NULL CHECK(before_status IN ('ACTIVE','REVIEW')),
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX usage_waiver_commands_hold_idx ON usage_waiver_commands(hold_id,created_at,command_reference);
CREATE TABLE usage_waiver_results (
 command_reference UUID PRIMARY KEY REFERENCES usage_waiver_commands(command_reference),
 outcome TEXT NOT NULL CHECK(outcome IN ('WAIVED','COMMITTED','RELEASED_OTHER')),
 result_snapshot JSONB NOT NULL,
 allocation_snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE TRIGGER usage_waiver_commands_immutable BEFORE UPDATE OR DELETE ON usage_waiver_commands
 FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();
CREATE TRIGGER usage_waiver_results_immutable BEFORE UPDATE OR DELETE ON usage_waiver_results
 FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();

-- Pending is a coordination fence, never evidence that a waiver succeeded.
-- Only the original persisted Token rejection can retire this exact claim.
CREATE TABLE usage_waiver_hold_claims (
 hold_id UUID PRIMARY KEY REFERENCES usage_holds(id),
 command_reference UUID NOT NULL UNIQUE REFERENCES usage_waiver_commands(command_reference),
 created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE usage_waiver_rejections (
 command_reference UUID PRIMARY KEY REFERENCES usage_waiver_commands(command_reference),
 decision_snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
CREATE TRIGGER usage_waiver_rejections_immutable BEFORE UPDATE OR DELETE ON usage_waiver_rejections
 FOR EACH ROW EXECUTE FUNCTION reject_usage_hold_evidence_mutation();
