package billingschema

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestGiftClosurePublicHarness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	am, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, ac, am); err != nil {
		t.Fatal("Account public Up", err)
	}
	if _, err = migrations.VerifyApplied(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, ac)
	bm, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = Up(ctx, bc, bm); err != nil {
		t.Fatal("Billing public Up", err)
	}
	if _, err = migrations.VerifyApplied(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, bc)
	name := "f08_token_closure_" + uuid.NewString()[:8] + time.Now().Format("150405")
	var exists bool
	if err = f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&exists); err != nil || exists {
		t.Fatal("own Token UUID absence", err)
	}
	if _, err = f.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	f.dbs[name] = f.config.User
	cfg := f.config.Copy()
	cfg.Database = name
	owner, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `CREATE EXTENSION pgcrypto; ALTER SCHEMA public OWNER TO token_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway`); err != nil {
		owner.Close(ctx)
		t.Fatal(err)
	}
	owner.Close(ctx)
	cfg.User = "token_migrator"
	cfg.Password = ""
	tc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.connections = append(f.connections, tc)
	if _, err = tc.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	tm, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenschema.Up(ctx, tc, tm); err != nil {
		t.Fatal("Token public Up", err)
	}
	if _, err = migrations.VerifyApplied(ctx, tc, tm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, tc)

	child := exec.CommandContext(ctx, os.Getenv("F08_TEST_GO"), "test", "-mod=readonly", "-race", "-count=1", "-json", "./internal/account", "-run", "^TestGiftClosureProducerBoundaries$")
	child.Dir = "../.."
	child.Env = append(os.Environ(), "F08_ACCOUNT_URL="+f.serviceURL(t, ac, "account_service"), "F08_BILLING_URL="+f.serviceURL(t, bc, "billing_service"), "F08_TOKEN_URL="+f.serviceURL(t, tc, "token_gateway"))
	out, e := child.CombinedOutput()
	if err = os.WriteFile(os.Getenv("F08_TEST_CHILD_LOG"), out, 0600); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatal("actual closure child failed; immutable log retained")
	}
}
