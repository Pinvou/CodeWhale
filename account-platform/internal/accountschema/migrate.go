// Package accountschema preserves the original SQL versions while allowing
// complete personal identity to be delivered before later domain producers.
package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const metadataVersion int64 = 900001

const metadataChecksum = "b14cb9c2e538d20ec869458940d60867090549c027f439ed199d703ef3e7f45d"
const metadataDownChecksum = "fb0c59fb6749b7c82dc824a64a248a152b5a86672c2b7c44fdd06c52ab2cef04"

type permissionFact struct{ code, description string }
type roleEdge struct{ role, permission string }

var retainedPermissions = map[int64][]permissionFact{
	27: {{"platform.account-closures.read", "Read redacted account closure state and blockers."}, {"platform.account-closures.repair", "Retry a failed account closure stage without bypassing blockers."}},
	33: {{"platform.billing.payment-controls.resume.request", "Request version-frozen payment Provider recovery."}, {"platform.billing.payment-controls.resume.approve", "Independently approve payment Provider recovery."}},
}

// Ordered places the narrowly scoped metadata seed before original 034. Lower
// original versions remain in their order; no applied version is skipped based
// on MAX(version). Every following delivery carries the same seed bytes.
func Ordered(input []migrations.Migration) ([]migrations.Migration, error) {
	if err := validateRegistrationBonusGuard(input); err != nil {
		return nil, err
	}
	if err := validateMemberUnitAssignments(input); err != nil {
		return nil, err
	}
	if err := validateExecutionGuard(input); err != nil {
		return nil, err
	}
	var seed *migrations.Migration
	for _, migration := range input {
		if migration.Version == metadataVersion {
			if seed != nil {
				return nil, errors.New("duplicate first-delivery metadata migration")
			}
			copy := migration
			seed = &copy
		}
	}
	if seed == nil {
		return nil, errors.New("first-delivery metadata migration is required")
	}
	canonicalSQL := strings.ReplaceAll(strings.ReplaceAll(seed.UpSQL, "\r\n", "\n"), "\r", "\n")
	digest := sha256.Sum256([]byte(canonicalSQL))
	if seed.Checksum != metadataChecksum || hex.EncodeToString(digest[:]) != metadataChecksum {
		return nil, errors.New("first-delivery metadata migration is not the frozen SQL")
	}
	canonicalDown := strings.ReplaceAll(strings.ReplaceAll(seed.DownSQL, "\r\n", "\n"), "\r", "\n")
	downDigest := sha256.Sum256([]byte(canonicalDown))
	if hex.EncodeToString(downDigest[:]) != metadataDownChecksum {
		return nil, errors.New("first-delivery metadata rollback is not the frozen forward-only SQL")
	}
	ordered := slices.Clone(input)
	slices.SortFunc(ordered, func(a, b migrations.Migration) int {
		if a.Version < b.Version {
			return -1
		}
		if a.Version > b.Version {
			return 1
		}
		return 0
	})
	result := make([]migrations.Migration, 0, len(ordered))
	inserted := false
	for _, migration := range ordered {
		if migration.Version == metadataVersion {
			continue
		}
		if !inserted && migration.Version >= 34 {
			result = append(result, *seed)
			inserted = true
		}
		result = append(result, migration)
	}
	if !inserted {
		result = append(result, *seed)
	}
	return result, nil
}

