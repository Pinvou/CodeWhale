DROP TRIGGER role_bindings_rbac_mirror ON role_bindings;
DROP FUNCTION mirror_legacy_role_binding();
DROP TRIGGER role_assignments_authorization_version ON role_assignments;
DROP FUNCTION bump_authorization_version();
DROP TABLE authorization_versions;
DROP TABLE role_assignments;
DROP TABLE role_permissions;
DROP TABLE roles;
DROP TABLE permissions;
