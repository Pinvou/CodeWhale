package accountschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// retainedRollbackGuard retains the frozen package checks for append-only
// policy history. Legacy databases may still have a low-numbered latest
// version; DownOne composes this guard with the audit-export rollback guard.
func retainedRollbackGuard(_ context.Context, _ pgx.Tx, migration migrations.Migration) error {
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
