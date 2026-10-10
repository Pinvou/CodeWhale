package billingschema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Cross-domain closure owns a separate UUID database per service. It installs
// the same five isolated Token actors as the public Token bootstrap, never
// adopts or modifies an existing deployment actor.
func (f *stageFixture) installClosureTokenActors(t *testing.T, ctx context.Context) {
	t.Helper()
	roles := []string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator"}
	var count int
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&count); err != nil || count != 0 {
		t.Fatal("existing Token actors: no fixture mutation permitted")
	}
	tx, err := f.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, role := range roles {
		options := " NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
		if role != "token_schema_owner" && role != "token_cap_policy_deployer" {
			options = " LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
		}
		if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+options); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator; GRANT token_gateway TO token_schema_owner WITH INHERIT TRUE,SET TRUE`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.roles = append(f.roles, roles...)
}
