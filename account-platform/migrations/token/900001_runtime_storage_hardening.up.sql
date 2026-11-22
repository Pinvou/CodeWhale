-- Forward-only: preserve historical checksums and evidence.
DO $$ BEGIN
 IF session_user<>'token_migrator' OR NOT pg_has_role('token_schema_owner','token_gateway','USAGE')
 OR pg_has_role('token_gateway','token_schema_owner','MEMBER') OR pg_has_role('token_gateway','token_cap_policy_deployer','MEMBER') THEN
 RAISE EXCEPTION 'Token hardening requires isolated provisioned deployment identities' USING ERRCODE='42501'; END IF;
END $$;
SET ROLE token_schema_owner;
CREATE TABLE public.token_runtime_hardening(singleton BOOLEAN PRIMARY KEY CHECK(singleton),installed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp());
INSERT INTO public.token_runtime_hardening(singleton) VALUES(true);
CREATE FUNCTION public.reject_token_truncate() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'Token storage cannot be truncated' USING ERRCODE='55000'; END $$;
CREATE FUNCTION public.reject_token_audit_mutation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN RAISE EXCEPTION 'Token audit facts are immutable' USING ERRCODE='55000'; END $$;
CREATE FUNCTION public.harden_token_runtime_objects() RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE object RECORD; qualified TEXT;
BEGIN
 IF current_user<>'token_schema_owner' THEN RAISE EXCEPTION 'Token hardening requires schema owner' USING ERRCODE='42501'; END IF;
 REVOKE CREATE ON SCHEMA public FROM PUBLIC,token_gateway;
 GRANT USAGE ON SCHEMA public TO token_gateway;
 FOR object IN SELECT c.relname,c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p','S','v','m') AND pg_get_userbyid(c.relowner) IN ('token_gateway','token_schema_owner') ORDER BY (c.relkind='S'),c.oid LOOP
 qualified:=format('public.%I',object.relname);
 IF object.relkind='S' THEN
 EXECUTE format('ALTER SEQUENCE %s OWNER TO token_schema_owner',qualified);
 EXECUTE format('REVOKE ALL ON SEQUENCE %s FROM PUBLIC,token_gateway',qualified);
 EXECUTE format('GRANT USAGE,SELECT ON SEQUENCE %s TO token_gateway',qualified);
 ELSE
 EXECUTE format('ALTER %s %s OWNER TO token_schema_owner',CASE object.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,qualified);
 EXECUTE format('REVOKE ALL ON TABLE %s FROM PUBLIC,token_gateway',qualified);
 EXECUTE format('GRANT SELECT ON TABLE %s TO token_gateway',qualified);
 IF object.relkind IN ('r','p') THEN
 IF object.relname NOT IN ('schema_migrations','token_runtime_hardening','token_command_governance_activation','token_command_governance_transitions') THEN
 EXECUTE format('GRANT INSERT,UPDATE,DELETE ON TABLE %s TO token_gateway',qualified);
 IF object.relname='token_audit_logs' OR EXISTS(SELECT 1 FROM pg_trigger trigger JOIN pg_proc function ON function.oid=trigger.tgfoid
 WHERE trigger.tgrelid=to_regclass(qualified) AND NOT trigger.tgisinternal AND function.proname IN (
 'reject_model_catalog_history_mutation','reject_usage_execution_history_mutation','reject_usage_command_or_evidence_mutation',
 'reject_token_manual_review_evidence_mutation','reject_route_health_history_mutation','reject_model_lifecycle_plan_event_mutation','reject_token_audit_mutation'))
 THEN EXECUTE format('REVOKE UPDATE,DELETE ON TABLE %s FROM token_gateway',qualified); END IF;
 END IF;
 EXECUTE format('DROP TRIGGER IF EXISTS token_no_truncate ON %s',qualified);
 EXECUTE format('CREATE TRIGGER token_no_truncate BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION public.reject_token_truncate()',qualified);
 END IF;
 END IF;
 END LOOP;
 FOR object IN SELECT p.oid::regprocedure::text AS signature,p.proname,p.prosecdef FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND pg_get_userbyid(p.proowner) IN ('token_gateway','token_schema_owner') LOOP
 EXECUTE format('ALTER FUNCTION %s OWNER TO token_schema_owner',object.signature);
 EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC,token_gateway',object.signature);
 IF object.prosecdef THEN
 IF object.proname<>'lock_token_command_governance_activation' THEN RAISE EXCEPTION 'unreviewed Token SECURITY DEFINER function'; END IF;
 EXECUTE format('ALTER FUNCTION %s SET search_path=pg_catalog',object.signature);
 END IF;
 IF object.proname NOT IN ('harden_token_runtime_objects','reject_token_truncate','reject_token_audit_mutation') THEN
 EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO token_gateway',object.signature);
 END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND pg_get_userbyid(c.relowner)='token_gateway') THEN
 RAISE EXCEPTION 'Token runtime still owns storage'; END IF;
END $$;
REVOKE ALL ON FUNCTION public.harden_token_runtime_objects(),public.reject_token_truncate(),public.reject_token_audit_mutation() FROM PUBLIC,token_gateway;
SELECT public.harden_token_runtime_objects();
CREATE TRIGGER token_audit_logs_immutable BEFORE UPDATE OR DELETE ON public.token_audit_logs FOR EACH ROW EXECUTE FUNCTION public.reject_token_audit_mutation();
REVOKE UPDATE,DELETE ON public.token_audit_logs FROM token_gateway;
CREATE INDEX usage_requests_admin_updated_idx ON public.usage_requests(updated_at DESC,id ASC);
-- Policy-owned facts retain their independent definer owner and ACL.
GRANT EXECUTE ON FUNCTION public.reject_token_truncate() TO token_cap_policy_deployer;
SET ROLE token_cap_policy_deployer;
DO $$ DECLARE object RECORD; BEGIN
 FOR object IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p') AND pg_get_userbyid(c.relowner)='token_cap_policy_deployer' LOOP
 EXECUTE format('CREATE TRIGGER token_no_truncate BEFORE TRUNCATE ON public.%I FOR EACH STATEMENT EXECUTE FUNCTION public.reject_token_truncate()',object.relname);
 END LOOP;
END $$;
SET ROLE token_gateway;
