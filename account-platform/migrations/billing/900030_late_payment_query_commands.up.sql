-- Persist dispatch admission before Provider I/O. No lease, expiry or replay
-- grants another dispatch token. Completed evidence and receipt are atomic.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=session_user) THEN
  RAISE EXCEPTION 'query commands require the deployment migrator';
 END IF;
END $$;
SET ROLE billing_schema_owner;
CREATE TABLE late_payment_query_commands (
    idempotency_key TEXT PRIMARY KEY CHECK(idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,119}$'),
    case_id UUID NOT NULL REFERENCES late_payment_review_cases(id),
    actor_id UUID NOT NULL,
    payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
    request_hash TEXT NOT NULL CHECK(request_hash ~ '^[0-9a-f]{64}$'),
    request_snapshot JSONB NOT NULL CHECK(jsonb_typeof(request_snapshot)='object'),
    started_at TIMESTAMPTZ NOT NULL,
    result_snapshot JSONB CHECK(jsonb_typeof(result_snapshot)='object'),
    completed_at TIMESTAMPTZ,
    CHECK((result_snapshot IS NULL) = (completed_at IS NULL))
);
CREATE INDEX late_payment_query_commands_case_idx ON late_payment_query_commands(case_id,started_at,idempotency_key);
CREATE FUNCTION guard_late_payment_query_command() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP <> 'UPDATE' THEN RAISE EXCEPTION 'query command identity and receipt are immutable'; END IF;
 IF OLD.completed_at IS NOT NULL OR NEW.completed_at IS NULL OR NEW.result_snapshot IS NULL
  OR ROW(NEW.idempotency_key,NEW.case_id,NEW.actor_id,NEW.payment_order_id,NEW.request_hash,NEW.request_snapshot,NEW.started_at)
   IS DISTINCT FROM ROW(OLD.idempotency_key,OLD.case_id,OLD.actor_id,OLD.payment_order_id,OLD.request_hash,OLD.request_snapshot,OLD.started_at) THEN
  RAISE EXCEPTION 'query command identity and receipt are immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER late_payment_query_command_immutable BEFORE UPDATE OR DELETE ON late_payment_query_commands
 FOR EACH ROW EXECUTE FUNCTION guard_late_payment_query_command();
CREATE TRIGGER late_payment_query_command_no_truncate BEFORE TRUNCATE ON late_payment_query_commands
 FOR EACH STATEMENT EXECUTE FUNCTION guard_late_payment_query_command();
REVOKE ALL ON late_payment_query_commands FROM PUBLIC,billing_service;
GRANT SELECT,INSERT ON late_payment_query_commands TO billing_service;
GRANT UPDATE(result_snapshot,completed_at) ON late_payment_query_commands TO billing_service;
REVOKE ALL ON FUNCTION guard_late_payment_query_command() FROM PUBLIC;
SET ROLE billing_service;
