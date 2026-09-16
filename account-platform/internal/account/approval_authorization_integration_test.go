package account

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type approvalAuthorizationIntegrationDatabase struct {
	databaseName string
	adminPool    *pgxpool.Pool
	runtimePool  *pgxpool.Pool
	writerPool   *pgxpool.Pool
	backfillPool *pgxpool.Pool
	migratorPool *pgxpool.Pool
}

func newApprovalAuthorizationIntegrationDatabase(t *testing.T) *approvalAuthorizationIntegrationDatabase {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Close(context.Background()) })
	// PINVOU_TEST_ACCOUNT_DATABASE_URL explicitly denotes an isolated cluster.
	// Mirror the production role precondition without creating passwords or
	// changing any externally reachable identity.
	if _, err := bootstrap.Exec(ctx, `
DO $$ BEGIN
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_authorization_owner') THEN
    CREATE ROLE account_approval_authorization_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_writer') THEN
    CREATE ROLE account_approval_writer LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_backfill') THEN
    CREATE ROLE account_approval_backfill LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='account_approval_recovery') THEN
    CREATE ROLE account_approval_recovery NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
END $$;
GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT FALSE, SET TRUE, ADMIN TRUE;
`); err != nil {
		t.Fatal(err)
	}
	name := "gsa06_authorization_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := bootstrap.Exec(ctx, `CREATE DATABASE `+identifier+` OWNER account_service TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, cleanupErr := bootstrap.Exec(cleanupCtx, `DROP DATABASE `+identifier); cleanupErr != nil {
			t.Errorf("remove isolated authorization test database: %v", cleanupErr)
		}
	})

	migrationConfig := base.Copy()
	migrationConfig.Database = name
	migrationConnection, err := pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrationConnection.Close(context.Background()) })
	if _, err := migrationConnection.Exec(ctx, `GRANT CREATE ON DATABASE `+identifier+` TO account_migrator; SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadApprovalAuthorizationBaselineMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 || loaded[len(loaded)-1].Version != 51 {
		t.Fatal("authorization integration fixture requires migration 51 as its historical Account boundary")
	}
	if err := migrations.Up(ctx, migrationConnection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, migrationConnection)
	if _, err := migrationConnection.Exec(ctx, `RESET ROLE; RESET SESSION AUTHORIZATION`); err != nil {
		t.Fatal(err)
	}

	database := &approvalAuthorizationIntegrationDatabase{databaseName: name}
	database.adminPool = newApprovalAuthorizationActorPool(t, ctx, databaseURL, name, "")
	database.runtimePool = newApprovalAuthorizationActorPool(t, ctx, databaseURL, name, "account_service")
	database.writerPool = newApprovalAuthorizationActorPool(t, ctx, databaseURL, name, "account_approval_writer")
	database.backfillPool = newApprovalAuthorizationActorPool(t, ctx, databaseURL, name, "account_approval_backfill")
	database.migratorPool = newApprovalAuthorizationActorPool(t, ctx, databaseURL, name, "account_migrator")
	return database
}

func newApprovalAuthorizationActorPool(t *testing.T, ctx context.Context, databaseURL, database, role string) *pgxpool.Pool {
	t.Helper()
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.Database = database
	if role != "" {
		configuration.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
			_, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION `+pgx.Identifier{role}.Sanitize())
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedSubmittedApprovalForAuthorization(t *testing.T, database *approvalAuthorizationIntegrationDatabase, commandType string) ApprovalRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	policy, err := approvalpolicy.Lookup(commandType)
	if err != nil {
		t.Fatal(err)
	}
	actor := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_SUPER_ADMIN", "authorization-"+strings.ToLower(commandType)+"-"+uuid.NewString())
	repository := &PostgresRepository{pool: database.adminPool, now: func() time.Time { return time.Now().UTC() }}
	requestID := seedAdvancementRequest(t, ctx, repository, actor.ID, policy, "DRAFT")
	if commandType == "WALLET_ADJUSTMENT" {
		adjustmentID, walletID := uuid.New(), uuid.New()
		normalized, _ := json.Marshal(map[string]any{
			"adjustment_id": adjustmentID, "wallet_id": walletID,
			"amount_microcredits": int64(10), "requested_by": actor.ID,
		})
		critical := json.RawMessage(`{"adjustment_version":1}`)
		financial, _ := json.Marshal(map[string]any{"wallet_id": walletID, "delta_microcredits": int64(10)})
		if _, err := database.adminPool.Exec(ctx, `UPDATE approval_requests
            SET normalized_command=$2,target_resource_type='MANUAL_ADJUSTMENT',target_resource_id=$3
            WHERE id=$1`, requestID, normalized, adjustmentID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := database.adminPool.Exec(ctx, `UPDATE approval_request_snapshots
            SET critical_resource_versions=$2,financial_snapshot=$3 WHERE approval_request_id=$1`,
			requestID, critical, financial); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.adminPool.Exec(ctx, `UPDATE approval_requests
        SET status='SUBMITTED',submitted_at=now(),version=version+1,updated_at=now()
        WHERE id=$1 AND status='DRAFT'`, requestID); err != nil {
		t.Fatal(err)
	}
	request, err := approvalRequestByID(ctx, database.adminPool, requestID)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestApprovalAuthorizationApproveAtomicIntegration(t *testing.T) {
	database := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request := seedSubmittedApprovalForAuthorization(t, database, "WALLET_ADJUSTMENT")
	approver := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_SUPER_ADMIN", "authorization-approver-"+uuid.NewString())
	repository := (&PostgresRepository{
		pool:               database.runtimePool,
		approvalWriterPool: database.writerPool,
		now:                func() time.Time { return time.Now().UTC() },
	}).WithBillingGovernanceActivation(&billingAdvancementReader{snapshot: billingAdvancementSnapshot("ACTIVE")})

	approved, err := repository.DecideApprovalRequest(ctx, approvalIntegrationPrincipal(approver), request.ID, request.Version, "APPROVED", "independent authorization approval", request.CriticalResourceVersions)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "APPROVED" || approved.Version != request.Version+1 || approved.ExecutionDeadline == nil {
		t.Fatalf("approved request mismatch: %s/%d/%v", approved.Status, approved.Version, approved.ExecutionDeadline)
	}
	var decisions, stateEvents, outboxRows, sidecars, pending int64
	var decision, provenance, serialization string
	if err := database.adminPool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
        (SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND to_status='APPROVED'),
        (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'),
        (SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes s JOIN outbox_events o ON o.event_id=s.event_id WHERE o.aggregate_id=$1),
        (SELECT count(*) FROM account_approval_authorization.authorization_event_pending p JOIN outbox_events o ON o.event_id=p.event_id WHERE o.aggregate_id=$1),
        (SELECT decision FROM approval_decisions WHERE approval_request_id=$1),
        (SELECT provenance FROM account_approval_authorization.approval_authorization_event_bytes s JOIN outbox_events o ON o.event_id=s.event_id WHERE o.aggregate_id=$1),
        (SELECT serialization_version FROM account_approval_authorization.approval_authorization_event_bytes s JOIN outbox_events o ON o.event_id=s.event_id WHERE o.aggregate_id=$1)`, request.ID).Scan(
		&decisions, &stateEvents, &outboxRows, &sidecars, &pending, &decision, &provenance, &serialization); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || stateEvents != 1 || outboxRows != 1 || sidecars != 1 || pending != 0 || decision != "APPROVED" || provenance != "FROZEN_AT_APPROVAL" || serialization != "LEGACY_TYPED_V1" {
		t.Fatalf("approval atomic facts mismatch: decisions=%d states=%d outbox=%d sidecars=%d pending=%d %s/%s/%s", decisions, stateEvents, outboxRows, sidecars, pending, decision, provenance, serialization)
	}
}
