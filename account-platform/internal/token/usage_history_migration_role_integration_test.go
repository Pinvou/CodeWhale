package token

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenmigrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUsageHistoryMigrationUsesProductionRoleChain(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if base == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	control, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	connection, isolatedURL := newThreeServiceDatabase(t, ctx, base, "token_saga_")
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil || len(loaded) == 0 || loaded[len(loaded)-1].Version != 34 {
		t.Fatalf("load Token migration 34: %v", err)
	}
	if err = tokenmigrations.Up(ctx, connection, loaded[:len(loaded)-1]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)

	var usageOwner string
	if err = connection.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(relowner) FROM pg_catalog.pg_class WHERE oid='public.usage_requests'::regclass`).Scan(&usageOwner); err != nil || usageOwner != "token_gateway" {
		t.Fatalf("usage_requests owner=%q err=%v, want token_gateway", usageOwner, err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, ownerID, walletID := uuid.New(), uuid.New(), uuid.New()
	accountID, credentialID, actorID := uuid.New(), uuid.New(), uuid.New()
	if _, err = connection.Exec(ctx, `
INSERT INTO public.token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($1,'USER',$2,$3,'Usage history role account','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO public.provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status)
VALUES($6,$1,'provider','https://provider.invalid','x'::bytea,'key','usage-history-role-fingerprint',1,'ACTIVE',ARRAY['model-one'],$5,$4,$5,'VALID');`,
		pgx.QueryExecModeSimpleProtocol, accountID, ownerID, walletID, actorID, now, credentialID); err != nil {
		t.Fatal(err)
	}

	insertUsage := func(q interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, id uuid.UUID, turnID string, startedAt time.Time) error {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(id.String())))
		_, insertErr := q.Exec(ctx, `INSERT INTO public.usage_requests(
id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,execution_status,delivery_status,settlement_status,
maximum_microcredits,actual_microcredits,request_snapshot,started_at,terminal_at,updated_at,idempotency_key_digest,request_fingerprint)
VALUES($1,$2,'usage-history-role-session',$3,$4,$5,'model-one','RELEASED','FAILED','DELIVERED','RELEASED',
       100,1,jsonb_build_object('logical_turn_id',$6::text),$7,$7,$7,$8,$8)`,
			id, userID, accountID, credentialID, walletID, turnID, startedAt, digest)
		return insertErr
	}

	historicalID := uuid.New()
	if err = insertUsage(connection, historicalID, "production-role-history", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	previousPassword, previousLogin, previousInherit, addedMemberships := prepareUsageHistoryMigratorRole(t, ctx, control)
	t.Cleanup(func() {
		restoreUsageHistoryMigratorRole(t, control, previousPassword, previousLogin, previousInherit, addedMemberships)
	})
	rolePassword := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	if _, err = control.Exec(ctx, `ALTER ROLE token_migrator LOGIN NOINHERIT PASSWORD '`+rolePassword+`'`); err != nil {
		t.Fatal(err)
	}

	migratorConfig, err := pgx.ParseConfig(isolatedURL)
	if err != nil {
		t.Fatal(err)
	}
	migratorConfig.User, migratorConfig.Password = "token_migrator", rolePassword
	migrator, err := pgx.ConnectConfig(ctx, migratorConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close(context.Background())
	if _, err = migrator.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	if err = tokenmigrations.Up(ctx, migrator, loaded); err != nil {
		t.Fatalf("apply migration 34 through token_migrator role chain: %v", err)
	}
	migrations.Unlock(ctx, migrator)

	var schemaOwnerCanSelect, schemaOwnerCanReference bool
	if err = migrator.QueryRow(ctx, `SELECT
pg_catalog.has_table_privilege('token_schema_owner','public.usage_requests','SELECT'),
pg_catalog.has_table_privilege('token_schema_owner','public.usage_requests','REFERENCES')`).Scan(&schemaOwnerCanSelect, &schemaOwnerCanReference); err != nil || !schemaOwnerCanSelect || schemaOwnerCanReference {
		t.Fatalf("schema owner privileges select=%v references=%v err=%v", schemaOwnerCanSelect, schemaOwnerCanReference, err)
	}

	runtimeRole := "usage_history_runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	runtimePassword := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	quotedRuntime := pgx.Identifier{runtimeRole}.Sanitize()
	if _, err = control.Exec(ctx, `CREATE ROLE `+quotedRuntime+` LOGIN INHERIT PASSWORD '`+runtimePassword+`'; GRANT token_gateway TO `+quotedRuntime); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, dropErr := control.Exec(context.Background(), `DROP ROLE `+quotedRuntime); dropErr != nil {
			t.Errorf("drop usage history runtime role: %v", dropErr)
		}
	})
	runtimeConfig := migratorConfig.Copy()
	runtimeConfig.User, runtimeConfig.Password = runtimeRole, runtimePassword
	runtime, err := pgx.ConnectConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())

	rollingID := uuid.New()
	if err = insertUsage(runtime, rollingID, "production-role-old-pod", now); err != nil {
		t.Fatalf("old Pod insert through runtime role: %v", err)
	}
	var rollingProjected bool
	if err = runtime.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.usage_history_members WHERE usage_request_id=$1)`, rollingID).Scan(&rollingProjected); err != nil || !rollingProjected {
		t.Fatalf("old Pod trigger projection=%v err=%v", rollingProjected, err)
	}

	var processed int
	var complete bool
	if err = runtime.QueryRow(ctx, `SELECT processed,complete FROM public.backfill_usage_history_projection(500)`).Scan(&processed, &complete); err != nil || processed < 2 || !complete {
		t.Fatalf("runtime backfill processed=%d complete=%v err=%v", processed, complete, err)
	}
	var projectedCount int
	if err = runtime.QueryRow(ctx, `SELECT count(*) FROM public.usage_history_members WHERE usage_request_id IN ($1,$2)`, historicalID, rollingID).Scan(&projectedCount); err != nil || projectedCount != 2 {
		t.Fatalf("projected rows=%d err=%v", projectedCount, err)
	}
	if _, err = runtime.Exec(ctx, `UPDATE public.usage_history_projection_state SET projected_rows=0 WHERE singleton`); err == nil {
		t.Fatal("runtime can forge usage history sidecar state")
	}
	if _, err = runtime.Exec(ctx, `SELECT public.project_usage_history_request($1,$2,NULL,'{}'::jsonb,$3)`, uuid.New(), userID, now); err == nil {
		t.Fatal("runtime can execute internal projection function")
	}

	if err = runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = migrations.DownOne(ctx, migrator, loaded); err != nil {
		t.Fatalf("rollback migration 34 through production role chain: %v", err)
	}
	var triggerExists, schemaOwnerStillSelects bool
	if err = migrator.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid='public.usage_requests'::regclass AND tgname='usage_history_projection_after_insert' AND NOT tgisinternal),
