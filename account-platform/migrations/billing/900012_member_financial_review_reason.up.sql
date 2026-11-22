-- Restore the mature financial-review reason without modifying original 000027.
SET ROLE billing_service;
DO $$
BEGIN
 IF session_user <> 'billing_migrator' OR current_user <> 'billing_service'
    OR NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
    OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
  RAISE EXCEPTION 'financial review reasons require the dedicated deployment roles';
 END IF;
 IF (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
     WHERE oid='public.member_quota_grants'::regclass) <> 'billing_service' THEN
  RAISE EXCEPTION 'member quota grants must retain the runtime owner';
 END IF;
END $$;
LOCK TABLE public.member_quota_grants IN ACCESS EXCLUSIVE MODE;
ALTER TABLE public.member_quota_grants DROP CONSTRAINT member_quota_grants_suspension_reason_check;
ALTER TABLE public.member_quota_grants ADD CONSTRAINT member_quota_grants_suspension_reason_check
 CHECK (suspension_reason IN ('MEMBER_REMOVED','ACCOUNT_CLOSED','CHARGEBACK_REVIEW','REFUND_PENDING','FORCED_REFUND_REVIEW'));
