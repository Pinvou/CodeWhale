package migrations

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type ledgerContextKey struct{}

type ledgerBinding struct {
	connection *pgx.Conn
	table      string
}

// PinLedger captures the currently visible migration ledger's namespace. When
// no ledger exists it captures current_schema without creating anything. Share
// the returned context across a migration workflow so later search_path changes
// cannot redirect bookkeeping. A separate workflow may select another schema.
func PinLedger(ctx context.Context, connection *pgx.Conn) (context.Context, error) {
	if binding, ok := ctx.Value(ledgerContextKey{}).(ledgerBinding); ok {
		if binding.connection != connection {
			return nil, errors.New("migration ledger context belongs to another connection")
		}
		return ctx, nil
	}
	var schema *string
	err := connection.QueryRow(ctx, `SELECT COALESCE(
 (SELECT n.nspname FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  WHERE c.oid=pg_catalog.to_regclass('schema_migrations')),
 pg_catalog.current_schema())`).Scan(&schema)
	if err != nil {
		return nil, fmt.Errorf("resolve migration ledger namespace: %w", err)
	}
	if schema == nil || *schema == "" {
		return nil, errors.New("migration ledger requires an existing schema in search_path")
	}
	return context.WithValue(ctx, ledgerContextKey{}, ledgerBinding{
		connection: connection,
		table:      pgx.Identifier{*schema, "schema_migrations"}.Sanitize(),
	}), nil
}

// LedgerTable returns a safely quoted, schema-qualified identifier for ledger
// queries. Call PinLedger once and retain its context across related operations.
// This does not qualify the business objects referenced by migration SQL/hooks.
func LedgerTable(ctx context.Context, connection *pgx.Conn) (string, error) {
	pinned, err := PinLedger(ctx, connection)
	if err != nil {
		return "", err
	}
	return pinned.Value(ledgerContextKey{}).(ledgerBinding).table, nil
}
