DROP TRIGGER organizations_lifecycle_guard ON organizations;
DROP FUNCTION protect_organization_lifecycle();
DROP TRIGGER organization_creation_requests_immutable ON organization_creation_requests;
DROP FUNCTION protect_organization_creation_request();
DROP TABLE organization_creation_requests;
DROP INDEX organizations_active_name_idx;
ALTER TABLE organizations DROP COLUMN deleted_at;
