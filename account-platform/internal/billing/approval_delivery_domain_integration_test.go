package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBillingAuthorizationDomainResultFirstPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "billing_authorization_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanup, `RESET SESSION AUTHORIZATION; RESET ROLE; DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadBillingIntegrationMigrations("../../migrations/billing", schema)
	if err != nil || len(loaded) < 2 || loaded[len(loaded)-2].Version != 54 || loaded[len(loaded)-1].Version != 55 {
		t.Fatalf("load isolated Billing 54 boundary: count=%d err=%v", len(loaded), err)
	}
	if err := migrations.Up(ctx, connection, loaded[:len(loaded)-2]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `GRANT USAGE ON SCHEMA `+identifier+` TO billing_service`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `ALTER TABLE `+identifier+`.schema_migrations OWNER TO billing_service`); err != nil {
		t.Fatal(err)
	}
	production, err := migrations.Load("../../migrations/billing")
	if err != nil || len(production) < 2 || production[len(production)-2].Version != 54 || production[len(production)-1].Version != 55 {
		t.Fatalf("load production Billing 54 migration: %v", err)
	}
	loaded[len(loaded)-2].UpSQL = rewriteBillingPublicSchema(production[len(production)-2].UpSQL, identifier)
	digest := sha256.Sum256([]byte(strings.ReplaceAll(loaded[len(loaded)-2].UpSQL, "\r\n", "\n")))
	loaded[len(loaded)-2].Checksum = hex.EncodeToString(digest[:])
	loaded[len(loaded)-1].UpSQL = rewriteBillingPublicSchema(production[len(production)-1].UpSQL, identifier)
	finalDigest := sha256.Sum256([]byte(strings.ReplaceAll(loaded[len(loaded)-1].UpSQL, "\r\n", "\n")))
	loaded[len(loaded)-1].Checksum = hex.EncodeToString(finalDigest[:])
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION billing_migrator`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SELECT set_config('search_path',$1,false)`, schema); err != nil {
		t.Fatal(err)
	}
	var sessionRole, currentRole, currentSchema string
	var ledgerExists bool
	if err := connection.QueryRow(ctx, `SELECT session_user,current_user,current_schema(),to_regclass('schema_migrations') IS NOT NULL`).Scan(&sessionRole, &currentRole, &currentSchema, &ledgerExists); err != nil || sessionRole != "billing_migrator" || currentRole != "billing_service" || currentSchema != schema || !ledgerExists {
		t.Fatalf("isolated Billing migration identity/session mismatch: session=%s current=%s schema=%s ledger=%v err=%v", sessionRole, currentRole, currentSchema, ledgerExists, err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams = map[string]string{"search_path": schema}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
	domain := &billingAuthorizationDomain{database: pool, repository: repository}

	requester, approver := uuid.New(), uuid.New()
	wallet, created, err := repository.EnsureWallet(ctx, "PERSONAL", requester)
	if err != nil || !created {
		t.Fatalf("provision wallet and ledger account: created=%v err=%v", created, err)
	}
	walletID := wallet.ID
	requestIDs := map[string]uuid.UUID{
		"WALLET_ADJUSTMENT":       uuid.New(),
		"PAYMENT_PROVIDER_RESUME": uuid.New(),
		"PAYMENT_PRODUCT_RESUME":  uuid.New(),
	}
	adjustmentID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO manual_adjustments(
id,wallet_id,amount_microcredits,reason,requested_by,approved_by,status,journal_entry_id,decision_reason,decided_at,
request_idempotency_key,approval_request_id,execution_idempotency_key,created_at,updated_at)
VALUES($1,$2,7,'approved adjustment',$3,$4,'POSTED',NULL,'approved by independent reviewer',now(),$5,$6,$7,now(),now())`,
		adjustmentID, walletID, requester, approver, "request-"+uuid.NewString(), requestIDs["WALLET_ADJUSTMENT"], "execution-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	// A ledger reference is optional for the result-first query. Keep the
	// fixture independent of ledger mutation and assert the closed empty object.
	orderID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO payment_orders(
id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,
late_review_boundary_at,final_archive_eligible_at,created_at,updated_at)
VALUES($1,$2,$3,$3,$4,'AUTH_RESULT_TEST','{}','ALIPAY',100,'CNY',100000,'PAID','PENDING',$5,$5,$6,$7,$8,$8)`,
		orderID, "authorization-result-"+uuid.NewString(), requester, walletID, createdAt.Add(2*time.Hour), createdAt.Add(72*time.Hour), createdAt.Add(8*24*time.Hour), createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_provider_resume_executions(
id,approval_request_id,provider_id,provider_version,probe_order_id,requester_user_id,approver_user_id,decision_reason,
provider_status,provider_trace_id,resumed_product_ids,control_versions_before,executed_at)
VALUES($1,$2,'provider-a','v1',$3,$4,$5,'approved provider recovery','AVAILABLE','trace','[]','{}',now())`,
		uuid.New(), requestIDs["PAYMENT_PROVIDER_RESUME"], orderID, requester, approver); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_product_resume_executions(
id,approval_request_id,provider_id,provider_version,product_id,probe_order_id,requester_user_id,approver_user_id,
decision_reason,provider_status,provider_trace_id,control_versions_before,executed_at)
VALUES($1,$2,'provider-a','v1','product-a',$3,$4,$5,'approved product recovery','AVAILABLE','trace','{}',now())`,
		uuid.New(), requestIDs["PAYMENT_PRODUCT_RESUME"], orderID, requester, approver); err != nil {
		t.Fatal(err)
	}

	for command, requestID := range requestIDs {
		result, found, err := domain.LookupAuthorizationResult(ctx, ApprovalAuthorizationBinding{ApprovalRequestID: requestID, CommandType: command}, approvalencoding.AuthorizationEvent{})
		if err != nil || !found || len(result.DomainResult) == 0 || string(result.LedgerReferences) != "{}" {
			t.Fatalf("%s result-first lookup found=%v result=%s ledger=%s err=%v", command, found, result.DomainResult, result.LedgerReferences, err)
		}
		observed, observedAt, err := repository.ObserveApprovalEffect(ctx, requestID, command)
		if err != nil || !observed || observedAt.IsZero() {
			t.Fatalf("%s authoritative effect observation observed=%v at=%s err=%v", command, observed, observedAt, err)
		}
	}
	missing, found, err := domain.LookupAuthorizationResult(ctx, ApprovalAuthorizationBinding{ApprovalRequestID: uuid.New(), CommandType: "PAYMENT_PROVIDER_RESUME"}, approvalencoding.AuthorizationEvent{})
	if err != nil || found || len(missing.DomainResult) != 0 {
		t.Fatalf("missing effect was treated as a result: found=%v result=%s err=%v", found, missing.DomainResult, err)
	}
	absent, observedAt, err := repository.ObserveApprovalEffect(ctx, uuid.New(), "PAYMENT_PROVIDER_RESUME")
	if err != nil || absent || observedAt.IsZero() {
		t.Fatalf("missing effect observation absent=%v at=%s err=%v", absent, observedAt, err)
	}

	pendingID, pendingRequestID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO manual_adjustments(
id,wallet_id,amount_microcredits,reason,requested_by,status,request_idempotency_key,created_at,updated_at)
VALUES($1,$2,9,'authorization execution fixture',$3,'PENDING_APPROVAL',$4,now(),now())`, pendingID, walletID, requester, "request-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	critical := json.RawMessage(`{"adjustment_version":1}`)
	normalized, _ := json.Marshal(map[string]any{"adjustment_id": pendingID, "wallet_id": walletID, "amount_microcredits": int64(9), "requested_by": requester})
	attempt := domain.ExecuteAuthorizationAttempt(ctx,
		ApprovalAuthorizationBinding{ApprovalRequestID: pendingRequestID, CommandType: "WALLET_ADJUSTMENT"},
		approvalencoding.AuthorizationEvent{CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: pendingID.String(), CriticalResourceVersions: critical},
		ApprovedCommand{CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: pendingID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, RequesterUserID: requester, ApproverUserID: approver, DecisionReason: "approved by independent reviewer", ExecutionIdempotencyKey: "approval-" + uuid.NewString()},
	)
	if attempt.FailureIsDefinite || len(attempt.Result.DomainResult) == 0 {
		t.Fatalf("real wallet adapter did not return a durable success: %+v", attempt)
	}
	var status string
	var storedRequest uuid.UUID
	var hasJournal bool
	if err := pool.QueryRow(ctx, `SELECT status,approval_request_id,journal_entry_id IS NOT NULL FROM manual_adjustments WHERE id=$1`, pendingID).Scan(&status, &storedRequest, &hasJournal); err != nil || status != "POSTED" || storedRequest != pendingRequestID || !hasJournal {
		t.Fatalf("real wallet effect was not committed once: status=%s request=%s journal=%v err=%v", status, storedRequest, hasJournal, err)
	}
}
