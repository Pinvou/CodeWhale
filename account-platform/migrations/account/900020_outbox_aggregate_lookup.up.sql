-- Registration compensation must check every event type for the candidate.
-- Keep the immutable outbox rows and the original authorization-only index.
DO $$ BEGIN
 IF pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass)) <> 'account_approval_authorization_owner' THEN
  RAISE EXCEPTION 'outbox aggregate index requires the installed authorization owner';
 END IF;
 IF pg_catalog.has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
  RAISE EXCEPTION 'outbox authorization owner has unexpected persistent schema CREATE';
 END IF;
END $$;
-- Match the original ownership bridge: grant only inside this transaction,
-- and restore the existing schema ACL before recording the migration.
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
SET LOCAL ROLE account_approval_authorization_owner;
CREATE INDEX outbox_events_aggregate_idx ON public.outbox_events (aggregate_id);
SET LOCAL ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
