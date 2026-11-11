package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestManualGrantRecoverySchemaPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if raw == "" {
		t.Skip("isolated PostgreSQL required")
	}
	t.Run("official-schema-runtime-and-migration-identities", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		repo, operator, _, _ := newEntitlementApprovalFixture(t, ctx, raw)
		n, err := repo.syncGrantRecoveryCases(ctx, 100)
		if err != nil || n != 0 {
			t.Fatal("empty admission", n, err)
		}
		var count int
		if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM public.list_approval_grant_recovery(NULL,101)`).Scan(&count); err != nil || count != 0 {
			t.Fatal("empty case inventory", count, err)
		}
		_, err = operator.Exec(ctx, `SELECT public.read_approval_grant_recovery($1)`, uuid.New())
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Fatal("migration session impersonated runtime", err)
		}
		var widened bool
		if err = repo.pool.QueryRow(ctx, `SELECT has_schema_privilege('account_approval_authorization_owner','public','CREATE')
 OR has_function_privilege('account_approval_writer','public.apply_approval_grant_recovery(uuid,uuid,text,uuid,bigint,jsonb,uuid,bigint)','EXECUTE')
 OR has_function_privilege('account_approval_recovery','public.apply_approval_grant_recovery(uuid,uuid,text,uuid,bigint,jsonb,uuid,bigint)','EXECUTE')`).Scan(&widened); err != nil || widened {
			t.Fatal("manual recovery ACL widened", err)
		}
		loaded, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		err = accountschema.DownOne(ctx, operator, loaded)
		if !errors.As(err, &pg) || pg.Code != "55000" {
			t.Fatal("evidence/fence down not refused", err)
		}
	})
}