// Up performs the metadata bridge inside the original migration transaction,
// including its ledger write. Failed SQL or restoration rolls everything back.
// These hooks install no future business endpoint, executor or capability.
func Up(ctx context.Context, connection *pgx.Conn, input []migrations.Migration) error {
	ordered, err := Ordered(input)
	if err != nil {
		return err
	}
	saved := make(map[int64][]roleEdge)
	bridged := make(map[int64]bool)
	hooks := migrations.UpHooks{
		BeforeApply: func(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
			if migration.Version != 27 && migration.Version != 28 && migration.Version != 33 {
				return nil
			}
			var mature bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=34) AND EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, metadataVersion).Scan(&mature); err != nil {
				return err
			}
			if !mature {
				return nil
			}
			if err := validateRetainedBridgeInput(ctx, tx, migration, input); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `LOCK TABLE permissions,role_permissions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				return err
			}
			var otherReferences bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE contype='f' AND confrelid='permissions'::regclass AND conrelid<>'role_permissions'::regclass)`).Scan(&otherReferences); err != nil {
				return err
			}
			if otherReferences {
				return errors.New("retained permission metadata has an unknown foreign-key consumer")
			}
			bridged[migration.Version] = true
			facts := retainedPermissions[migration.Version]
			for _, fact := range facts {
				var scope, description string
				if err := tx.QueryRow(ctx, `SELECT scope,description FROM permissions WHERE code=$1`, fact.code).Scan(&scope, &description); err != nil {
					return err
				}
				if scope != "PLATFORM" || description != fact.description {
					return errors.New("retained permission metadata changed before its original migration")
				}
				rows, err := tx.Query(ctx, `SELECT role_code,permission_code FROM role_permissions WHERE permission_code=$1 ORDER BY role_code`, fact.code)
				if err != nil {
					return err
				}
				for rows.Next() {
					var edge roleEdge
					if err := rows.Scan(&edge.role, &edge.permission); err != nil {
						rows.Close()
						return err
					}
					saved[migration.Version] = append(saved[migration.Version], edge)
				}
				rowsErr := rows.Err()
				rows.Close()
				if rowsErr != nil {
					return rowsErr
				}
				// Delete the sole known FK edges explicitly; do not rely on CASCADE.
				if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE permission_code=$1`, fact.code); err != nil {
					return err
				}
				command, err := tx.Exec(ctx, `DELETE FROM permissions WHERE code=$1`, fact.code)
				if err != nil {
					return err
				}
				if command.RowsAffected() != 1 {
					return errors.New("retained permission metadata was not uniquely removed")
				}
			}
			return nil
		},
		AfterApply: func(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
			if err := restoreExecutionFactsAfterBinding(ctx, tx, migration, ordered); err != nil {
				return err
			}
			if !bridged[migration.Version] {
				return nil
			}
			if migration.Version == 28 {
				return removeObsoleteReclaimMetadata(ctx, tx)
			}
			for _, fact := range retainedPermissions[migration.Version] {
				var scope, description string
				if err := tx.QueryRow(ctx, `SELECT scope,description FROM permissions WHERE code=$1`, fact.code).Scan(&scope, &description); err != nil {
					return err
				}
				if scope != "PLATFORM" || description != fact.description {
					return errors.New("original migration did not restore its retained permission facts")
				}
			}
			for _, edge := range saved[migration.Version] {
				if _, err := tx.Exec(ctx, `INSERT INTO role_permissions(role_code,permission_code) VALUES($1,$2) ON CONFLICT DO NOTHING`, edge.role, edge.permission); err != nil {
					return err
				}
			}
			return nil
		},
	}
	return migrations.UpWithHooks(ctx, connection, ordered, hooks)
}

func validateRetainedBridgeInput(ctx context.Context, tx pgx.Tx, migration migrations.Migration, input []migrations.Migration) error {
	expected, ok := bridgeChecksums[migration.Version]
	if !ok || migration.Checksum != expected {
		return fmt.Errorf("original migration %d is not the retained bridge input", migration.Version)
	}
	var seedChecksum string
	for _, item := range input {
		if item.Version == metadataVersion {
			seedChecksum = item.Checksum
		}
	}
	var installedSeed, installedScope string
	if err := tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, metadataVersion).Scan(&installedSeed); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=34`).Scan(&installedScope); err != nil {
		return err
	}
	if installedSeed != seedChecksum || !slices.Contains(retainedScopeChecksums, installedScope) {
		return errors.New("retained bridge migration provenance does not match")
	}
	var genuine bool
	if err := tx.QueryRow(ctx, `SELECT to_regprocedure('increment_authorization_scope_version(uuid,text,uuid)') IS NOT NULL AND EXISTS(SELECT 1 FROM roles WHERE code='PERSONAL_USER' AND scope='PERSONAL')`).Scan(&genuine); err != nil {
		return err
	}
	if !genuine {
		return errors.New("retained three-scope authority is incomplete")
	}
	return nil
}

func removeObsoleteReclaimMetadata(ctx context.Context, tx pgx.Tx) error {
	const obsolete = "organization.grant.reclaim_closed"
	var scope, description string
	if err := tx.QueryRow(ctx, `SELECT scope,description FROM permissions WHERE code=$1`, obsolete).Scan(&scope, &description); err != nil {
		return err
	}
	if scope != "ORGANIZATION" || description != "Reclaim an eligible grant isolated by irreversible account closure." {
		return errors.New("obsolete reclaim metadata differs from original migration")
	}
	rows, err := tx.Query(ctx, `SELECT role_code FROM role_permissions WHERE permission_code=$1 ORDER BY role_code`, obsolete)
	if err != nil {
		return err
	}
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			rows.Close()
			return err
		}
		roles = append(roles, role)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	if !slices.Equal(roles, []string{"BILLING_ADMIN", "ORG_ADMIN", "OWNER"}) {
		return errors.New("obsolete reclaim metadata has unexpected grants")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE permission_code=$1`, obsolete); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `DELETE FROM permissions WHERE code=$1`, obsolete)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return errors.New("obsolete reclaim metadata was not uniquely removed")
	}
	return nil
}
