-- Forward-only parent identity; no existing grants or migration identities are rewritten.
CREATE TABLE member_quota_batch_commands (
 organization_id UUID NOT NULL,
 idempotency_key TEXT NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 16 AND 128),
 actor_user_id UUID NOT NULL,
 request_fingerprint TEXT NOT NULL CHECK (char_length(request_fingerprint)=64),
 request_snapshot JSONB NOT NULL CHECK (jsonb_typeof(request_snapshot)='object'),
 created_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(organization_id,idempotency_key)
);
CREATE FUNCTION reject_member_quota_batch_command_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'member quota batch facts are immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER member_quota_batch_commands_immutable BEFORE UPDATE OR DELETE ON member_quota_batch_commands
FOR EACH ROW EXECUTE FUNCTION reject_member_quota_batch_command_mutation();
CREATE TRIGGER member_quota_batch_commands_no_truncate BEFORE TRUNCATE ON member_quota_batch_commands
FOR EACH STATEMENT EXECUTE FUNCTION reject_member_quota_batch_command_mutation();
