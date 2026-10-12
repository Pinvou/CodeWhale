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

const executionGuardVersion int64 = 900002
const executionGuardChecksum = "019fd548cf79e833fd53ef60591a4f714a40268dceb8d1237c53e69b0162215a"
const executionGuardDownChecksum = "3defe5329273a8dd6a56e026e39f44616b63e1e0416fd20e688f3f5ca6fda21d"

func validateExecutionGuard(input []migrations.Migration) error {
	required := false
	var guard *migrations.Migration
	for _, item := range input {
		if item.Version == 53 {
			required = true
		}
		if item.Version == executionGuardVersion {
			if guard != nil {
				return errors.New("duplicate execution facts guard")
			}
			copy := item
			guard = &copy
		}
	}
	if guard == nil {
		if required {
			return errors.New("execution facts guard migration is required")
		}
		return nil
	}
	if !required {
		return errors.New("execution facts guard requires original authorization binding migration")
	}
	digest := func(source string) string {
		source = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
		sum := sha256.Sum256([]byte(source))
		return hex.EncodeToString(sum[:])
	}
	if guard.Checksum != executionGuardChecksum || digest(guard.UpSQL) != executionGuardChecksum || digest(guard.DownSQL) != executionGuardDownChecksum {
		return errors.New("execution facts guard is not the frozen forward-only SQL")
	}
	return nil
}

// Original 053 replaces the execution trigger function. Restore the already
// frozen guard before that migration can commit, rather than leaving a window
// until 900002's separate transaction. Its ledger identity is still recorded
// only when the migration engine later applies original 900002 normally.
func restoreExecutionFactsAfterBinding(ctx context.Context, tx pgx.Tx, migration migrations.Migration, input []migrations.Migration) error {
	if migration.Version != 53 {
		return nil
	}
	if err := validateExecutionGuard(input); err != nil {
		return err
	}
	for _, guard := range input {
		if guard.Version == executionGuardVersion {
			_, err := tx.Exec(ctx, guard.UpSQL)
			return err
		}
	}
	return errors.New("execution facts guard migration is required")
}
