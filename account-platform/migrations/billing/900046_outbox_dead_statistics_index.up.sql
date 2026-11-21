-- Keep every event and its references; index only the exact dead-letter set.
DO $$ BEGIN
 IF session_user<>'billing_migrator' OR current_user<>'billing_service' THEN
  RAISE EXCEPTION 'outbox statistics index requires the dedicated migration identity';
 END IF;
END $$;
CREATE INDEX outbox_events_dead_statistics_idx ON public.outbox_events (event_type) WHERE dead_lettered_at IS NOT NULL;
