-- The route-health worker must be stopped before this rollback. Preserve the last
-- server evidence as non-routable while the old read projection is restored.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM route_health_observations)
       OR EXISTS (SELECT 1 FROM routing_policy_tombstones)
       OR EXISTS (SELECT 1 FROM routing_policy_mutations) THEN
        RAISE EXCEPTION 'APT-125 rollback refused: immutable route history must be archived before downgrade';
    END IF;
    IF EXISTS (SELECT 1 FROM routing_policies WHERE reference_status='DELETED')
       OR EXISTS (
            SELECT 1 FROM routing_policies
             WHERE reference_status<>'DELETED'
             GROUP BY token_account_id,model
            HAVING count(*)>1
       ) THEN
        RAISE EXCEPTION 'APT-125 rollback refused: v24 cannot represent deleted or multi-protocol route facts';
    END IF;
END $$;

UPDATE routing_policies
   SET health_status='UNHEALTHY'
 WHERE health_status IN ('UNKNOWN','PROBING','DEGRADED','STALE');

DROP TRIGGER routing_policy_tombstone_immutable ON routing_policy_tombstones;
DROP TRIGGER route_health_observation_immutable ON route_health_observations;
DROP FUNCTION reject_route_health_history_mutation();
DROP TABLE routing_policy_tombstones;
DROP TABLE routing_policy_mutations;
DROP TABLE route_health_observations;
DROP INDEX routing_policy_health_due_idx;
DROP INDEX routing_policy_stable_reference_uniq;
-- A v24 reader can represent only one route per account/upstream reference.
-- Keep the newest generation deterministically before restoring its constraint.
DELETE FROM routing_policies losing
USING routing_policies winning
WHERE losing.token_account_id=winning.token_account_id
  AND losing.model=winning.model
  AND (losing.route_generation,losing.updated_at,losing.id)<(winning.route_generation,winning.updated_at,winning.id);
ALTER TABLE routing_policies DROP CONSTRAINT routing_policy_stable_reference_complete;
ALTER TABLE routing_policies DROP CONSTRAINT routing_policy_probe_lease_consistent;
ALTER TABLE routing_policies DROP CONSTRAINT routing_policy_health_evidence_consistent;
ALTER TABLE routing_policies DROP CONSTRAINT routing_policy_health_diagnostic_stable;
ALTER TABLE routing_policies DROP CONSTRAINT routing_policies_health_status_check;
ALTER TABLE routing_policies ADD CONSTRAINT routing_policies_health_status_check
    CHECK (health_status IN ('HEALTHY','DEGRADED','UNHEALTHY'));
ALTER TABLE routing_policies
    DROP COLUMN probe_lease_until,
    DROP COLUMN probe_lease_owner,
    DROP COLUMN next_probe_at,
    DROP COLUMN health_failure_count,
    DROP COLUMN health_diagnostic_code,
    DROP COLUMN health_expires_at,
    DROP COLUMN health_observed_at,
    DROP COLUMN health_version,
    DROP COLUMN route_version,
    DROP COLUMN reference_status,
    DROP COLUMN protocol_version_id,
    DROP COLUMN model_id;
ALTER TABLE routing_policies
    ADD CONSTRAINT routing_policies_token_account_id_model_key UNIQUE(token_account_id,model);
ALTER TABLE route_provisionings DROP COLUMN protocol_version_id;
ALTER TABLE token_accounts DROP COLUMN management_version;
