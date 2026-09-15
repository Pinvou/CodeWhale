package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalRelayMigrationRejectsMissingOrInvalidSidecarIntegration(t *testing.T) {
	for _, kind := range []string{"missing", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			db := newApprovalAuthorizationIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			if _, err := db.adminPool.Exec(ctx, `DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_relay') THEN
  CREATE ROLE account_approval_relay LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
 END IF;
END $$`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.adminPool.Exec(ctx, `ALTER TABLE account_approval_authorization.approval_authorization_event_bytes DISABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				if _, err := db.adminPool.Exec(ctx, `DELETE FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1`, eventID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_authorization.approval_authorization_event_bytes
SET delivery_bytes='{}'::bytea,event_digest=encode(sha256('{}'::bytea),'hex') WHERE event_id=$1`, eventID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.adminPool.Exec(ctx, `ALTER TABLE account_approval_authorization.approval_authorization_event_bytes ENABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadApprovalRelayBaselineMigrations()
			if err != nil {
				t.Fatal(err)
			}
			connection := authorizationTestConnection(t, ctx, db.migratorPool)
			if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			defer migrations.Unlock(ctx, connection.Conn())
			err = migrations.Up(ctx, connection.Conn(), loaded)
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != "55000" {
				t.Fatalf("migration accepted %s sidecar: %v", kind, err)
			}
			var version int64
			var relayColumn bool
			if err := db.adminPool.QueryRow(ctx, `SELECT max(version),EXISTS(SELECT 1 FROM information_schema.columns
WHERE table_schema='public' AND table_name='outbox_events' AND column_name='relay_claim_token') FROM public.schema_migrations`).Scan(&version, &relayColumn); err != nil || version != 51 || relayColumn {
				t.Fatalf("failed migration left state: version=%d relay_column=%v err=%v", version, relayColumn, err)
			}
		})
	}
}
