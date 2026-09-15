package account

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalAuthorizationBackfillBoundsIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	// Exercise the granted SQL interface itself: a Go int parameter would hide
	// SQL NULL, whose LIMIT semantics otherwise remove the bound entirely.
	for _, limit := range []any{nil, 0, -1, 201} {
		rows, err := db.backfillPool.Query(ctx, `SELECT * FROM account_approval_authorization.authorization_backfill_candidates(NULL,NULL,$1,$2,$3)`, time.Now().UTC(), uuid.New(), limit)
		if err == nil {
			for rows.Next() {
				t.Error("invalid limit returned a candidate")
			}
			err = rows.Err()
			rows.Close()
		}
		requireControlSQLState(t, err, "22023")
	}
}
