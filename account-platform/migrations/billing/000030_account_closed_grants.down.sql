DROP TRIGGER account_closed_grant_reclaim_commands_immutable ON account_closed_grant_reclaim_commands;
DROP TRIGGER account_closure_grant_isolations_immutable ON account_closure_grant_isolations;
DROP FUNCTION protect_account_closed_grant_commands();
DROP TABLE account_closed_grant_reclaim_commands;
DROP TABLE account_closure_grant_isolations;

ALTER TABLE member_quota_grant_audits DROP CONSTRAINT member_quota_grant_audits_action_check;
ALTER TABLE member_quota_grant_audits ADD CONSTRAINT member_quota_grant_audits_action_check
    CHECK (action IN ('ALLOCATED','SUSPENDED','RESTORED','REVOKED','HOLD_RESERVED','HOLD_RELEASED','HOLD_SETTLED','EXPIRED'));
