package accountschema

import (
	"context"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const threeScopeVersion int64 = 34
const threeScopeName = "three_scope_authorization"
const threeScopeUpChecksum = "658324df04b4e19fb98531c192f409fb68b82f5d0f027fe6b65527ebddca8637"
const threeScopeDownChecksum = "2c431af91fd697f08754f4a783c800f6b4f30326d1c121cfb5c2c7f8d76edabc"

const threeScopeInvalidationSQL = `SELECT bump_role_assignment_scope_version(user_id,role_code,organization_id,unit_id)
FROM role_assignments
WHERE revoked_at IS NULL;`

func validateThreeScopeRollback(m migrations.Migration) error {
	if m.Version != threeScopeVersion || m.Name != threeScopeName || m.Checksum != threeScopeUpChecksum || governanceSQLDigest(normalizedGovernanceSQL(m.UpSQL)) != threeScopeUpChecksum || governanceSQLDigest(normalizedGovernanceSQL(m.DownSQL)) != threeScopeDownChecksum {
		return errors.New("three-scope rollback requires the exact frozen migration identity")
	}
	return nil
}

func guardThreeScopeRollback(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if err := validateThreeScopeRollback(m); err != nil {
		return err
	}
	var name, checksum string
	if err := tx.QueryRow(ctx, `SELECT name,checksum FROM schema_migrations WHERE version=$1 FOR UPDATE`, m.Version).Scan(&name, &checksum); err != nil {
		return err
	}
	// These are the two existing retainedScopeChecksums: frozen LF and the
	// identical historical CRLF checkout. No ledger bytes are rewritten.
	crlf := governanceSQLDigest(strings.ReplaceAll(normalizedGovernanceSQL(m.UpSQL), "\n", "\r\n"))
	if name != threeScopeName || checksum != threeScopeUpChecksum && checksum != crlf {
		return errors.New("applied three-scope migration identity does not match the frozen source")
	}
	return nil
}

func compatibleThreeScopeRollback(m migrations.Migration) (string, error) {
	if err := validateThreeScopeRollback(m); err != nil {
		return "", err
	}
	sql := normalizedGovernanceSQL(m.DownSQL)
	if strings.Count(sql, threeScopeInvalidationSQL) != 1 {
		return "", errors.New("frozen three-scope invalidation statement is absent")
	}
	// Keep the original loop: UNIT assignments invalidate their organization
	// cache as well as recreating a UNIT version. Only discard retired cache
	// scopes after that loop, before restoring the two-scope CHECK. Bindings,
	// users and organizations remain untouched; original role-removal FK errors
	// (including bound ORG_MODEL_ADMIN) still abort the entire transaction.
	return strings.Replace(sql, threeScopeInvalidationSQL, threeScopeInvalidationSQL+"\n\nDELETE FROM authorization_versions WHERE scope IN ('PERSONAL','UNIT');", 1), nil
}
