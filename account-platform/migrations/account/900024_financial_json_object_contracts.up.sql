-- Validate existing evidence without rewriting or discarding any history.
DO $$ BEGIN
 IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
  RAISE EXCEPTION '900024 requires isolated account migration identities' USING ERRCODE='42501';
 END IF;
END $$;
ALTER TABLE public.approval_executions ADD CONSTRAINT approval_executions_critical_resource_versions_object_check
 CHECK (jsonb_typeof(critical_resource_versions)='object') NOT VALID;
ALTER TABLE public.approval_executions ADD CONSTRAINT approval_executions_financial_snapshot_object_check
 CHECK (jsonb_typeof(financial_snapshot)='object') NOT VALID;
ALTER TABLE public.approval_executions VALIDATE CONSTRAINT approval_executions_critical_resource_versions_object_check;
ALTER TABLE public.approval_executions VALIDATE CONSTRAINT approval_executions_financial_snapshot_object_check;
