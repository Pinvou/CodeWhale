package billingschema

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Original later migrations are read-only fixtures. They are not installed as
// first-delivery product files or exposed as executable future capabilities.
// This diagnostic covers the original metadata bridge through version 49;
// later privileged migrations require their own isolated deployment fixture.
func TestSparseMetadataUpgradeMatchesOriginalThrough49(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	originalDirectory := os.Getenv("PINVOU_TEST_ACCOUNT_ORIGINAL_MIGRATIONS_DIR")
	if raw == "" || originalDirectory == "" {
		t.Skip("actual PostgreSQL and original migration fixture directory required")
	}
	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	defer stop()
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	original, err := migrations.Load(originalDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var target []migrations.Migration
	for _, migration := range original {
		if migration.Version <= 49 {
			target = append(target, migration)
		}
	}
	for _, migration := range installed {
		// The authorized unpublished ordinary-Outbox correction is the same
		// target in both paths. All other original SQL bytes are retained.
		if migration.Version == 3 {
			for i := range target {
				if target[i].Version == 3 {
					target[i] = migration
				}
			}
		} else if migration.Version > 49 {
			target = append(target, migration)
		}
	}
	f := newStageFixture(t, ctx, raw)
	connections := []*pgx.Conn{f.database(t, ctx, "account", "account_service"), f.database(t, ctx, "account", "account_service")}
	// Independent databases preserve the real public extension namespace and
	// prevent one fixture's functions from satisfying another's dependencies.
	connection := connections[0]
	if err := accountschema.Up(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	// Observe retained authority edges and raw assignment rows before bridging.
	edgesBefore := permissionEdges(t, ctx, connection)
	if err := accountschema.Up(ctx, connection, target); err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.VerifyApplied(ctx, connection, target); err != nil {
		t.Fatal(err)
	}
	sparseEdges := permissionEdges(t, ctx, connection)
	sparseShape := schemaShape(t, ctx, connection)
	for _, edge := range edgesBefore {
		if !slices.Contains(sparseEdges, edge) {
			t.Fatalf("sparse upgrade removed a retained authority edge: %s", edge)
		}
	}
	var legacyCount int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM permissions WHERE code='organization.grant.reclaim_closed'`).Scan(&legacyCount); err != nil || legacyCount != 0 {
		t.Fatalf("obsolete metadata restored by delayed original 028: count=%d err=%v", legacyCount, err)
	}
	connection = connections[1]
	if err := accountschema.Up(ctx, connection, target); err != nil {
		t.Fatal(err)
	}
	freshEdges := permissionEdges(t, ctx, connection)
	if !slices.Equal(sparseEdges, freshEdges) {
		t.Fatalf("sparse/fresh permission edges differ: sparse=%d fresh=%d", len(sparseEdges), len(freshEdges))
	}
	if freshShape := schemaShape(t, ctx, connection); freshShape != sparseShape {
		for _, fact := range strings.Split(sparseShape, "\n") {
			if !slices.Contains(strings.Split(freshShape, "\n"), fact) {
				t.Logf("sparse-only schema fact: %s", fact)
			}
		}
		for _, fact := range strings.Split(freshShape, "\n") {
			if !slices.Contains(strings.Split(sparseShape, "\n"), fact) {
				t.Logf("fresh-only schema fact: %s", fact)
			}
		}
		t.Fatal("sparse/fresh columns, defaults, constraints, indexes, triggers, functions, ownership or ACLs differ")
	}
}

func schemaShape(t *testing.T, ctx context.Context, connection *pgx.Conn) string {
	t.Helper()
	var shape string
	err := connection.QueryRow(ctx, `
WITH facts(value) AS (
 SELECT jsonb_build_array('relation',c.relname,c.relkind::text,pg_get_userbyid(c.relowner),c.relacl)::text
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'
 UNION ALL
 SELECT jsonb_build_array('column',c.relname,a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,a.attidentity::text,a.attgenerated::text,pg_get_expr(d.adbin,d.adrelid))::text
 FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
 WHERE n.nspname='public' AND a.attnum>0 AND NOT a.attisdropped
 UNION ALL
 SELECT jsonb_build_array('constraint',c.relname,k.conname,k.convalidated,pg_get_constraintdef(k.oid))::text
 FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'
 UNION ALL
 SELECT jsonb_build_array('index',c.relname,pg_get_indexdef(i.indexrelid),i.indisvalid,i.indisready)::text
 FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'
 UNION ALL
 SELECT jsonb_build_array('trigger',c.relname,g.tgname,g.tgenabled::text,pg_get_triggerdef(g.oid))::text
 FROM pg_trigger g JOIN pg_class c ON c.oid=g.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT g.tgisinternal
 UNION ALL
 SELECT jsonb_build_array('function',p.proname,pg_get_functiondef(p.oid),pg_get_userbyid(p.proowner),p.proacl)::text
 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
)
SELECT string_agg(value,E'\n' ORDER BY value) FROM facts`).Scan(&shape)
	if err != nil {
		t.Fatal(err)
	}
	return shape
}

func permissionEdges(t *testing.T, ctx context.Context, connection *pgx.Conn) []string {
	t.Helper()
	rows, err := connection.Query(ctx, `SELECT role_code||':'||permission_code FROM role_permissions ORDER BY role_code,permission_code`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var edges []string
	for rows.Next() {
		var edge string
		if err := rows.Scan(&edge); err != nil {
			t.Fatal(err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return edges
}
