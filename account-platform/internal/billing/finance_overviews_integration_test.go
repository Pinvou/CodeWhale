package billing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestFinanceOverviewsPostgresIntegration exercises the aggregate statements
// against isolated minimal tables mirroring every production column they read.
// It complements, but does not replace, full migration-chain tests.
func TestFinanceOverviewsPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newFinanceOverviewIntegrationRepository(t, ctx, databaseURL)
	asOf := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	accountsBefore, err := repository.AdminFinanceAccountsOverview(ctx, asOf)
	if err != nil {
		t.Fatalf("accounts overview query: %v", err)
	}
	pricingBefore, err := repository.AdminFinancePricingOverview(ctx, asOf)
	if err != nil {
		t.Fatalf("pricing overview query: %v", err)
	}
	productsBefore, err := repository.AdminFinanceProductsOverview(ctx, asOf)
	if err != nil {
		t.Fatalf("products overview query: %v", err)
	}

	walletID, ownerID, requesterID := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,receivable_microcredits,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',1000,200,100,$3,$3)`, walletID, ownerID, asOf.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	accountsAfter, err := repository.AdminFinanceAccountsOverview(ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if accountsAfter.WalletCount != accountsBefore.WalletCount+1 || accountsAfter.PostedMicrocredits != accountsBefore.PostedMicrocredits+1000 || accountsAfter.ReservedMicrocredits != accountsBefore.ReservedMicrocredits+200 || accountsAfter.ReceivableMicrocredits != accountsBefore.ReceivableMicrocredits+100 || accountsAfter.AvailableMicrocredits != accountsBefore.AvailableMicrocredits+700 {
		t.Fatalf("wallet projections were miscounted: before=%+v after=%+v", accountsBefore, accountsAfter)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO manual_adjustments(id,wallet_id,status) VALUES
