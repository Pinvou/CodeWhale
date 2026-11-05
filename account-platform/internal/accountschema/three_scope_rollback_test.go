package accountschema

import (
	"context"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestThreeScopeRollbackIdentity(t *testing.T) {
	if err := retainedRollbackGuard(context.Background(), nil, migrations.Migration{Version: 34}); err == nil {
		t.Fatal("unpinned 034 accepted")
	}
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	for _, m := range all {
		if m.Version == 34 {
			original = m
		}
	}
	if original.Version != 34 {
		t.Fatal("original 034 absent")
	}
	for _, sourceCRLF := range []bool{false, true} {
		for _, ledger := range retainedScopeChecksums {
			m := original
			if sourceCRLF {
				m.UpSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.UpSQL), "\n", "\r\n")
				m.DownSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.DownSQL), "\n", "\r\n")
			}
			tx := &governanceRollbackTx{row: governanceRollbackRow{name: m.Name, checksum: ledger}}
			if err := retainedRollbackGuard(context.Background(), tx, m); err != nil {
				t.Fatal(err)
			}
			if err := applyAccountRollbackSQL(context.Background(), tx, m); err != nil || len(tx.executions) != 1 {
				t.Fatal(err)
			}
			sql := tx.executions[0]
			added := threeScopeInvalidationSQL + "\n\nDELETE FROM authorization_versions WHERE scope IN ('PERSONAL','UNIT');"
			if strings.Count(sql, added) != 1 || strings.Replace(sql, added, threeScopeInvalidationSQL, 1) != normalizedGovernanceSQL(original.DownSQL) {
				t.Fatal("changed more than post-invalidation cache cleanup")
			}
		}
	}
	for _, change := range []string{"version", "name", "checksum", "up", "down"} {
		t.Run(change, func(t *testing.T) {
			m := original
			switch change {
			case "version":
				m.Version++
			case "name":
				m.Name += "_drift"
			case "checksum":
				m.Checksum = strings.Repeat("0", 64)
			case "up":
				m.UpSQL += "\nSELECT 1;"
			case "down":
				m.DownSQL += "\nSELECT 1;"
			}
			if _, err := compatibleThreeScopeRollback(m); err == nil {
				t.Fatal("source drift accepted")
			}
			tx := &governanceRollbackTx{}
			if err := guardThreeScopeRollback(context.Background(), tx, m); err == nil || len(tx.queries) != 0 || len(tx.executions) != 0 {
				t.Fatal("drift reached database")
			}
		})
	}
	// The execution hook rechecks source identity even without the guard.
	m := original
	m.DownSQL += "\nSELECT 1;"
	tx := &governanceRollbackTx{}
	if err := applyAccountRollbackSQL(context.Background(), tx, m); err == nil || len(tx.executions) != 0 {
		t.Fatal("execution bypass accepted")
	}
}
