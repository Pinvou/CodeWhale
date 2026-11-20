package accountschema

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
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
	connection          *pgx.Conn
	connectionCalls     int
	queries, executions []string
	row                 governanceRollbackRow
	err                 error
}

func (tx *governanceRollbackTx) Conn() *pgx.Conn {
	tx.connectionCalls++
	return tx.connection
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
	ledgerCtx, connection := governanceRollbackLedgerFixture(t)
	t.Run("exact-source-distinct-inputs-original-identity", func(t *testing.T) {
		tx := &governanceRollbackTx{connection: connection, row: governanceRollbackRow{name: original.Name, checksum: original.Checksum}}
		before := original
		if err := retainedRollbackGuard(ledgerCtx, tx, original); err != nil {
			t.Fatal(err)
		}
		if err := applyAccountRollbackSQL(context.Background(), tx, original); err != nil {
			t.Fatal(err)
		}
		if original != before || len(tx.queries) != 1 || !strings.Contains(tx.queries[0], "FOR UPDATE") || !strings.Contains(tx.queries[0], governanceRollbackLedgerTable) || len(tx.executions) != 1 {
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
			if err := guardGovernancePermissionsRollback(context.Background(), tx, m); err == nil || len(tx.queries) != 0 || len(tx.executions) != 0 || tx.connectionCalls != 0 {
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
			tx := &governanceRollbackTx{connection: connection, row: row}
			if err := retainedRollbackGuard(ledgerCtx, tx, original); err == nil || len(tx.executions) != 0 {
				t.Fatal("unrecognized database identity accepted")
			}
		})
	}
	t.Run("historical-crlf-source-and-ledger", func(t *testing.T) {
		m := original
		m.UpSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.UpSQL), "\n", "\r\n")
		m.DownSQL = strings.ReplaceAll(normalizedGovernanceSQL(m.DownSQL), "\n", "\r\n")
		tx := &governanceRollbackTx{connection: connection, row: governanceRollbackRow{name: m.Name, checksum: governanceSQLDigest(m.UpSQL)}}
		if err := retainedRollbackGuard(ledgerCtx, tx, m); err != nil {
			t.Fatal(err)
		}
		if err := applyAccountRollbackSQL(context.Background(), tx, m); err != nil || len(tx.executions) != 1 {
			t.Fatal("same-source CRLF rollback rejected", err)
		}
	})
	t.Run("storage-error-not-misclassified", func(t *testing.T) {
		failure := errors.New("controlled storage failure")
		tx := &governanceRollbackTx{connection: connection, row: governanceRollbackRow{err: failure}}
		if err := retainedRollbackGuard(ledgerCtx, tx, original); !errors.Is(err, failure) {
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

// A wire-only fixture supplies a real connection and the real PinLedger context.
// It does not model PostgreSQL catalog semantics; those have separate PG tests.
const governanceRollbackLedgerSchema = `governance "owned`
const governanceRollbackLedgerTable = `"governance ""owned"."schema_migrations"`

func governanceRollbackLedgerFixture(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	client, server := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	if err := server.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- serveGovernanceRollbackLedger(server)
	}()
	var connection *pgx.Conn
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if connection != nil {
			if err := connection.Close(ctx); err != nil {
				t.Error("close wire fixture connection:", err)
			}
		}
		client.Close()
		server.Close()
		if err := <-done; err != nil {
			t.Error("wire fixture protocol:", err)
		}
	})
	cfg, err := pgx.ParseConfig("host=127.0.0.1 user=fixture database=fixture sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) { return client, nil }
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	connection, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := migrations.PinLedger(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the real binding, but avoid exporting a cancelled setup deadline.
	return context.WithoutCancel(pinned), connection
}

func serveGovernanceRollbackLedger(server net.Conn) error {
	backend := pgproto3.NewBackend(server, server)
	message, err := backend.ReceiveStartupMessage()
	if err != nil {
		return err
	}
	if _, ok := message.(*pgproto3.StartupMessage); !ok {
		return fmt.Errorf("unexpected startup message %T", message)
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.11"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return err
	}
	message, err = backend.Receive()
	if err != nil {
		return err
	}
	query, ok := message.(*pgproto3.Query)
	if !ok || !strings.Contains(query.String, "WITH permanent_path AS") {
		return fmt.Errorf("expected real PinLedger query, got %T", message)
	}
	backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("schema"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1}}})
	backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(governanceRollbackLedgerSchema)}})
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return err
	}
	message, err = backend.Receive()
	if err != nil {
		return err
	}
	if _, ok := message.(*pgproto3.Terminate); !ok {
		return fmt.Errorf("unexpected additional connection query %T", message)
	}
	return nil
}

func TestOrganizationGovernanceRollbackLedgerBinding(t *testing.T) {
	ctx, connection := governanceRollbackLedgerFixture(t)
	original := originalGovernanceRollbackFixture(t)
	t.Run("real-binding-qualifies-original-ledger", func(t *testing.T) {
		tx := &governanceRollbackTx{connection: connection, row: governanceRollbackRow{name: original.Name, checksum: original.Checksum}}
		if err := retainedRollbackGuard(ctx, tx, original); err != nil {
			t.Fatal(err)
		}
		if tx.connectionCalls != 1 || len(tx.queries) != 1 || !strings.Contains(tx.queries[0], governanceRollbackLedgerTable) {
			t.Fatal("guard did not use the real bound connection and quoted ledger")
		}
	})
	t.Run("another-connection-rejected-before-query", func(t *testing.T) {
		tx := &governanceRollbackTx{connection: &pgx.Conn{}, row: governanceRollbackRow{name: original.Name, checksum: original.Checksum}}
		err := retainedRollbackGuard(ctx, tx, original)
		if err == nil || !strings.Contains(err.Error(), "belongs to another connection") || tx.connectionCalls != 1 || len(tx.queries) != 0 || len(tx.executions) != 0 {
			t.Fatal("foreign connection acquired ledger authority", err)
		}
	})
}
