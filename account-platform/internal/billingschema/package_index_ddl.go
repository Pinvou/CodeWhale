package billingschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Frozen 065/077 revoke runtime CREATE before fresh-install 080 runs. Lend
// only the exact index migration's DDL privilege in its own transaction;
// already-applied migrations do not enter this hook and rollback returns it.
const packageIndexDDLUp = "768099650ccd0ea81262cbdd2ef1fa8ec273304f4995a144ae829e1401fe6f4d"
const packageIndexDDLDown = "b132cba123ea9e393bb0750e4ae4e97966aad347a7739a98c5a1abc5c4af94b0"

func validatePackageIndexDDL(m migrations.Migration) error {
	if m.Version != 80 || m.Name != "entitlement_components_package_index" || m.Checksum != packageIndexDDLUp || sqlChecksum(m.UpSQL) != packageIndexDDLUp || sqlChecksum(m.DownSQL) != packageIndexDDLDown {
		return errors.New("package index DDL loan requires original frozen migration")
	}
	return nil
}

func lendPackageIndexDDL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if err := validatePackageIndexDDL(m); err != nil {
		return err
	}
	if err := protectLegacyRefundMigration(ctx, tx, m); err != nil {
		return err
	}
	var permitted, alreadyCreate bool
	if err := tx.QueryRow(ctx, `SELECT session_user='billing_migrator' AND current_user='billing_service'
 AND (SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE oid=to_regnamespace('public'))='billing_schema_owner'
 AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER')
 , has_schema_privilege('billing_service','public','CREATE')`).Scan(&permitted, &alreadyCreate); err != nil {
		return err
	}
	if !permitted {
		return errors.New("package index DDL loan requires isolated deployment identity and runtime")
	}
	if alreadyCreate {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT set_config('pinvou.package_index_ddl_loan','true',true); SET ROLE billing_schema_owner; GRANT CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`)
	return err
}

func revokePackageIndexDDL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != 80 {
		return nil
	}
	if err := validatePackageIndexDDL(m); err != nil {
		return err
	}
	var borrowed bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('pinvou.package_index_ddl_loan',true),'')='true'`).Scan(&borrowed); err != nil {
		return err
	}
	if !borrowed {
		return nil
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM billing_service; SET ROLE billing_service`)
	return err
}