pg_catalog.has_table_privilege('token_schema_owner','public.usage_requests','SELECT')`).Scan(&triggerExists, &schemaOwnerStillSelects); err != nil || triggerExists || schemaOwnerStillSelects {
		t.Fatalf("down cleanup trigger=%v schema_owner_select=%v err=%v", triggerExists, schemaOwnerStillSelects, err)
	}
}

func prepareUsageHistoryMigratorRole(t *testing.T, ctx context.Context, control *pgx.Conn) (*string, bool, bool, []string) {
	t.Helper()
	var previousPassword *string
	var previousLogin, previousInherit bool
	if err := control.QueryRow(ctx, `SELECT rolpassword,rolcanlogin,rolinherit FROM pg_catalog.pg_authid WHERE rolname='token_migrator'`).Scan(&previousPassword, &previousLogin, &previousInherit); err != nil {
		t.Fatal(err)
	}
	added := make([]string, 0, 2)
	for _, role := range []string{"token_gateway", "token_schema_owner"} {
		var member bool
		if err := control.QueryRow(ctx, `SELECT pg_catalog.pg_has_role('token_migrator',$1,'MEMBER')`, role).Scan(&member); err != nil {
			t.Fatal(err)
		}
		if !member {
			if _, err := control.Exec(ctx, `GRANT `+pgx.Identifier{role}.Sanitize()+` TO token_migrator`); err != nil {
				t.Fatal(err)
			}
			added = append(added, role)
		}
	}
	return previousPassword, previousLogin, previousInherit, added
}

func restoreUsageHistoryMigratorRole(t *testing.T, control *pgx.Conn, password *string, login, inherit bool, added []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, role := range added {
		if _, err := control.Exec(ctx, `REVOKE `+pgx.Identifier{role}.Sanitize()+` FROM token_migrator`); err != nil {
			t.Errorf("restore token_migrator membership: %v", err)
		}
	}
	passwordSQL := "NULL"
	if password != nil {
		passwordSQL = "'" + strings.ReplaceAll(*password, "'", "''") + "'"
	}
	loginSQL, inheritSQL := "NOLOGIN", "NOINHERIT"
	if login {
		loginSQL = "LOGIN"
	}
	if inherit {
		inheritSQL = "INHERIT"
	}
	if _, err := control.Exec(ctx, `ALTER ROLE token_migrator `+loginSQL+` `+inheritSQL+` PASSWORD `+passwordSQL); err != nil {
		t.Errorf("restore token_migrator attributes: %v", err)
	}
}
