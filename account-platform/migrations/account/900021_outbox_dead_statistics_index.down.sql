-- Remove only this index; event history, grants and other indexes remain.
DO $$ BEGIN
 IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
  RAISE EXCEPTION 'outbox statistics index requires the dedicated migration identity';
 END IF;
END $$;
DO $$ BEGIN
 IF NOT EXISTS (
  SELECT FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am a ON a.oid=c.relam
  WHERE c.oid=to_regclass('public.outbox_events_dead_statistics_idx')
   AND i.indrelid='public.outbox_events'::regclass AND i.indisvalid AND i.indisready
   AND a.amname='btree' AND NOT i.indisunique AND i.indnatts=1 AND i.indnkeyatts=1
   AND i.indexprs IS NULL AND pg_get_indexdef(i.indexrelid,1,true)='event_type'
   AND pg_get_expr(i.indpred,i.indrelid)='(dead_lettered_at IS NOT NULL)'
   AND pg_get_userbyid(c.relowner)='account_approval_authorization_owner'
 ) THEN RAISE EXCEPTION 'outbox statistics rollback requires the exact installed index'; END IF;
END $$;
SET LOCAL ROLE account_approval_authorization_owner;
DROP INDEX public.outbox_events_dead_statistics_idx;
SET LOCAL ROLE account_service;
