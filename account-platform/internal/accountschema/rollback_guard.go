package accountschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// DownOne retains the frozen package checks before selecting an applied
// version. Legacy databases may still have a low-numbered latest version.
func DownOne(ctx context.Context, conn *pgx.Conn, input []migrations.Migration) error {
	ordered, err := Ordered(input)
	if err != nil {
		return err
	}
	return migrations.DownOneWithGuard(ctx, conn, ordered, retainedRollbackGuard)
}

func retainedRollbackGuard(_ context.Context, _ pgx.Tx, migration migrations.Migration) error {
	if migration.Version == 33 {
		// Original 018 protects policies with a statement-level immutable
		// trigger, including DELETE statements that match no policy rows.
		return errors.New("payment provider resume policy rollback would delete append-only policy history")
	}
	return nil
}