($1,$2,'PENDING_APPROVAL'),($3,$2,'POSTED')`, uuid.New(), walletID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	for _, hold := range []struct {
		status    string
		expiresAt time.Time
	}{
		{status: "REVIEW", expiresAt: asOf.Add(time.Hour)},
		{status: "ACTIVE", expiresAt: asOf},
		{status: "ACTIVE", expiresAt: asOf.Add(time.Hour)},
		{status: "RELEASED", expiresAt: asOf.Add(-time.Hour)},
	} {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_holds(id,status,expires_at) VALUES($1,$2,$3)`, uuid.New(), hold.status, hold.expiresAt); err != nil {
			t.Fatal(err)
		}
	}
	accountsWithAttention, err := repository.AdminFinanceAccountsOverview(ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if accountsWithAttention.PendingAdjustmentCount != accountsBefore.PendingAdjustmentCount+1 || accountsWithAttention.AtRiskHoldCount != accountsBefore.AtRiskHoldCount+2 {
		t.Fatalf("account attention semantics were miscounted: before=%+v after=%+v", accountsBefore, accountsWithAttention)
	}

	for _, fixture := range []struct {
		name          string
		lifecycle     string
		effectiveFrom time.Time
		effectiveTo   any
		publishedAt   any
	}{
		{name: "draft", lifecycle: "DRAFT", effectiveFrom: asOf.Add(time.Hour), effectiveTo: nil, publishedAt: nil},
		{name: "future", lifecycle: "PUBLISHED", effectiveFrom: asOf.Add(time.Hour), effectiveTo: nil, publishedAt: asOf.Add(-time.Hour)},
		{name: "active", lifecycle: "PUBLISHED", effectiveFrom: asOf, effectiveTo: asOf.Add(time.Hour), publishedAt: asOf.Add(-time.Hour)},
		{name: "ended-boundary", lifecycle: "PUBLISHED", effectiveFrom: asOf.Add(-time.Hour), effectiveTo: asOf, publishedAt: asOf.Add(-2 * time.Hour)},
		{name: "cancelled", lifecycle: "CANCELLED", effectiveFrom: asOf.Add(time.Hour), effectiveTo: nil, publishedAt: asOf.Add(-time.Hour)},
	} {
		model := "overview-" + fixture.name + "-" + uuid.NewString()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,effective_to,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,published_at) VALUES($1,$2,1,1,1,$3,$4,$5,'MODEL',$2,'CHAT','00000000-0000-7000-8200-000000000001','MICROCREDIT',$6,$7)`, uuid.New(), model, fixture.effectiveFrom, fixture.effectiveTo, asOf.Add(-2*time.Hour), fixture.lifecycle, fixture.publishedAt); err != nil {
			t.Fatalf("insert %s price: %v", fixture.name, err)
		}
	}
	pricingAfter, err := repository.AdminFinancePricingOverview(ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if pricingAfter.PriceDraftCount != pricingBefore.PriceDraftCount+1 || pricingAfter.PriceFutureCount != pricingBefore.PriceFutureCount+1 || pricingAfter.PriceActiveCount != pricingBefore.PriceActiveCount+1 || pricingAfter.PriceEndedOrCancelledCount != pricingBefore.PriceEndedOrCancelledCount+2 {
		t.Fatalf("derived price states were not mutually exclusive: before=%+v after=%+v", pricingBefore, pricingAfter)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO metering_unit_definitions(id,status) VALUES
($1,'ACTIVE'),($2,'INACTIVE')`, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	pricingWithMetering, err := repository.AdminFinancePricingOverview(ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if pricingWithMetering.MeteringActiveCount != pricingBefore.MeteringActiveCount+1 || pricingWithMetering.MeteringInactiveCount != pricingBefore.MeteringInactiveCount+1 {
		t.Fatalf("metering states were miscounted: before=%+v after=%+v", pricingBefore, pricingWithMetering)
	}
	orderID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_orders(
id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,
late_review_boundary_at,final_archive_eligible_at,created_at,updated_at
) VALUES($1,$2,$3,$3,$4,'OVERVIEW_TEST','{}','ALIPAY',600,'CNY',600000,'PAID','PENDING',$5,$5,$6,$7,$8,$8)`,
		orderID, "overview-"+uuid.NewString(), requesterID, walletID, asOf.Add(time.Hour), asOf.Add(72*time.Hour), asOf.Add(7*24*time.Hour), asOf.Add(-12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var evidenceTransactionID uuid.UUID
	for index, fact := range []struct {
		amount     int64
		paidAt     any
		verifiedAt time.Time
	}{
		{amount: 100, paidAt: asOf.Add(-24 * time.Hour), verifiedAt: asOf.Add(-24 * time.Hour)},
		{amount: 200, paidAt: asOf.Add(-time.Nanosecond), verifiedAt: asOf.Add(-time.Nanosecond)},
		{amount: 300, paidAt: asOf, verifiedAt: asOf},
		{amount: 400, paidAt: nil, verifiedAt: asOf.Add(-time.Hour)},
	} {
		transactionID := uuid.New()
		if index == 0 {
			evidenceTransactionID = transactionID
		}
		if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,'ALIPAY',$3,'PAID',$4,'CNY','QUERY',$5,$6,$6,$6)`, transactionID, orderID, "overview-fact-"+uuid.NewString(), fact.amount, fact.paidAt, fact.verifiedAt); err != nil {
			t.Fatalf("insert payment fact %d: %v", index, err)
		}
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_evidence(id,payment_transaction_id) VALUES($1,$3),($2,$3)`, uuid.New(), uuid.New(), evidenceTransactionID); err != nil {
		t.Fatal(err)
	}
	for table, statuses := range map[string][]string{
		"late_payment_review_cases": {"REVIEW_REQUIRED_LATE_PAYMENT", "PROVIDER_BLIND", "RESOLVED_PAID"},
		"payment_anomaly_cases":     {"OPEN", "RESOLVED"},
		"reconciliation_cases":      {"OPEN", "IN_REVIEW", "RESOLVED"},
		"refunds":                   {"FAILED", "MANUAL_REVIEW", "SUCCEEDED"},
	} {
		statusColumn := "status"
		if table == "late_payment_review_cases" {
			statusColumn = "review_status"
		}
		for _, status := range statuses {
			if _, err := repository.pool.Exec(ctx, `INSERT INTO `+table+`(id,`+statusColumn+`) VALUES($1,$2)`, uuid.New(), status); err != nil {
				t.Fatalf("insert %s %s: %v", table, status, err)
			}
		}
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,updated_at) VALUES
('disabled','v1',false,$1),('enabled','v1',true,$1),('future-disabled','v1',false,$2)`, asOf, asOf.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,updated_at) VALUES
('disabled','v1','p1',false,$1),('enabled','v1','p2',true,$1),('future-disabled','v1','p3',false,$2)`, asOf, asOf.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	payments, err := repository.AdminFinancePaymentsOverview(ctx, asOf.Add(-24*time.Hour), asOf, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if payments.OrdersCreatedCount != 1 || payments.PaymentsReceivedFen != 700 || payments.LateReviewOpenCount != 2 || payments.PaymentAnomalyOpenCount != 1 || payments.ReconciliationOpenCount != 2 || payments.RefundAttentionCount != 2 || payments.ProviderControlledCount != 1 || payments.ProductControlledCount != 1 {
		t.Fatalf("payment window, fallback, deduplication, or attention semantics were miscounted: %+v", payments)
	}

	type productFixture struct {
		productID        string
		status           string
		deletedAt        any
		verification     string
		evidenceExpiry   any
		revoked          bool
		bindingMode      string
		priorFailedCheck bool
	}
	for _, fixture := range []productFixture{
		{productID: "OVERVIEW_MISSING_" + uuid.NewString(), status: "DRAFT"},
		{productID: "OVERVIEW_PENDING_" + uuid.NewString(), status: "OFF_SALE", verification: "PENDING", priorFailedCheck: true},
		{productID: "OVERVIEW_FAILED_" + uuid.NewString(), status: "ON_SALE", verification: "FAILED"},
		{productID: "OVERVIEW_VALID_" + uuid.NewString(), status: "ON_SALE", verification: "VERIFIED", evidenceExpiry: asOf.Add(time.Hour), bindingMode: "LATEST_ACTIVE"},
		{productID: "OVERVIEW_EXPIRED_" + uuid.NewString(), status: "ON_SALE", verification: "VERIFIED", evidenceExpiry: asOf},
		{productID: "OVERVIEW_REVOKED_" + uuid.NewString(), status: "ON_SALE", verification: "VERIFIED", evidenceExpiry: asOf.Add(time.Hour), revoked: true},
		{productID: "OVERVIEW_BINDING_EXPIRED_" + uuid.NewString(), status: "ON_SALE", verification: "VERIFIED", evidenceExpiry: asOf.Add(time.Hour), bindingMode: "ENDED_EXPIRED"},
		{productID: "OVERVIEW_BINDING_REVOKED_" + uuid.NewString(), status: "ON_SALE", verification: "VERIFIED", evidenceExpiry: asOf.Add(time.Hour), bindingMode: "ENDED_REVOKED"},
		{productID: "OVERVIEW_DELETED_" + uuid.NewString(), status: "DELETED", deletedAt: asOf},
	} {
		catalogItemID := uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO recharge_product_catalog_items(catalog_item_id,product_id,display_name_zh_cn,display_name_en,display_name_ja,lifecycle_status,deleted_at,created_at,updated_at) VALUES($1,$2,$2,$2,$2,$3,$4,$5,$5)`, catalogItemID, fixture.productID, fixture.status, fixture.deletedAt, asOf); err != nil {
			t.Fatal(err)
		}
		if fixture.priorFailedCheck {
			if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_verifications(verification_id,catalog_item_id,target_draft_version,publish_fingerprint,status,requested_at) VALUES($1,$2,1,$3,'FAILED',$4)`, uuid.New(), catalogItemID, uuid.NewString(), asOf.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if fixture.verification == "" {
			continue
		}
		verificationID := uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_verifications(verification_id,catalog_item_id,target_draft_version,publish_fingerprint,status,requested_at) VALUES($1,$2,1,$3,$4,$5)`, verificationID, catalogItemID, uuid.NewString(), fixture.verification, asOf); err != nil {
			t.Fatal(err)
		}
		if fixture.verification != "VERIFIED" {
			continue
		}
		evidenceID := uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_evidence(evidence_id,verification_id,catalog_item_id,expires_at) VALUES($1,$2,$3,$4)`, evidenceID, verificationID, catalogItemID, fixture.evidenceExpiry); err != nil {
			t.Fatal(err)
		}
		if fixture.revoked {
			if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_revocations(revocation_id,evidence_id) VALUES($1,$2)`, uuid.New(), evidenceID); err != nil {
				t.Fatal(err)
			}
		}
		if fixture.bindingMode == "LATEST_ACTIVE" {
			if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_bindings(catalog_item_id,admission_version,evidence_id,effective_to) VALUES($1,1,$2,NULL)`, catalogItemID, evidenceID); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasPrefix(fixture.bindingMode, "ENDED_") {
			bindingVerificationID := uuid.New()
			if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_verifications(verification_id,catalog_item_id,target_draft_version,publish_fingerprint,status,requested_at) VALUES($1,$2,1,$3,'VERIFIED',$4)`, bindingVerificationID, catalogItemID, uuid.NewString(), asOf.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			bindingEvidenceID := uuid.New()
			bindingExpiry := asOf
			if fixture.bindingMode == "ENDED_REVOKED" {
				bindingExpiry = asOf.Add(time.Hour)
			}
			if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_evidence(evidence_id,verification_id,catalog_item_id,expires_at) VALUES($1,$2,$3,$4)`, bindingEvidenceID, bindingVerificationID, catalogItemID, bindingExpiry); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_bindings(catalog_item_id,admission_version,evidence_id,effective_to) VALUES($1,1,$2,$3)`, catalogItemID, bindingEvidenceID, asOf); err != nil {
				t.Fatal(err)
			}
			if fixture.bindingMode == "ENDED_REVOKED" {
				if _, err := repository.pool.Exec(ctx, `INSERT INTO product_admission_revocations(revocation_id,evidence_id) VALUES($1,$2)`, uuid.New(), bindingEvidenceID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	productsAfter, err := repository.AdminFinanceProductsOverview(ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if productsAfter.ProductDraftCount != productsBefore.ProductDraftCount+1 || productsAfter.ProductOnSaleCount != productsBefore.ProductOnSaleCount+6 || productsAfter.ProductOffSaleCount != productsBefore.ProductOffSaleCount+1 || productsAfter.ProductDeletedCount != productsBefore.ProductDeletedCount+1 || productsAfter.AdmissionPendingCount != productsBefore.AdmissionPendingCount+1 || productsAfter.AdmissionBlockedCount != productsBefore.AdmissionBlockedCount+6 {
		t.Fatalf("missing admission or tombstone semantics changed: before=%+v after=%+v", productsBefore, productsAfter)
	}
}

func newFinanceOverviewIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string) *PostgresRepository {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "billing_overview_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = connection.Exec(cleanupContext, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	ddl := []string{
		`CREATE TABLE wallets(id UUID PRIMARY KEY,owner_type TEXT,owner_id UUID,status TEXT,posted_microcredits BIGINT NOT NULL DEFAULT 0,reserved_microcredits BIGINT NOT NULL DEFAULT 0,receivable_microcredits BIGINT NOT NULL DEFAULT 0,created_at TIMESTAMPTZ,updated_at TIMESTAMPTZ)`,
		`CREATE TABLE manual_adjustments(id UUID PRIMARY KEY,wallet_id UUID,status TEXT)`,
		`CREATE TABLE usage_holds(id UUID PRIMARY KEY,status TEXT,expires_at TIMESTAMPTZ)`,
		`CREATE TABLE payment_orders(id UUID PRIMARY KEY,merchant_order_no TEXT,requester_id UUID,payer_user_id UUID,wallet_id UUID,product_id TEXT,product_snapshot JSONB,channel TEXT,amount_fen BIGINT,currency TEXT,credit_microcredits BIGINT,status TEXT,fulfillment_status TEXT,expires_at TIMESTAMPTZ,provider_payable_until TIMESTAMPTZ,late_review_boundary_at TIMESTAMPTZ,final_archive_eligible_at TIMESTAMPTZ,created_at TIMESTAMPTZ,updated_at TIMESTAMPTZ)`,
		`CREATE TABLE payment_transactions(id UUID PRIMARY KEY,payment_order_id UUID,channel TEXT,provider_transaction_id TEXT,status TEXT,amount_fen BIGINT,currency TEXT,source TEXT,provider_paid_at TIMESTAMPTZ,verified_at TIMESTAMPTZ,created_at TIMESTAMPTZ,updated_at TIMESTAMPTZ)`,
		`CREATE TABLE payment_evidence(id UUID PRIMARY KEY,payment_transaction_id UUID)`,
		`CREATE TABLE late_payment_review_cases(id UUID PRIMARY KEY,review_status TEXT)`,
		`CREATE TABLE payment_anomaly_cases(id UUID PRIMARY KEY,status TEXT)`,
		`CREATE TABLE reconciliation_cases(id UUID PRIMARY KEY,status TEXT)`,
		`CREATE TABLE refunds(id UUID PRIMARY KEY,status TEXT)`,
		`CREATE TABLE payment_provider_controls(provider_id TEXT,provider_version TEXT,new_orders_enabled BOOLEAN,updated_at TIMESTAMPTZ)`,
		`CREATE TABLE payment_product_controls(provider_id TEXT,provider_version TEXT,product_id TEXT,new_orders_enabled BOOLEAN,updated_at TIMESTAMPTZ)`,
		`CREATE TABLE pricing_versions(id UUID PRIMARY KEY,model TEXT,input_per_million BIGINT,output_per_million BIGINT,cached_input_per_million BIGINT,effective_from TIMESTAMPTZ,effective_to TIMESTAMPTZ,created_at TIMESTAMPTZ,scope_type TEXT,scope_id TEXT,protocol_code TEXT,formula_version_id UUID,credit_unit TEXT,lifecycle_status TEXT,published_at TIMESTAMPTZ)`,
		`CREATE TABLE metering_unit_definitions(id UUID PRIMARY KEY,status TEXT)`,
		`CREATE TABLE recharge_product_catalog_items(catalog_item_id UUID PRIMARY KEY,product_id TEXT,display_name_zh_cn TEXT,display_name_en TEXT,display_name_ja TEXT,lifecycle_status TEXT,deleted_at TIMESTAMPTZ,created_at TIMESTAMPTZ,updated_at TIMESTAMPTZ)`,
		`CREATE TABLE product_admission_verifications(verification_id UUID PRIMARY KEY,catalog_item_id UUID,target_draft_version BIGINT,publish_fingerprint TEXT,status TEXT,requested_at TIMESTAMPTZ)`,
		`CREATE TABLE product_admission_evidence(evidence_id UUID PRIMARY KEY,verification_id UUID,catalog_item_id UUID,expires_at TIMESTAMPTZ)`,
		`CREATE TABLE product_admission_bindings(catalog_item_id UUID,admission_version BIGINT,evidence_id UUID,effective_to TIMESTAMPTZ)`,
		`CREATE TABLE product_admission_revocations(revocation_id UUID PRIMARY KEY,evidence_id UUID)`,
	}
	for index, statement := range ddl {
		if _, err := connection.Exec(ctx, statement); err != nil {
			t.Fatalf("create finance overview fixture table %d: %v", index, err)
		}
	}
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = make(map[string]string)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(fmt.Errorf("ping finance overview integration schema: %w", err))
	}
	return &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
}
