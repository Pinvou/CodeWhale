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
	if migration.Version == 39 {
		// Preserve the retained migration identity. Its original DISTINCT
		// expression compares void scope-version results and fails at planning,
		// including an empty role-assignment set. Reject before any Down SQL.
		return errors.New("retained organization governance rollback is unsupported: original scope-version SQL cannot compare void results")
	}
	if migration.Version == 33 {
		// Original 018 protects policies with a statement-level immutable
		// trigger, including DELETE statements that match no policy rows.
		return errors.New("payment provider resume policy rollback would delete append-only policy history")
	}
	switch migration.Version {
	case 24, 25, 26:
		// These original DELETE statements are also rejected by 018's
		// statement-level trigger, even when no command policy matches.
		return errors.New("Account command policy rollback would delete append-only policy history")
	}
	return nil
}
