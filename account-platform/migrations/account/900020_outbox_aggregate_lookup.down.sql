-- Remove only this lookup index. Never delete events or change their ACLs.
DO $$
DECLARE valid boolean;
BEGIN
 SELECT i.indisvalid AND i.indisready AND NOT i.indisunique
        AND i.indpred IS NULL AND i.indexprs IS NULL
        AND i.indnatts=1 AND i.indnkeyatts=1
        AND i.indkey::text=(SELECT attnum::text FROM pg_catalog.pg_attribute WHERE attrelid='public.outbox_events'::regclass AND attname='aggregate_id' AND NOT attisdropped)
        AND am.amname='btree'
        AND pg_catalog.pg_get_userbyid(index_relation.relowner)='account_approval_authorization_owner'
 INTO valid
 FROM pg_catalog.pg_index i
 JOIN pg_catalog.pg_class index_relation ON index_relation.oid=i.indexrelid
 JOIN pg_catalog.pg_am am ON am.oid=index_relation.relam
 WHERE i.indexrelid=pg_catalog.to_regclass('public.outbox_events_aggregate_idx')
   AND i.indrelid='public.outbox_events'::regclass;
 IF valid IS DISTINCT FROM true THEN
  RAISE EXCEPTION 'outbox aggregate index identity changed; rollback refused';
 END IF;
END $$;
SET LOCAL ROLE account_approval_authorization_owner;
DROP INDEX public.outbox_events_aggregate_idx;
SET LOCAL ROLE account_service;
