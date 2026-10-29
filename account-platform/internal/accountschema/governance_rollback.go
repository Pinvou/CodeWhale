package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const governancePermissionsVersion int64 = 39
const governancePermissionsName = "organization_governance_permissions"
const governancePermissionsChecksum = "47aa9cccf678caaba684ee9d6d2cc133bb98b8f4d20389ded3037e28c6f330e7"
const governancePermissionsDownChecksum = "cdca3a2cb723c69bcf7b51943283881d18a05365b8c544c942ce77858e9b53be"

const originalGovernanceInvalidationSQL = `SELECT DISTINCT bump_role_assignment_scope_version(
    ra.user_id,ra.role_code,ra.organization_id,ra.unit_id
)
FROM role_assignments ra
JOIN role_permissions rp ON rp.role_code=ra.role_code
WHERE ra.revoked_at IS NULL
  AND rp.permission_code IN (
      'platform.organizations.profile.manage',
      'platform.organizations.members.manage',
      'platform.organizations.units.manage'
  );`

// Use the exact input-tuple DISTINCT from original 039 Up. The void function
// is evaluated once per distinct assignment tuple, never compared by DISTINCT.
const compatibleGovernanceInvalidationSQL = `SELECT bump_role_assignment_scope_version(
    assignment.user_id,assignment.role_code,
    assignment.organization_id,assignment.unit_id
)
FROM (
    SELECT DISTINCT ra.user_id,ra.role_code,ra.organization_id,ra.unit_id
    FROM role_assignments ra
    JOIN role_permissions rp ON rp.role_code=ra.role_code
    WHERE ra.revoked_at IS NULL
      AND rp.permission_code IN (
          'platform.organizations.profile.manage',
          'platform.organizations.members.manage',
          'platform.organizations.units.manage'
      )
) assignment;`

func normalizedGovernanceSQL(source string) string {
	return strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
}
func governanceSQLDigest(source string) string {
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}
func validateGovernancePermissionsRollback(m migrations.Migration) error {
	if m.Version != governancePermissionsVersion || m.Name != governancePermissionsName || m.Checksum != governancePermissionsChecksum || governanceSQLDigest(normalizedGovernanceSQL(m.UpSQL)) != governancePermissionsChecksum || governanceSQLDigest(normalizedGovernanceSQL(m.DownSQL)) != governancePermissionsDownChecksum {
		return errors.New("organization governance rollback requires the exact frozen migration identity")
	}
	return nil
}

func guardGovernancePermissionsRollback(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if err := validateGovernancePermissionsRollback(m); err != nil {
		return err
	}
	var name, checksum string
	if err := tx.QueryRow(ctx, `SELECT name,checksum FROM schema_migrations WHERE version=$1 FOR UPDATE`, m.Version).Scan(&name, &checksum); err != nil {
		return err
	}
	// Preserve the migration engine's historical LF/CRLF checksum compatibility;
	// neither source bytes nor an existing ledger identity are rewritten.
	crlfChecksum := governanceSQLDigest(strings.ReplaceAll(normalizedGovernanceSQL(m.UpSQL), "\n", "\r\n"))
	if name != m.Name || checksum != governancePermissionsChecksum && checksum != crlfChecksum {
		return errors.New("applied organization governance migration identity does not match the frozen source")
	}
	return nil
}

func applyAccountRollbackSQL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	sql := m.DownSQL
	if m.Version == threeScopeVersion {
		var err error
		sql, err = compatibleThreeScopeRollback(m)
		if err != nil {
			return err
		}
	}
	if m.Version == governancePermissionsVersion {
		if err := validateGovernancePermissionsRollback(m); err != nil {
			return err
		}
		sql = normalizedGovernanceSQL(sql)
		if strings.Count(sql, originalGovernanceInvalidationSQL) != 1 {
			return errors.New("frozen organization governance invalidation statement is absent")
		}
		sql = strings.Replace(sql, originalGovernanceInvalidationSQL, compatibleGovernanceInvalidationSQL, 1)
	}
	_, err := tx.Exec(ctx, sql)
	return err
}
