package tokenschema

import (
	"context"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Ordered freezes both SQL directions before the public runner executes them.
// Only the new reversible statistics index uses a trusted ledger identity;
// earlier migration behavior and runtime ledger privileges stay unchanged.
func DownOne(ctx context.Context, c *pgx.Conn, loaded []migrations.Migration) error {
	ordered, err := Ordered(loaded)
	if err != nil {
		return err
	}
	return migrations.DownOneWithHooks(ctx, c, ordered, migrations.DownHooks{
		LedgerRole: func(m migrations.Migration) string {
			if m.Version == 900010 {
				return "token_schema_owner"
			}
			return ""
		},
	})
}
