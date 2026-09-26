// Package tokenmigrations contains the narrowly scoped compatibility bridge
// needed to apply the immutable Token migration history with the current
// least-privilege role model.
package tokenmigrations

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const (
	legacyOverviewIndexVersion = int64(33)
	legacyOverviewIndexName    = "model_token_page_overview_indexes"
	legacyOverviewIndexSHA256  = "a17e2fb2855c376b3a0b41961aa3ad0bc45f4193ebd445d6f4db31c430d3e0d6"
)

// Up applies Token migrations. The already-published migration 29 revokes
// token_gateway's schema CREATE privilege, while the already-published
// migration 33 creates an index as token_gateway. For that one pending version
// only, the trusted migrator lends CREATE and revokes it in the very same
// transaction. The privilege is therefore never committed or observable by a
// runtime session; a crash or any migration/hook error rolls the grant back.
func Up(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) error {
	return migrations.UpWithHooks(ctx, connection, loaded, migrations.UpHooks{
		BeforeApply: beforeApply,
		AfterApply:  afterApply,
	})
}

func beforeApply(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	if migration.Version != legacyOverviewIndexVersion {
		return nil
	}
	if migration.Name != legacyOverviewIndexName || migrationDigest(migration.UpSQL) != legacyOverviewIndexSHA256 {
		return fmt.Errorf("token migration 33 identity is unexpected")
	}
	var safe bool
	if err := tx.QueryRow(ctx, `SELECT
 current_user='token_gateway'
 AND (session_user='token_migrator' OR (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=session_user))
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname='token_schema_owner' AND NOT rolcanlogin AND NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolreplication AND NOT rolbypassrls)
 AND pg_catalog.pg_has_role(session_user,'token_schema_owner','SET')
 AND pg_catalog.pg_has_role(session_user,'token_gateway','SET')
 AND (SELECT pg_catalog.pg_get_userbyid(nspowner)='token_schema_owner' FROM pg_catalog.pg_namespace WHERE nspname='public')
 AND (SELECT pg_catalog.pg_get_userbyid(relowner)='token_gateway' FROM pg_catalog.pg_class WHERE oid=to_regclass('public.routing_policies'))`).Scan(&safe); err != nil {
		return fmt.Errorf("verify token migration 33 privilege bridge: %w", err)
	}
	if !safe {
		return fmt.Errorf("token migration 33 privilege bridge identity is unsafe")
	}
	if _, err := tx.Exec(ctx, `SET ROLE token_schema_owner; GRANT CREATE ON SCHEMA public TO token_gateway; SET ROLE token_gateway`); err != nil {
		return fmt.Errorf("lend token migration 33 schema privilege: %w", err)
	}
	return nil
}

func afterApply(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	if migration.Version != legacyOverviewIndexVersion {
		return nil
	}
	if migration.Name != legacyOverviewIndexName || migrationDigest(migration.UpSQL) != legacyOverviewIndexSHA256 {
		return fmt.Errorf("token migration 33 identity is unexpected")
	}
	if _, err := tx.Exec(ctx, `SET ROLE token_schema_owner; REVOKE CREATE ON SCHEMA public FROM token_gateway; SET ROLE token_gateway`); err != nil {
		return fmt.Errorf("revoke token migration 33 schema privilege: %w", err)
	}
	return nil
}

func migrationDigest(sql string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(sql, "\r\n", "\n"), "\r", "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(normalized)))
}
