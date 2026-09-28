SET ROLE billing_service;
DO $$
DECLARE is_superuser BOOLEAN;
BEGIN
    SELECT rolsuper INTO is_superuser FROM pg_catalog.pg_roles WHERE rolname=session_user;
    IF session_user<>'billing_migrator' AND NOT is_superuser THEN
        RAISE EXCEPTION '000078 requires the dedicated billing_migrator deployment identity';
    END IF;
    IF NOT is_superuser AND current_user<>'billing_service' THEN
        RAISE EXCEPTION '000078 must run with MIGRATION_ROLE=billing_service';
    END IF;
    IF NOT pg_catalog.pg_has_role(session_user,'billing_schema_owner','MEMBER')
       OR pg_catalog.pg_has_role('billing_service','billing_schema_owner','MEMBER') THEN
        RAISE EXCEPTION '000078 schema owner must remain separate from runtime';
    END IF;
    IF (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.member_quota_grants'::regclass) <> 'billing_service'
       OR (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.member_quota_grant_audits'::regclass) <> 'billing_service'
       OR (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.organization_member_lifecycle_states'::regclass) <> 'billing_service'
       OR (SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class
        WHERE oid='public.outbox_events'::regclass) <> 'billing_service' THEN
        RAISE EXCEPTION '000078 member quota lifecycle tables must stay owned by billing_service';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                   WHERE conname='member_quota_grant_audits_action_check'
                     AND conrelid='public.member_quota_grant_audits'::regclass) THEN
        RAISE EXCEPTION '000078 requires the existing member quota audit action check';
    END IF;
END $$;

-- Runtime member-quota transactions acquire grant rows before appending audit
-- and outbox records. Take the conflicting table locks in the same order before
-- changing the audit constraint, so the migration cannot hold the audit DDL
-- lock while waiting for an in-flight grant update. The later lifecycle read is
-- MVCC-only and the outbox insert takes a RowExclusiveLock, which is compatible
-- with normal service writes; neither introduces a reverse incompatible edge.
LOCK TABLE public.member_quota_grants IN ACCESS EXCLUSIVE MODE;
LOCK TABLE public.member_quota_grant_audits IN ACCESS EXCLUSIVE MODE;

SET ROLE billing_service;
ALTER TABLE public.member_quota_grant_audits DROP CONSTRAINT member_quota_grant_audits_action_check;
ALTER TABLE public.member_quota_grant_audits ADD CONSTRAINT member_quota_grant_audits_action_check
    CHECK (action IN ('ALLOCATED','SUSPENDED','RESTORED','REVOKED','HOLD_RESERVED','HOLD_RELEASED','HOLD_SETTLED','EXPIRED','ACCOUNT_CLOSED_ISOLATED','RECLAIMED_ACCOUNT_CLOSED','MEMBERSHIP_VERSION_ADVANCED'));

-- Repair grants written before organization_membership_version was separated
-- from the user's global membership version. Only live ACTIVE grants whose
-- authoritative organization member is still ACTIVE are advanced; balances,
-- status, expiry, and existing Hold allocations are left untouched.
CREATE TEMP TABLE member_quota_version_reconciled ON COMMIT DROP AS
WITH reconciled AS (
    UPDATE public.member_quota_grants grant
       SET membership_version=lifecycle.membership_version,
           allocation_version=grant.allocation_version+1,
           updated_at=transaction_timestamp()
      FROM public.organization_member_lifecycle_states lifecycle
     WHERE lifecycle.organization_id=grant.organization_id
       AND lifecycle.user_id=grant.member_user_id
       AND lifecycle.status='ACTIVE'
       AND grant.status='ACTIVE'
       AND grant.expires_at>transaction_timestamp()
       AND grant.membership_version<>lifecycle.membership_version
    RETURNING grant.*
)
SELECT * FROM reconciled;

INSERT INTO public.member_quota_grant_audits(
    id,grant_id,organization_id,member_user_id,action,actor_user_id,
    amount_microcredits,snapshot,occurred_at
)
SELECT gen_random_uuid(),grant.id,grant.organization_id,grant.member_user_id,
       'MEMBERSHIP_VERSION_ADVANCED',NULL,0,to_jsonb(grant),transaction_timestamp()
FROM member_quota_version_reconciled grant;

INSERT INTO public.outbox_events(
    event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at
)
SELECT gen_random_uuid(),'billing.member-quota.membership-version-advanced.v1',
       grant.id,grant.allocation_version,to_jsonb(grant),
       'member-quota:'||grant.id::text,transaction_timestamp()
FROM member_quota_version_reconciled grant;

SET ROLE billing_service;
