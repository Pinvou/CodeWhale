package accountschema

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetainedPolicyRollbackGuard(t *testing.T) {
	for _, version := range []int64{24, 25, 26, 33} {
		t.Run(fmt.Sprintf("policy-%d", version), func(t *testing.T) {
			if err := retainedRollbackGuard(context.Background(), nil, migrations.Migration{Version: version}); err == nil || !strings.Contains(err.Error(), "append-only policy history") {
				t.Fatalf("statement-level immutable policy deletion was allowed: %v", err)
			}
		})
	}
	for _, version := range []int64{18, 31, 32, 34, 41, 44, 900001, 900002} {
		if err := retainedRollbackGuard(context.Background(), nil, migrations.Migration{Version: version}); err != nil {
			t.Fatalf("unrelated migration %d was blocked: %v", version, err)
		}
	}
}

type executionGuardTx struct {
	pgx.Tx
	sql []string
	err error
}

func (tx *executionGuardTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	tx.sql = append(tx.sql, sql)
	return pgconn.CommandTag{}, tx.err
}

func TestExecutionFactsRestoredBeforeOriginal53Commit(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var guard migrations.Migration
	for _, m := range loaded {
		if m.Version == executionGuardVersion {
			guard = m
		}
	}
	t.Run("exact-frozen-source-without-ledger-write", func(t *testing.T) {
		tx := &executionGuardTx{}
		if err := restoreExecutionFactsAfterBinding(context.Background(), tx, migrations.Migration{Version: 53}, loaded); err != nil {
			t.Fatal(err)
		}
		if len(tx.sql) != 1 || tx.sql[0] != guard.UpSQL {
			t.Fatal("bridge did not execute only original frozen guard SQL")
		}
	})
	t.Run("invalid-pin-fails-before-execution", func(t *testing.T) {
		tx := &executionGuardTx{}
		changed := append([]migrations.Migration(nil), loaded...)
		for i := range changed {
			if changed[i].Version == executionGuardVersion {
				changed[i].UpSQL += "\nSELECT 1;"
			}
		}
		if err := restoreExecutionFactsAfterBinding(context.Background(), tx, migrations.Migration{Version: 53}, changed); err == nil || len(tx.sql) != 0 {
			t.Fatal("changed guard source reached execution")
		}
	})
	t.Run("execution-failure-propagates", func(t *testing.T) {
		failure := errors.New("controlled hook failure")
		tx := &executionGuardTx{err: failure}
		if err := restoreExecutionFactsAfterBinding(context.Background(), tx, migrations.Migration{Version: 53}, loaded); !errors.Is(err, failure) {
			t.Fatal("hook failure was swallowed")
		}
	})
	t.Run("other-migrations-untouched", func(t *testing.T) {
		if err := restoreExecutionFactsAfterBinding(context.Background(), nil, migrations.Migration{Version: 52}, nil); err != nil {
			t.Fatal(err)
		}
	})
}
