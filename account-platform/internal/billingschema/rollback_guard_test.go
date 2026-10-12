package billingschema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type rollbackGuardTx struct {
	pgx.Tx
	queries           []string
	locks             []string
	present           bool
	lockErr, queryErr error
}
type rollbackGuardRow struct {
	present bool
	err     error
}

func (r rollbackGuardRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*bool) = r.present
	return nil
}
func (tx *rollbackGuardTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	tx.locks = append(tx.locks, sql)
	return pgconn.CommandTag{}, tx.lockErr
}
func (tx *rollbackGuardTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	tx.queries = append(tx.queries, sql)
	return rollbackGuardRow{tx.present, tx.queryErr}
}
func TestRetainedBillingRollbackGuard(t *testing.T) {
	t.Run("incomplete-journal-enum", func(t *testing.T) {
		if err := retainedRollbackGuard(context.Background(), nil, migrations.Migration{Version: 25}); err == nil {
			t.Fatal("incomplete original enum restoration allowed")
		}
	})
	for _, version := range []int64{23, 29} {
		name := map[int64]string{23: "catalog", 29: "anomaly"}[version]
		t.Run(name, func(t *testing.T) {
			for _, present := range []bool{false, true} {
				tx := &rollbackGuardTx{present: present}
				err := retainedRollbackGuard(context.Background(), tx, migrations.Migration{Version: version})
				if (err != nil) != present {
					t.Fatalf("guard result does not match actual unsafe state %v: %v", present, err)
				}
				if len(tx.locks) != 1 || !strings.Contains(tx.locks[0], "SHARE ROW EXCLUSIVE") || len(tx.queries) != 1 {
					t.Fatal("guard did not lock both inputs before checking")
				}
			}
			for _, duringLock := range []bool{false, true} {
				failure := errors.New("controlled database error")
				tx := &rollbackGuardTx{}
				if duringLock {
					tx.lockErr = failure
				} else {
					tx.queryErr = failure
				}
				if err := retainedRollbackGuard(context.Background(), tx, migrations.Migration{Version: version}); !errors.Is(err, failure) {
					t.Fatal("database failure was swallowed")
				}
				if duringLock && len(tx.queries) != 0 {
					t.Fatal("query ran after lock failure")
				}
			}
		})
	}
	t.Run("unrelated-history", func(t *testing.T) {
		for _, v := range []int64{10, 24, 26, 28, 900001, 900002, 900003} {
			if err := retainedRollbackGuard(context.Background(), nil, migrations.Migration{Version: v}); err != nil {
				t.Fatalf("unrelated migration %d blocked", v)
			}
		}
	})
}
