\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;

SELECT 'identity_without_user' AS check_name,count(*) AS violations
FROM identities identity LEFT JOIN users account_user ON account_user.id=identity.user_id
WHERE account_user.id IS NULL;

SELECT 'active_session_without_user' AS check_name,count(*) AS violations
FROM user_sessions session LEFT JOIN users account_user ON account_user.id=session.user_id
WHERE account_user.id IS NULL;

SELECT 'organization_without_active_owner' AS check_name,count(*) AS violations
FROM organizations organization
WHERE organization.status='ACTIVE' AND NOT EXISTS (
  SELECT 1 FROM memberships membership
  JOIN role_bindings binding ON binding.membership_id=membership.id AND binding.role='OWNER'
  WHERE membership.organization_id=organization.id AND membership.status='ACTIVE'
);

SELECT 'closure_missing_self_edge' AS check_name,count(*) AS violations
FROM organization_units unit
WHERE NOT EXISTS (
  SELECT 1 FROM organization_unit_closure closure
  WHERE closure.organization_id=unit.organization_id AND closure.ancestor_id=unit.id
    AND closure.descendant_id=unit.id AND closure.depth=0
);

ROLLBACK;
