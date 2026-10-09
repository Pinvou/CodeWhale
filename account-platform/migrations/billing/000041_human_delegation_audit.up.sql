ALTER TABLE member_quota_grant_audits
    ADD COLUMN workload_service TEXT,
    ADD COLUMN workload_subject TEXT,
    ADD COLUMN delegation_id UUID,
    ADD COLUMN delegated_permission TEXT,
    ADD COLUMN delegation_authorization_version BIGINT,
    ADD CONSTRAINT member_quota_grant_audits_delegation_complete CHECK (
        num_nonnulls(workload_service,workload_subject,delegation_id,delegated_permission,delegation_authorization_version) = 0
        OR
        (workload_service IS NOT NULL AND workload_service <> '' AND workload_subject IS NOT NULL AND workload_subject <> '' AND delegation_id IS NOT NULL AND delegated_permission IS NOT NULL AND delegated_permission LIKE 'org.%' AND delegation_authorization_version IS NOT NULL AND delegation_authorization_version > 0)
    );

ALTER TABLE account_closed_grant_reclaim_commands
    ADD COLUMN delegation_evidence_required BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN workload_service TEXT,
    ADD COLUMN workload_subject TEXT,
    ADD COLUMN delegation_id UUID,
    ADD COLUMN delegated_permission TEXT,
    ADD COLUMN delegation_authorization_version BIGINT;

ALTER TABLE account_closed_grant_reclaim_commands
    ALTER COLUMN delegation_evidence_required SET DEFAULT TRUE,
    ADD CONSTRAINT account_closed_grant_reclaim_delegation_complete CHECK (
        NOT delegation_evidence_required
        OR (workload_service IS NOT NULL AND workload_service <> '' AND workload_subject IS NOT NULL AND workload_subject <> '' AND delegation_id IS NOT NULL AND delegated_permission IS NOT NULL AND delegated_permission = 'org.grants.reclaim_closed' AND delegation_authorization_version IS NOT NULL AND delegation_authorization_version > 0)
    );

CREATE INDEX member_quota_grant_audits_delegation_idx
    ON member_quota_grant_audits(delegation_id)
    WHERE delegation_id IS NOT NULL;
