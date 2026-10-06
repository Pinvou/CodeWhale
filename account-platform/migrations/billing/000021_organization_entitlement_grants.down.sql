DROP TABLE member_quota_grant_audits;
DROP TABLE member_quota_hold_allocations;
DROP TABLE member_quota_revoke_commands;
DROP TABLE member_quota_grants;
ALTER TABLE entitlement_components DROP CONSTRAINT entitlement_components_grant_identity_unique;
ALTER TABLE payment_orders DROP COLUMN beneficiary_organization_id;

ALTER TABLE usage_holds DROP CONSTRAINT usage_holds_member_authority, DROP COLUMN quota_organization_id, DROP COLUMN quota_member_user_id, DROP COLUMN quota_membership_version;
