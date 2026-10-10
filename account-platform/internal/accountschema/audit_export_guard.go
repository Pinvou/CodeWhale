package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"strings"
)

const auditExportUpChecksum = "d60a688ed9ee49014725543dbe458ac4386940033f09549877491f8f835be5ad"
const auditExportDownChecksum = "1d5b61eb28a6b09c8d716dde49ebdff203907331a3d226d878810bb1a8d9557e"

func validateAuditExportMigration(input []migrations.Migration) error {
	for _, m := range input {
		if m.Version != 32 {
			continue
		}
		digest := func(sql string) string {
			sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")))
			return hex.EncodeToString(sum[:])
		}
		if m.Checksum != auditExportUpChecksum || digest(m.UpSQL) != auditExportUpChecksum || digest(m.DownSQL) != auditExportDownChecksum {
			return errors.New("audit export SQL identity changed")
		}
	}
	return nil
}
func DownOne(ctx context.Context, conn *pgx.Conn, input []migrations.Migration) error {
	ordered, err := Ordered(input)
	if err != nil {
		return err
	}
	return migrations.DownOneWithGuard(ctx, conn, ordered, auditExportDownGuard)
}

func auditExportDownGuard(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if err := auditExportRollbackGuard(ctx, tx, m); err != nil {
		return err
	}
	if m.Version == 32 {
		// The frozen Down does not reconstruct the prior PII permission edge.
		// Later authorization migrations explicitly remove that edge too, so
		// granting it here would not be a proven inverse of retained history.
		return errors.New("audit export migration rollback is incomplete and forbidden")
	}
	return nil
}

func auditExportRollbackGuard(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != 32 {
		return nil
	}
	// Share-row-exclusive conflicts with inserts/deletes and serializes bindings
	// with the role removal; stale active or expired bindings remain evidence.
	if _, err := tx.Exec(ctx, `LOCK TABLE role_assignments IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_assignments WHERE role_code='PLATFORM_AUDIT_PII_EXPORT')`).Scan(&bound); err != nil {
		return err
	}
	if bound {
		return errors.New("audit PII role binding prevents rollback")
	}
	return nil
}
