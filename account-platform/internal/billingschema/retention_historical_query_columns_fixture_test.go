package billingschema

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
)

// This oracle belongs only to an explicit pre-900047 -> complete upgrade.
// Ordinary current-writer snapshots keep all fields, including both new facts.
type retentionHistoricalQueryColumnUpgrade struct {
	columns, authority, orders, ledger string
	versions                           []int64
}

const retentionHistoricalPaymentOrderColumns = `SELECT jsonb_agg(jsonb_build_array(
 a.attnum,a.attname,a.atttypid,a.atttypmod,a.attnotnull,a.attcollation,a.attidentity,a.attgenerated,
 a.attacl::text,pg_get_expr(d.adbin,d.adrelid)) ORDER BY a.attnum)::text
 FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
 WHERE a.attrelid='public.payment_orders'::regclass AND a.attnum>0 AND NOT a.attisdropped
 AND a.attname NOT IN ('archive_query_generation','archive_query_authoritative_unpaid')`

const retentionHistoricalPaymentOrderAuthority = `SELECT jsonb_build_array(
 c.oid,pg_get_userbyid(c.relowner),c.relacl::text,
 has_table_privilege('billing_service',c.oid,'SELECT'),has_table_privilege('billing_service',c.oid,'INSERT'),
 has_table_privilege('billing_service',c.oid,'UPDATE'),has_table_privilege('billing_service',c.oid,'DELETE'),
 has_table_privilege('billing_service',c.oid,'TRUNCATE'),pg_has_role('billing_service','billing_schema_owner','MEMBER'),
 has_schema_privilege('billing_service','public','CREATE'))::text
 FROM pg_class c WHERE c.oid='public.payment_orders'::regclass`

const retentionHistoricalPaymentOrderRows = `SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]'::jsonb)::text FROM public.payment_orders p`

func freezeRetentionHistoricalQueryUpgrade(t *testing.T, ctx context.Context, connection *pgx.Conn) retentionHistoricalQueryColumnUpgrade {
	t.Helper()
	var frozen retentionHistoricalQueryColumnUpgrade
	var absent bool
	if err := connection.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='public.payment_orders'::regclass AND attname IN ('archive_query_generation','archive_query_authoritative_unpaid') AND NOT attisdropped)`).Scan(&absent); err != nil || !absent {
		t.Fatal("historical fixture must actually predate both current-query columns", err)
	}
	for query, target := range map[string]*string{retentionHistoricalPaymentOrderColumns: &frozen.columns, retentionHistoricalPaymentOrderAuthority: &frozen.authority, retentionHistoricalPaymentOrderRows: &frozen.orders, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`: &frozen.ledger} {
		if err := connection.QueryRow(ctx, query).Scan(target); err != nil {
			t.Fatal("capture complete historical query upgrade contract", err)
		}
	}
	if err := connection.QueryRow(ctx, `SELECT array_agg(version ORDER BY version) FROM schema_migrations`).Scan(&frozen.versions); err != nil {
		t.Fatal(err)
	}
	return frozen
}

func (f retentionHistoricalQueryColumnUpgrade) assertAfter(t *testing.T, ctx context.Context, connection *pgx.Conn) {
	t.Helper()
	var columns, authority, orders, ledger string
	for query, target := range map[string]*string{retentionHistoricalPaymentOrderColumns: &columns, retentionHistoricalPaymentOrderAuthority: &authority, retentionHistoricalPaymentOrderRows: &orders} {
		if err := connection.QueryRow(ctx, query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version=ANY($1::bigint[])`, f.versions).Scan(&ledger); err != nil || ledger != f.ledger || columns != f.columns || authority != f.authority {
		t.Fatal("upgrade changed an original ledger row, column attribute or payment owner/ACL", err)
	}
	var newColumns bool
	if err := connection.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(
 NOT a.attnotnull AND a.attacl IS NULL AND a.attidentity='' AND a.attgenerated='' AND d.oid IS NULL
 AND ((a.attname='archive_query_generation' AND a.atttypid='uuid'::regtype)
   OR (a.attname='archive_query_authoritative_unpaid' AND a.atttypid='boolean'::regtype))
 AND has_column_privilege('billing_service',a.attrelid,a.attname,'SELECT')
 AND has_column_privilege('billing_service',a.attrelid,a.attname,'INSERT')
 AND has_column_privilege('billing_service',a.attrelid,a.attname,'UPDATE'))
 FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
 WHERE a.attrelid='public.payment_orders'::regclass AND a.attname IN ('archive_query_generation','archive_query_authoritative_unpaid') AND NOT a.attisdropped`).Scan(&newColumns); err != nil || !newColumns {
		t.Fatal("complete upgrade did not install the exact nullable current-query shape and inherited runtime rights", err)
	}
	assertRetentionHistoricalQueryOrderRows(t, []byte(f.orders), []byte(orders))
}

// Compare every original row/key/value. Only these two named new keys may be
// added, and each must explicitly exist as JSON null, never guessed/backfilled.
func assertRetentionHistoricalQueryOrderRows(t *testing.T, before, after []byte) {
	t.Helper()
	var oldRows, newRows []map[string]json.RawMessage
	if json.Unmarshal(before, &oldRows) != nil || json.Unmarshal(after, &newRows) != nil || len(oldRows) != len(newRows) {
		t.Fatal("historical payment row set changed")
	}
	for n, row := range newRows {
		for _, key := range []string{"archive_query_generation", "archive_query_authoritative_unpaid"} {
			if _, existed := oldRows[n][key]; existed || !bytes.Equal(row[key], []byte("null")) {
				t.Fatal("historical current-query authority was missing or fabricated")
			}
			delete(row, key)
		}
		old, _ := json.Marshal(oldRows[n])
		current, _ := json.Marshal(row)
		if !bytes.Equal(old, current) {
			t.Fatal("upgrade changed an original payment field or row identity")
		}
	}
}

func assertRetentionHistoricalQueryFinancialSnapshot(t *testing.T, before, after, ordersKey string) {
	t.Helper()
	var old, current map[string]json.RawMessage
	if json.Unmarshal([]byte(before), &old) != nil || json.Unmarshal([]byte(after), &current) != nil || old[ordersKey] == nil || current[ordersKey] == nil {
		t.Fatal("historical financial snapshot shape unavailable")
	}
	assertRetentionHistoricalQueryOrderRows(t, old[ordersKey], current[ordersKey])
	// The order rows have their independent full-field oracle above. No nested
	// evidence/receipt JSON, any other table, numeric value or array is excluded.
	delete(old, ordersKey)
	delete(current, ordersKey)
	a, _ := json.Marshal(old)
	b, _ := json.Marshal(current)
	if !bytes.Equal(a, b) {
		t.Fatal("complete upgrade changed other original financial, receipt or review facts")
	}
}
