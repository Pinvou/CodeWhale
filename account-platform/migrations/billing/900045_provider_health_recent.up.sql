-- Support the complete global recent-observation discovery without replacing
-- the existing provider-leading index used by each partition's evidence read.
DO $$ BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'provider health index requires isolated deployment identities' USING ERRCODE='42501';
 END IF;
END $$;
CREATE INDEX payment_provider_health_recent_idx
 ON public.payment_provider_health_observations (received_at)
 INCLUDE (provider_id,provider_version,capability,channel,route);
