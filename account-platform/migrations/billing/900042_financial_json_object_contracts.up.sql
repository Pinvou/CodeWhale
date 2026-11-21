-- Validate existing evidence without rewriting or discarding any history.
DO $$ BEGIN
 IF session_user<>'billing_migrator' OR current_user<>'billing_service' THEN
  RAISE EXCEPTION '900042 requires isolated billing migration identities' USING ERRCODE='42501';
 END IF;
END $$;
RESET ROLE;
SET ROLE billing_schema_owner;
ALTER TABLE public.manual_review_case_events ADD CONSTRAINT manual_review_events_details_object_check
 CHECK (jsonb_typeof(details)='object') NOT VALID;
ALTER TABLE public.late_payment_review_cases ADD CONSTRAINT late_review_provider_capability_object_check
 CHECK (jsonb_typeof(provider_capability_snapshot)='object') NOT VALID;
ALTER TABLE public.late_payment_review_cases ADD CONSTRAINT late_review_policy_snapshot_object_check
 CHECK (jsonb_typeof(policy_snapshot)='object') NOT VALID;
ALTER TABLE public.late_payment_review_cases ADD CONSTRAINT late_review_evidence_summary_object_check
 CHECK (jsonb_typeof(evidence_summary)='object') NOT VALID;
ALTER TABLE public.late_payment_review_evidence ADD CONSTRAINT late_review_evidence_payload_object_check
 CHECK (jsonb_typeof(payload_snapshot)='object') NOT VALID;
ALTER TABLE public.manual_review_case_events VALIDATE CONSTRAINT manual_review_events_details_object_check;
ALTER TABLE public.late_payment_review_cases VALIDATE CONSTRAINT late_review_provider_capability_object_check;
ALTER TABLE public.late_payment_review_cases VALIDATE CONSTRAINT late_review_policy_snapshot_object_check;
ALTER TABLE public.late_payment_review_cases VALIDATE CONSTRAINT late_review_evidence_summary_object_check;
ALTER TABLE public.late_payment_review_evidence VALIDATE CONSTRAINT late_review_evidence_payload_object_check;
RESET ROLE;
SET ROLE billing_service;
