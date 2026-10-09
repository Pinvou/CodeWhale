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
