package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestApprovalAuthorizationDownReentryIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) {
			t.Logf("isolated migration failure position=%d context=%s", postgresError.Position, postgresError.Where)
		}
		t.Fatal(err)
	}
	var latest int64
	var owner string
	if err := db.adminPool.QueryRow(ctx, `SELECT (SELECT max(version) FROM public.schema_migrations), pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.outbox_events'::regclass`).Scan(&latest, &owner); err != nil || latest != 50 || owner != "account_service" {
		t.Fatalf("rollback did not restore migration/table ownership: %d %s %v", latest, owner, err)
	}
	// A safe rollback must restore ordinary production append, not only DDL.
	tx, err := db.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := events.AppendOutbox(ctx, tx, "account.fixture.rollback.v1", uuid.New(), 1, map[string]any{"rollback": "compatible"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("reentry after safe rollback: %v", err)
	}
	if err := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool}).ApprovalWriterReady(ctx); err != nil {
		t.Fatalf("writer did not recover after reentry: %v", err)
	}
}

func TestApprovalAuthorizationDownPreservesHistoryIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	tx, err := db.runtimePool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	approved := authorizationPrepareLegacyApproval(t, ctx, tx, request)
	if err := authorizationAppendOldWriter(ctx, tx, approved); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	requireControlSQLState(t, migrations.DownOne(ctx, connection.Conn(), loaded), "55000")
	var count int64
	var owner string
	if err := db.adminPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.outbox_events WHERE aggregate_id=$1 AND approval_control_epoch=1),pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.outbox_events'::regclass`, request.ID).Scan(&count, &owner); err != nil || count != 1 || owner != "account_approval_authorization_owner" {
		t.Fatalf("refused rollback changed authorization history: %d %s %v", count, owner, err)
	}
}

func TestApprovalAuthorizationDownGrantRollbackIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	down := loaded[len(loaded)-1].DownSQL
	marker := "\nREVOKE account_approval_authorization_owner FROM account_service;"
	if strings.Count(down, marker) != 1 {
		t.Fatal("rollback fault point must match exactly one final membership revoke")
	}
	tx, err := db.migratorPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Pause after the temporary inherited grant, before its final revocation.
	// All dropped objects and the grant must still be transaction-local.
	if _, err := tx.Exec(ctx, strings.SplitN(down, marker, 2)[0]); err != nil {
		t.Fatal(err)
	}
	var visible bool
	if err := db.runtimePool.QueryRow(ctx, `SELECT pg_has_role(session_user,'account_approval_authorization_owner','MEMBER')`).Scan(&visible); err != nil || visible {
		t.Fatalf("temporary migration grant escaped to another session: %v / %v", visible, err)
	}
	_, err = tx.Exec(ctx, `SELECT 1/0`)
	requireControlSQLState(t, err, "22012")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (&PostgresRepository{pool: db.runtimePool, approvalWriterPool: db.writerPool}).ApprovalWriterReady(ctx); err != nil {
		t.Fatalf("rollback fault failed to restore the writer contract: %v", err)
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT pg_has_role('account_service','account_approval_authorization_owner','MEMBER')`).Scan(&visible); err != nil || visible {
		t.Fatalf("rollback retained temporary role membership: %v / %v", visible, err)
	}
}

func TestApprovalAuthorizationDownRejectsControlStateIntegration(t *testing.T) {
	for _, state := range []string{"FENCED", "QUIESCING"} {
		t.Run(state, func(t *testing.T) {
			db := newApprovalAuthorizationIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			t.Cleanup(cancel)
			tx := authorizationControlOwnerTransaction(t, ctx, db.migratorPool)
			statement := `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`
			if state == "QUIESCING" {
				statement = `UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`
			}
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			before := readControlTuple(t, ctx, db.adminPool)
			connection := authorizationTestConnection(t, ctx, db.migratorPool)
			loaded, err := migrations.Load("../../migrations/account")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			defer migrations.Unlock(ctx, connection.Conn())
			requireControlSQLState(t, migrations.DownOne(ctx, connection.Conn(), loaded), "55000")
			if after := readControlTuple(t, ctx, db.adminPool); after != before {
				t.Fatalf("refused rollback changed control: before=%+v after=%+v", before, after)
			}
		})
	}
}
