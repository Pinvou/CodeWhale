package accountschema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func originalGovernanceRollbackFixture(t *testing.T) migrations.Migration {
	t.Helper()
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version == governancePermissionsVersion {
			return m
		}
	}
	t.Fatal("original organization governance migration absent")
	return migrations.Migration{}
}

type governanceRollbackRow struct {
	name, checksum string
	err            error
}

func (r governanceRollbackRow) Scan(values ...any) error {
	if r.err != nil {
		return r.err
	}
	*values[0].(*string), *values[1].(*string) = r.name, r.checksum
	return nil
}

type governanceRollbackTx struct {
	pgx.Tx
	queries, executions []string
	row                 governanceRollbackRow
	err                 error
}

func (tx *governanceRollbackTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	tx.queries = append(tx.queries, sql)
	return tx.row
}
func (tx *governanceRollbackTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	tx.executions = append(tx.executions, sql)
	return pgconn.CommandTag{}, tx.err
}

func TestOrganizationGovernanceRollbackCompatibility(t *testing.T) {
	original := originalGovernanceRollbackFixture(t)
	t.Run("exact-source-distinct-inputs-original-identity", func(t *testing.T) {
		tx := &governanceRollbackTx{row: governanceRollbackRow{name: original.Name, checksum: original.Checksum}}
		before := original
		if err := retainedRollbackGuard(context.Background(), tx, original); err != nil {
			t.Fatal(err)
		}
		if err := applyAccountRollbackSQL(context.Background(), tx, original); err != nil {
			t.Fatal(err)
		}
		if original != before || len(tx.queries) != 1 || !strings.Contains(tx.queries[0], "FOR UPDATE") || len(tx.executions) != 1 {
			t.Fatal("source identity or guarded execution changed")
		}
		sql := tx.executions[0]
		if strings.Contains(sql, "SELECT DISTINCT bump_role_assignment_scope_version") || strings.Count(sql, compatibleGovernanceInvalidationSQL) != 1 || strings.Replace(sql, compatibleGovernanceInvalidationSQL, originalGovernanceInvalidationSQL, 1) != normalizedGovernanceSQL(original.DownSQL) {
			t.Fatal("compatibility changed more than the void DISTINCT statement")
		}
		if !strings.Contains(normalizedGovernanceSQL(original.UpSQL), compatibleGovernanceInvalidationSQL) {
			t.Fatal("compatibility no longer uses the exact original Up input tuple query")
		}
	})
	for _, change := range []string{"name", "checksum", "up", "down", "version"} {
		t.Run("changed-source-"+change, func(t *testing.T) {
			m := original
			switch change {
			case "name":
				m.Name += "_changed"
			case "checksum":
				m.Checksum = strings.Repeat("0", 64)
			case "up":
				m.UpSQL += "\nSELECT 1;"
			case "down":
				m.DownSQL += "\nSELECT 1;"
			case "version":
				m.Version++
			}
			tx := &governanceRollbackTx{}
			if err := guardGovernancePermissionsRollback(context.Background(), tx, m); err == nil || len(tx.queries) != 0 || len(tx.executions) != 0 {
				t.Fatal("unknown source reached database")
			}
		})
	}
	for _, which := range []string{"name", "checksum"} {
		t.Run("changed-applied-"+which, func(t *testing.T) {
			row := governanceRollbackRow{name: original.Name, checksum: original.Checksum}
			if which == "name" {
				row.name += "_changed"
			} else {
				row.checksum = strings.Repeat("0", 64)
			}
			tx := &governanceRollbackTx{row: row}
			if err := retainedRollbackGuard(context.Background(), tx, original); err == nil || len(tx.executions) != 0 {
				t.Fatal("unrecognized database identity accepted")
			}
		})
	}
	t.Run("historical-crlf-source-and-ledger", func(t *testing.T) {
		m := original
		m.UpSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.UpSQL), "\n", "\r\n")
		m.DownSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.DownSQL), "\n", "\r\n")
		tx := &governanceRollbackTx{row: governanceRollbackRow{name: m.Name, checksum: governanceSQLDigest(m.UpSQL)}}
		if err := retainedRollbackGuard(context.Background(), tx, m); err != nil {
			t.Fatal(err)
		}
		if err := applyAccountRollbackSQL(context.Background(), tx, m); err != nil || len(tx.executions) != 1 {
			t.Fatal("same-source CRLF rollback rejected", err)
		}
	})
	t.Run("storage-error-not-misclassified", func(t *testing.T) {
		failure := errors.New("controlled storage failure")
		tx := &governanceRollbackTx{row: governanceRollbackRow{err: failure}}
		if err := retainedRollbackGuard(context.Background(), tx, original); !errors.Is(err, failure) {
			t.Fatal("ledger read failure replaced", err)
		}
		tx = &governanceRollbackTx{err: failure}
		if err := applyAccountRollbackSQL(context.Background(), tx, original); !errors.Is(err, failure) {
			t.Fatal("execution failure replaced", err)
		}
	})
	t.Run("unrelated-sql-byte-identical", func(t *testing.T) {
		m := migrations.Migration{Version: 31, DownSQL: "SELECT 'untouched';\r\n"}
		tx := &governanceRollbackTx{}
		if err := applyAccountRollbackSQL(context.Background(), tx, m); err != nil || len(tx.executions) != 1 || tx.executions[0] != m.DownSQL {
			t.Fatal("unrelated Down SQL rewritten", err)
		}
	})
}
