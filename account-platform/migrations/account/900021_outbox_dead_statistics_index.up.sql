-- Keep every event and its references; index only the exact dead-letter set.
DO $$ BEGIN
 IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
  RAISE EXCEPTION 'outbox statistics index requires the dedicated migration identity';
 END IF;
END $$;
DO $$ BEGIN
 IF pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid='public.outbox_events'::regclass))<>'account_approval_authorization_owner'
 OR has_schema_privilege('account_approval_authorization_owner','public','CREATE') THEN
  RAISE EXCEPTION 'outbox statistics index requires the installed authorization ownership boundary';
 END IF;
END $$;
GRANT CREATE ON SCHEMA public TO account_approval_authorization_owner;
SET LOCAL ROLE account_approval_authorization_owner;
CREATE INDEX outbox_events_dead_statistics_idx ON public.outbox_events (event_type) WHERE dead_lettered_at IS NOT NULL;
SET LOCAL ROLE account_service;
REVOKE CREATE ON SCHEMA public FROM account_approval_authorization_owner;
