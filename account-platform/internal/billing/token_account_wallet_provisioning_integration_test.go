package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestTokenAccountWalletProvisioningPostgresIntegration is the Billing half
// of the TOKEN_ACCOUNT_CREATE recovery saga. CI runs it against real
// PostgreSQL; the response itself (never a direct DB lookup) is the receipt
// consumed by Token.
func TestTokenAccountWalletProvisioningPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	const serviceToken = "billing-token-account-provisioning-integration"
	handler := NewService(repository).Handler(serviceToken)
	ownerID, reference := uuid.New(), uuid.New()
	digest := tokenAccountWalletRequestDigest("PERSONAL", ownerID)
	payload := func(id uuid.UUID, owner uuid.UUID, requestDigest string) []byte {
		raw, _ := json.Marshal(map[string]any{"command_reference": id, "owner_type": "PERSONAL", "owner_id": owner, "request_digest": requestDigest})
		return raw
	}
	post := func(id uuid.UUID, owner uuid.UUID, requestDigest string, authorized bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/token-account-wallet-provisioning-receipts", bytes.NewReader(payload(id, owner, requestDigest)))
		request.Header.Set("Content-Type", "application/json")
		if authorized {
			request.Header.Set("Authorization", "Bearer "+serviceToken)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := post(reference, ownerID, digest, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized provisioning=%d/%s", response.Code, response.Body.String())
	}

	responses := make([]*httptest.ResponseRecorder, 2)
	var group sync.WaitGroup
	for index := range responses {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			responses[index] = post(reference, ownerID, digest, true)
		}(index)
	}
	group.Wait()
	var receipts [2]TokenAccountWalletProvisioningReceipt
	for index, response := range responses {
		if (response.Code != http.StatusCreated && response.Code != http.StatusOK) || json.Unmarshal(response.Body.Bytes(), &receipts[index]) != nil || receipts[index].Status != "SUCCEEDED" || receipts[index].WalletID == nil || receipts[index].CommandReference != reference || receipts[index].RequestDigest != digest || receipts[index].CompletedAt == nil {
			t.Fatalf("concurrent receipt[%d]=%d/%s", index, response.Code, response.Body.String())
		}
	}
	if *receipts[0].WalletID != *receipts[1].WalletID {
		t.Fatalf("existing-first wallet mismatch=%s/%s", *receipts[0].WalletID, *receipts[1].WalletID)
	}

	get := httptest.NewRequest(http.MethodGet, "/internal/v1/token-account-wallet-provisioning-receipts/"+reference.String(), nil)
	get.Header.Set("Authorization", "Bearer "+serviceToken)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	var recovered TokenAccountWalletProvisioningReceipt
	if getResponse.Code != http.StatusOK || json.Unmarshal(getResponse.Body.Bytes(), &recovered) != nil || recovered.CommandReference != reference || recovered.WalletID == nil || *recovered.WalletID != *receipts[0].WalletID {
		t.Fatalf("GET receipt=%d/%s", getResponse.Code, getResponse.Body.String())
	}

	mismatch := post(reference, uuid.New(), tokenAccountWalletRequestDigest("PERSONAL", uuid.New()), true)
	if mismatch.Code != http.StatusUnprocessableEntity {
		// The request digest above deliberately does not match its owner and must
		// be rejected before the existing receipt can be used as an oracle.
		t.Fatalf("invalid digest=%d/%s", mismatch.Code, mismatch.Body.String())
	}
	otherOwner := uuid.New()
	mismatch = post(reference, otherOwner, tokenAccountWalletRequestDigest("PERSONAL", otherOwner), true)
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), "WALLET_PROVISIONING_RECEIPT_MISMATCH") {
		t.Fatalf("receipt mismatch=%d/%s", mismatch.Code, mismatch.Body.String())
	}
	var wallets, ledgers, receiptFacts int
	if err := repository.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1),
 (SELECT count(*) FROM ledger_accounts WHERE wallet_id=$2),
 (SELECT count(*) FROM token_account_wallet_provisioning_receipts WHERE command_reference=$3)`, ownerID, recovered.WalletID, reference).Scan(&wallets, &ledgers, &receiptFacts); err != nil || wallets != 1 || ledgers != 1 || receiptFacts != 1 {
		t.Fatalf("wallet/ledger/receipt facts=%d/%d/%d err=%v", wallets, ledgers, receiptFacts, err)
	}
	var owner string
	var publicRead, runtimeRead, runtimeWrite bool
	qualifiedReceipt := schema + ".token_account_wallet_provisioning_receipts"
	if err := repository.pool.QueryRow(ctx, `SELECT
 pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid=to_regclass($1))),
 has_table_privilege('public',$1,'SELECT'),
 has_table_privilege('billing_service',$1,'SELECT'),
 has_table_privilege('billing_service',$1,'INSERT')`, qualifiedReceipt).Scan(&owner, &publicRead, &runtimeRead, &runtimeWrite); err != nil || owner != "billing_wallet_receipt_deployer" || publicRead || !runtimeRead || !runtimeWrite {
		t.Fatalf("receipt ACL owner/public/runtime-read/runtime-write=%s/%v/%v/%v err=%v", owner, publicRead, runtimeRead, runtimeWrite, err)
	}
}

// TestTokenAccountWalletReceiptMigrationRolesPostgresIntegration proves the
// production migration can be applied and safely rolled back by the dedicated
// non-superuser login without granting the runtime or object-owner role schema
// creation authority after installation.
func TestTokenAccountWalletReceiptMigrationRolesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	configuration, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := configuration.Copy()
	adminConfig.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	const databaseName = "account_platform_billing_wallet_receipt_roles_test"
	const migratorPassword = "billing-migrator-integration-only"
	const runtimePassword = "billing-runtime-integration-only"
	if _, err = admin.Exec(ctx, `
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_service') THEN CREATE ROLE billing_service; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_wallet_receipt_deployer') THEN CREATE ROLE billing_wallet_receipt_deployer NOLOGIN; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_schema_owner') THEN CREATE ROLE billing_schema_owner NOLOGIN; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='billing_migrator') THEN CREATE ROLE billing_migrator LOGIN NOINHERIT; END IF;
END $$;
ALTER ROLE billing_service LOGIN PASSWORD '`+runtimePassword+`';
ALTER ROLE billing_migrator LOGIN NOINHERIT PASSWORD '`+migratorPassword+`';
GRANT billing_service,billing_wallet_receipt_deployer,billing_schema_owner TO billing_migrator;`); err != nil {
		t.Fatal(err)
	}
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{databaseName}.Sanitize()+` WITH (FORCE)`)
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{databaseName}.Sanitize()+` OWNER billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+pgx.Identifier{databaseName}.Sanitize()+` WITH (FORCE)`)
	migratorConfig := configuration.Copy()
	migratorConfig.Database, migratorConfig.User, migratorConfig.Password = databaseName, "billing_migrator", migratorPassword
	connection, err := pgx.ConnectConfig(ctx, migratorConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	receiptIndex := -1
	for index := range loaded {
		if loaded[index].Version == 53 {
			receiptIndex = index
			break
		}
	}
	if receiptIndex < 0 || receiptIndex+1 >= len(loaded) || loaded[receiptIndex+1].Version != 54 {
		t.Fatal("integration fixture must explicitly isolate migration 53 from future migrations")
	}
	if err = migrations.Up(ctx, connection, loaded[:receiptIndex+1]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	var databaseIdentity string
	if err = connection.QueryRow(ctx, `SELECT current_database()`).Scan(&databaseIdentity); err != nil || databaseIdentity != databaseName {
		t.Fatalf("billing migration database=%s err=%v", databaseIdentity, err)
	}
	runtimeConfig := configuration.Copy()
	runtimeConfig.Database, runtimeConfig.User, runtimeConfig.Password = databaseName, "billing_service", runtimePassword
	runtimeConnection, err := pgx.ConnectConfig(ctx, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeConnection.Close(context.Background())
	var runtimeDatabase, runtimeSession, runtimeCurrent string
	var runtimeSuper bool
	if err = runtimeConnection.QueryRow(ctx, `SELECT current_database(),session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&runtimeDatabase, &runtimeSession, &runtimeCurrent, &runtimeSuper); err != nil || runtimeDatabase != databaseName || runtimeSession != "billing_service" || runtimeCurrent != "billing_service" || runtimeSuper {
		t.Fatalf("billing runtime identity=%s/%s/%s super=%v err=%v", runtimeDatabase, runtimeSession, runtimeCurrent, runtimeSuper, err)
	}
	if _, err = runtimeConnection.Exec(ctx, `CREATE TABLE public.runtime_receipt_shadow(value integer)`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("billing runtime schema CREATE=%v", err)
	}
	if _, err = runtimeConnection.Exec(ctx, `UPDATE token_account_wallet_provisioning_receipts SET request_digest=request_digest`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("billing runtime immutable receipt UPDATE=%v", err)
	}
	var sessionUser, currentUser, owner string
	var publicRead, runtimeRead, runtimeWrite, deployerCreate bool
	if err = connection.QueryRow(ctx, `SELECT session_user,current_user,
 pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid='public.token_account_wallet_provisioning_receipts'::regclass)),
 has_table_privilege('public','public.token_account_wallet_provisioning_receipts','SELECT'),
 has_table_privilege('billing_service','public.token_account_wallet_provisioning_receipts','SELECT'),
 has_table_privilege('billing_service','public.token_account_wallet_provisioning_receipts','INSERT'),
 has_schema_privilege('billing_wallet_receipt_deployer','public','CREATE')`).Scan(&sessionUser, &currentUser, &owner, &publicRead, &runtimeRead, &runtimeWrite, &deployerCreate); err != nil || sessionUser != "billing_migrator" || currentUser != "billing_service" || owner != "billing_wallet_receipt_deployer" || publicRead || !runtimeRead || !runtimeWrite || deployerCreate {
		t.Fatalf("migration identity/session/current/owner=%s/%s/%s ACL=%v/%v/%v deployer-create=%v err=%v", sessionUser, currentUser, owner, publicRead, runtimeRead, runtimeWrite, deployerCreate, err)
	}
	if _, err = connection.Exec(ctx, `CREATE TABLE public.runtime_shadow_receipt(value integer)`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("runtime schema CREATE must be denied: %v", err)
	}
	downSQL, err := os.ReadFile("../../migrations/billing/000053_token_account_wallet_provisioning_receipts.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(downSQL)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("non-superuser empty receipt down: %v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}
