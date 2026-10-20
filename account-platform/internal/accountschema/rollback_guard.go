package accountschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// retainedRollbackGuard composes frozen closure, governance and append-only policy checks.
func retainedRollbackGuard(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	if migration.Version == 28 {
		// Keep the original SQL identity. Even a legacy database whose latest
		// applied version is 028 must retain cleanup proofs, lifecycle fences
		// and retention records; a later forward-only migration is not enough.
		return errors.New("retained account closure cleanup rollback is unsupported: preserve lifecycle, cleanup and retention evidence")
	}
	if migration.Version == governancePermissionsVersion {
		return guardGovernancePermissionsRollback(ctx, tx, migration)
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
