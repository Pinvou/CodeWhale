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

// PinLedger captures the first permanent ledger in the effective search path.
// When none exists it captures the first non-temporary creation schema without
// creating anything. Temporary tables cannot supply migration history. Share
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
	err := connection.QueryRow(ctx, `WITH permanent_path AS (
 SELECT n.oid,n.nspname,path.ordinal,
        n.nspname=ANY(pg_catalog.current_schemas(false)) AS creation_schema
 FROM pg_catalog.unnest(pg_catalog.current_schemas(true)) WITH ORDINALITY AS path(name,ordinal)
 JOIN pg_catalog.pg_namespace n ON n.nspname=path.name
 WHERE n.oid<>pg_catalog.pg_my_temp_schema()
   AND NOT pg_catalog.pg_is_other_temp_schema(n.oid)
)
SELECT COALESCE(
 (SELECT path.nspname FROM permanent_path path
  JOIN pg_catalog.pg_class c ON c.relnamespace=path.oid AND c.relname='schema_migrations'
  ORDER BY path.ordinal LIMIT 1),
 (SELECT nspname FROM permanent_path WHERE creation_schema ORDER BY ordinal LIMIT 1))`).Scan(&schema)
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
