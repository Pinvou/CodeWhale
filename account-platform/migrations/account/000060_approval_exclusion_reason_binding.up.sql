-- Bind every authorization delivery exclusion to the request's exact
-- invalidation fact and, in SAFE_ROLLBACK, to the immediately preceding
-- approval epoch.  An older epoch is evidence of an incomplete earlier gate,
-- not a fact that SAFE recovery may silently discount.
DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000060 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=59) THEN
    RAISE EXCEPTION '000060 requires approval authorization exclusions' USING ERRCODE='55000';
  END IF;
END $$;

GRANT SELECT(invalidation_code) ON public.approval_requests TO account_approval_authorization_owner;

RESET ROLE;
SET ROLE account_approval_authorization_owner;

CREATE FUNCTION account_approval_authorization.validate_authorization_exclusion_reason()
RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE control_row RECORD; request_code TEXT; audit_summary JSONB;
BEGIN
  SELECT * INTO STRICT control_row FROM account_approval_control.lock_admin_approval_control_shared();
  SELECT r.invalidation_code INTO STRICT request_code
    FROM public.approval_requests r
   WHERE r.id=NEW.request_id AND r.status='INVALIDATED' AND r.version=NEW.request_version;
  SELECT a.change_summary INTO STRICT audit_summary FROM public.audit_logs a WHERE a.id=NEW.audit_ref;
  IF (NEW.reason_code='CUTOVER_INVALIDATED_NO_EXECUTION' AND request_code<>'EXECUTION_WINDOW_EXPIRED')
     OR (NEW.reason_code='SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION'
         AND request_code NOT IN ('EXECUTION_WINDOW_EXPIRED','APPROVAL_SAFE_ROLLBACK_INVALIDATED'))
     OR audit_summary->>'invalidation_code' IS DISTINCT FROM request_code
     OR NEW.sealed_at_control_approval_epoch<>control_row.approval_epoch
     OR (control_row.mode='SAFE_ROLLBACK'
         AND (control_row.approval_epoch<=1 OR NEW.event_approval_epoch<>control_row.approval_epoch-1)) THEN
    RAISE EXCEPTION 'authorization exclusion reason or epoch is not bound to invalidation history' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER approval_authorization_exclusions_reason_guard
BEFORE INSERT ON account_approval_authorization.approval_authorization_delivery_exclusions
FOR EACH ROW EXECUTE FUNCTION account_approval_authorization.validate_authorization_exclusion_reason();

CREATE OR REPLACE FUNCTION account_approval_authorization.authorization_exclusion_matches(p_event UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
  SELECT EXISTS(
    SELECT 1
      FROM account_approval_authorization.approval_authorization_delivery_exclusions x
      JOIN public.outbox_events o ON o.event_id=x.event_id
      JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
      JOIN public.approval_requests r ON r.id=x.request_id
      JOIN public.audit_logs a ON a.id=x.audit_ref
     WHERE x.event_id=p_event
       AND o.event_type='account.approval-command.authorized.v1'
       AND o.aggregate_id=x.request_id
       AND o.approval_control_epoch=x.event_approval_epoch
       AND o.published_at IS NULL AND o.dead_lettered_at IS NOT NULL
       AND o.locked_by IS NULL AND o.locked_until IS NULL
       AND s.event_digest=x.event_digest
       AND s.provenance IN ('FROZEN_AT_APPROVAL','RECONSTRUCTED_FOR_DELIVERY')
       AND s.delivery_bytes IS NOT NULL
       AND s.event_digest=pg_catalog.encode(pg_catalog.sha256(s.delivery_bytes),'hex')
       AND r.id=o.aggregate_id AND r.status=x.request_status AND r.version=x.request_version
       AND ((x.reason_code='CUTOVER_INVALIDATED_NO_EXECUTION' AND r.invalidation_code='EXECUTION_WINDOW_EXPIRED')
         OR (x.reason_code='SAFE_ROLLBACK_INVALIDATED_NO_EXECUTION'
             AND r.invalidation_code IN ('EXECUTION_WINDOW_EXPIRED','APPROVAL_SAFE_ROLLBACK_INVALIDATED')
             AND x.sealed_at_control_approval_epoch>1
             AND x.event_approval_epoch=x.sealed_at_control_approval_epoch-1))
       AND NOT EXISTS(SELECT 1 FROM public.approval_executions e WHERE e.approval_request_id=r.id)
       AND a.action='approval.authorization-exclusion.effect-verified'
       AND a.resource_type='approval_authorization_event'
       AND a.resource_id=x.event_id::TEXT AND a.result='SUCCEEDED'
       AND a.occurred_at=x.verified_at
       AND a.change_summary->>'request_id'=x.request_id::TEXT
       AND a.change_summary->>'event_digest'=x.event_digest
       AND a.change_summary->>'effect_absent'='true'
       AND (NOT (a.change_summary ? 'invalidation_code')
         OR a.change_summary->>'invalidation_code'=r.invalidation_code)
  )
$$;

DO $$
BEGIN
  IF EXISTS(
    SELECT 1 FROM account_approval_authorization.approval_authorization_delivery_exclusions x
    WHERE NOT account_approval_authorization.authorization_exclusion_matches(x.event_id)
  ) THEN
    RAISE EXCEPTION '000060 found an exclusion without exact invalidation binding' USING ERRCODE='55000';
  END IF;
END $$;

REVOKE ALL ON FUNCTION account_approval_authorization.validate_authorization_exclusion_reason()
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;
REVOKE ALL ON FUNCTION account_approval_authorization.authorization_exclusion_matches(UUID)
  FROM PUBLIC,account_service,account_approval_writer,account_approval_backfill,account_approval_relay;

RESET ROLE;
SET ROLE account_service;
