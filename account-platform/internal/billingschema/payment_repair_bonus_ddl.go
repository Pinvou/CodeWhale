package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Original 065/077 return runtime CREATE to the schema owner. Original 079,
// installed on a fresh F12 database afterwards, still creates bonus tables.
// Lend only the original frozen migration's schema privilege in its own tx;
// rollback also rolls back the loan. Existing applied 079 is never re-run.
const bonusDDLUp = "27802b2d3e76e6a42c809d620d3d9833cfaa529d7d5517982318236a56719deb"
const bonusDDLDown = "56c6319eb573db51e60cb8e8a1a91173b882b4a025c7d6a941f0b01833f027c6"

func protectBillingMigrationAndLendBonusDDL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if err := protectLegacyRefundMigration(ctx, tx, m); err != nil {
		return err
	}
	if m.Version != 79 {
		return nil
	}
	if m.Name != "registration_bonus" || m.Checksum != bonusDDLUp || sqlChecksum(m.UpSQL) != bonusDDLUp || sqlChecksum(m.DownSQL) != bonusDDLDown {
		return errors.New("bonus DDL loan requires original frozen migration")
	}
	var permitted bool
	if err := tx.QueryRow(ctx, `SELECT session_user='billing_migrator' AND current_user='billing_service'
 AND (SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE oid=to_regnamespace('public'))='billing_schema_owner'
 AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER')
 AND NOT has_schema_privilege('billing_service','public','CREATE')`).Scan(&permitted); err != nil {
		return err
	}
	if !permitted {
		return errors.New("bonus DDL loan requires isolated deployment identity and runtime")
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; GRANT CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`)
	return err
}
func revokeBonusDDL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != 79 {
		return nil
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM billing_service; SET ROLE billing_service`)
	return err
}
