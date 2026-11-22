-- A logical admin repair command is independent of the immutable per-payment
-- audit. No historical audit is assigned a guessed request key.
CREATE TABLE payment_order_repair_commands (
 id UUID PRIMARY KEY,
 payment_order_id UUID NOT NULL REFERENCES payment_orders(id),
 actor_id UUID NOT NULL,
 idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 16 AND 128),
 command_protocol INTEGER NOT NULL CHECK (command_protocol=1),
 reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 500),
 request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
 response_body JSONB NOT NULL CHECK (jsonb_typeof(response_body)='object'),
 created_at TIMESTAMPTZ NOT NULL,
 CONSTRAINT payment_order_repair_commands_actor_key UNIQUE(actor_id,idempotency_key)
);
CREATE TRIGGER payment_order_repair_commands_immutable
BEFORE UPDATE OR DELETE ON payment_order_repair_commands
FOR EACH ROW EXECUTE FUNCTION reject_payment_evidence_mutation();
