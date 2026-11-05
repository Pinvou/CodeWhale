DROP INDEX IF EXISTS member_quota_grant_audits_delegation_idx;

ALTER TABLE account_closed_grant_reclaim_commands
    DROP CONSTRAINT account_closed_grant_reclaim_delegation_complete,
    DROP COLUMN delegation_authorization_version,
    DROP COLUMN delegated_permission,
    DROP COLUMN delegation_id,
    DROP COLUMN workload_subject,
    DROP COLUMN workload_service,
    DROP COLUMN delegation_evidence_required;

ALTER TABLE member_quota_grant_audits
    DROP CONSTRAINT member_quota_grant_audits_delegation_complete,
    DROP COLUMN delegation_authorization_version,
    DROP COLUMN delegated_permission,
    DROP COLUMN delegation_id,
    DROP COLUMN workload_subject,
    DROP COLUMN workload_service;
