package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestProductResumeDefaultProviderPostgres(t *testing.T) {
	t.Run("owner-proof-account-codec-and-executor-retain-zero-version", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, expected, order, _, _ := defaultProviderResumeFixture(t, r, ctx)
		// Build the real persisted failure-confirmation prerequisite through
		// its production observation, claim and confirmation writers.
		order.ProviderSKU = item.Mapping.ExternalSKUBOM
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku=$2 WHERE id=$1`, order.ID, order.ProviderSKU); err != nil {
			t.Fatal(err)
		}
		failed := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "REVIEW_REQUIRED", ProviderStatus: "FAILED", ProviderTraceID: "controlled-default-failure", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU}
		coordinator := &PaymentCoordinator{repository: r, now: r.now}
		if err := coordinator.recordProviderObservation(ctx, order.ID, "QUERY", failed); !errors.Is(err, ErrConflict) {
			t.Fatal("a raw FAILED in review must create its actual illegal-transition case", err)
		}
		cases, err := r.AdminPaymentAnomalies(ctx, PaymentAnomalyFilter{AnomalyType: "ILLEGAL_TRANSITION"}, 10)
		if err != nil || len(cases) != 1 {
			t.Fatal(cases, err)
		}
		actor := uuid.New()
		claimed, err := r.ClaimPaymentAnomaly(ctx, cases[0].ID, actor, "c09-claim-"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ConfirmProviderFailed(ctx, claimed.ID, actor, "c09-confirm-"+uuid.NewString(), "controlled verified failure confirmation", claimed.Version, r.now(), failed); err != nil {
			t.Fatal(err)
		}
		actual, err := r.productResumeVersions(ctx, expected)
		if err != nil {
			t.Fatal(err)
		}
		command, err := json.Marshal(actual)
		if err != nil {
			t.Fatal(err)
		}
		financial, _ := json.Marshal(map[string]any{"new_orders_enabled": false, "provider_control_version": actual.ProviderControlVersion, "product_control_version": actual.ProductControlVersion})
		frozen := approvalencoding.LegacyProofInput{CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_PRODUCT_CONTROL", TargetResourceID: item.ProductID, NormalizedCommand: command, CriticalResourceVersions: command, FinancialSnapshot: financial}
		current, reason, ownerState, err := currentBillingProofInput(ctx, r, frozen)
		if err != nil || reason != "" || ownerState != "confirmed:true" {
			t.Fatal("actual owner facts must permit the already-confirmed product draft", reason, ownerState, err)
		}
		artifacts, err := approvalencoding.ConvertLegacyProofInput(current)
		if err != nil {
			t.Fatal("owner proof conversion must accept the actual default-provider facts", err)
		}
		// Account and the authorization consumer use this same production
		// codec. This test does not replace it with a fake JSON projection.
		decoded, err := approvalencoding.DecodeProofInput(current.CommandType, current.PolicyVersion, artifacts.ProofInputJSON)
		if err != nil || decoded.CommandDigest != artifacts.CommandDigest || decoded.SnapshotDigest != artifacts.SnapshotDigest {
			t.Fatal("actual Account proof-input codec rejected the owner facts", err)
		}
		legacy, err := approvalencoding.LegacyProofInputFromArtifacts(decoded)
		if err != nil || json.Unmarshal(legacy.NormalizedCommand, &expected) != nil || expected.ProviderControlVersion != 0 {
			t.Fatal("executor must retain the exact approved virtual version", expected, err)
		}
		// The approval freezes the first probe; execution must get a distinct
		// subsequent recorded probe rather than replaying its frozen watermark.
		attempt, err := r.beginProviderAttempt(ctx, order.ProviderID, order.ProviderVersion, "ORDER_QUERY", order.Channel, "controlled-after-proof-query", ProviderCallScope{OrderID: order.ID, ProductID: order.ProductID, SubjectID: order.RequesterID, TrustedProbe: true})
		if err != nil {
			t.Fatal(err)
		}
		if err = r.finishProviderAttempt(ctx, attempt, "SUCCESS", nil); err != nil {
			t.Fatal(err)
		}
		probe, err := latestTrustedProbeForOrder(ctx, r.pool, order.ID)
		if err != nil || probe == nil || expected.ObservationHighWatermark == nil || *probe == *expected.ObservationHighWatermark {
			t.Fatal("execution needs an actual fresh post-proof observation", probe, err)
		}
		order, err = getPaymentOrderByID(ctx, r.pool, order.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.executeProductResume(ctx, expected, *probe, uuid.New(), uuid.New(), uuid.New(), "controlled proof-encoded product recovery", order, failed)
		if err != nil || result.Status != "RESUMED" {
			t.Fatal(result, err)
		}
		defaultProviderResumeAssert(t, r, ctx, item, 1, true, 2, 1)
	})
	t.Run("draft-observes-missing-provider-as-available-without-writing", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, expected, _, _, _ := defaultProviderResumeFixture(t, r, ctx)
		actual, err := r.productResumeVersions(ctx, expected)
		if err != nil || actual.ProviderControlVersion != 0 || actual.ProductControlVersion != 1 || actual.CatalogItemID != item.CatalogItemID || actual.ProbeOrderID != expected.ProbeOrderID || actual.PolicyVersion != paymentStopLossPolicyVersion {
			t.Fatal("default available provider must produce an unchanged approval draft", actual, err)
		}
		defaultProviderResumeAssert(t, r, ctx, item, 0, false, 1, 0)
	})
	t.Run("approved-default-state-materializes-and-resumes-atomically", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, expected, order, provider, probe := defaultProviderResumeFixture(t, r, ctx)
		actual, err := r.productResumeVersions(ctx, expected)
		if err != nil {
			t.Fatal(err)
		}
		// This is the actual domain executor with a real recorded probe, not
		// an Account/Keycloak two-person approval end-to-end test.
		result, err := r.executeProductResume(ctx, actual, probe, uuid.New(), uuid.New(), uuid.New(), "controlled approved default provider recovery", order, provider)
		if err != nil || result.Status != "RESUMED" {
			t.Fatal(result, err)
		}
		defaultProviderResumeAssert(t, r, ctx, item, 1, true, 2, 1)
		var snapshotVersion int
		if err = r.pool.QueryRow(ctx, `SELECT (control_versions_before->>'provider_control_version')::integer FROM payment_product_resume_executions WHERE id=$1`, result.ExecutionID).Scan(&snapshotVersion); err != nil || snapshotVersion != 0 {
			t.Fatal("immutable execution must retain the actually approved virtual version", snapshotVersion, err)
		}
	})
	t.Run("invalid-probe-rolls-back-provider-materialization", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, expected, order, provider, _ := defaultProviderResumeFixture(t, r, ctx)
		_, err := r.executeProductResume(ctx, expected, uuid.New(), uuid.New(), uuid.New(), uuid.New(), "controlled invalid fresh probe", order, provider)
		if err == nil {
			t.Fatal("missing probe must not resume the product")
		}
		defaultProviderResumeAssert(t, r, ctx, item, 0, false, 1, 0)
	})
	for _, enabled := range []bool{false, true} {
		name := "paused"
		if enabled {
			name = "available"
		}
		t.Run("stale-default-approval-rejects-existing-"+name+"-row", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			item, expected, order, provider, probe := defaultProviderResumeFixture(t, r, ctx)
			if _, err := r.pool.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason_code,reason,updated_at) VALUES($1,$2,$3,'AVAILABLE','controlled new version',now())`, item.Mapping.ProviderID, item.Mapping.ProviderVersion, enabled); err != nil {
				t.Fatal(err)
			}
			_, err := r.executeProductResume(ctx, expected, probe, uuid.New(), uuid.New(), uuid.New(), "stale default version", order, provider)
			if !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatal("an existing row is a different approval snapshot, even when available", err)
			}
			defaultProviderResumeAssert(t, r, ctx, item, 1, false, 1, 0)
			var after bool
			if err = r.pool.QueryRow(ctx, `SELECT new_orders_enabled FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, item.Mapping.ProviderID, item.Mapping.ProviderVersion).Scan(&after); err != nil || after != enabled {
				t.Fatal("stale resume must never overwrite provider control", after, err)
			}
			current, err := r.productResumeVersions(ctx, expected)
			if enabled && (err != nil || current.ProviderControlVersion != 1) || !enabled && err == nil {
				t.Fatal("fresh draft must respect persisted provider state", current, err)
			}
		})
	}
	t.Run("concurrent-provider-pause-cannot-be-overwritten-by-default-resume", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, expected, order, provider, probe := defaultProviderResumeFixture(t, r, ctx)
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err = blocker.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason_code,reason,updated_at) VALUES($1,$2,FALSE,'MANUAL_SAFETY_PAUSE','controlled concurrent pause',now())`, item.Mapping.ProviderID, item.Mapping.ProviderVersion); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := r.executeProductResume(ctx, expected, probe, uuid.New(), uuid.New(), uuid.New(), "controlled concurrent default recovery", order, provider)
			result <- err
		}()
		deadline := time.Now().Add(4 * time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO payment_provider_controls%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("recovery did not reach the actual missing-row uniqueness barrier")
		}
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-result:
			if err == nil {
				t.Fatal("concurrent provider pause must invalidate the old default snapshot")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("recovery did not finish after the actual pause committed")
		}
		defaultProviderResumeAssert(t, r, ctx, item, 1, false, 1, 0)
		var enabled bool
		if err = r.pool.QueryRow(ctx, `SELECT new_orders_enabled FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, item.Mapping.ProviderID, item.Mapping.ProviderVersion).Scan(&enabled); err != nil || enabled {
			t.Fatal("concurrent pause must survive", enabled, err)
		}
	})
	t.Run("persisted-available-provider-still-resumes-with-its-exact-version", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _, expected := admissionRetirementResumeFixture(t, r, ctx)
		order, provider, probe := admissionRetirementFreshResumeProbe(t, r, ctx, item, &expected)
		result, err := r.executeProductResume(ctx, expected, probe, uuid.New(), uuid.New(), uuid.New(), "controlled persisted recovery", order, provider)
		if err != nil || result.Status != "RESUMED" {
			t.Fatal(result, err)
		}
		defaultProviderResumeAssert(t, r, ctx, item, 1, true, 2, 1)
	})
}

func defaultProviderResumeFixture(t *testing.T, r *PostgresRepository, ctx context.Context) (RechargeProductCatalogItem, ProductResumeVersions, PaymentOrder, ProviderOrder, uuid.UUID) {
	t.Helper()
	item, _, expected := admissionRetirementResumeFixture(t, r, ctx)
	// Each test owns a fresh database. Remove only its controlled setup row to
	// represent the production initial state, never a retained or deployed row.
	if _, err := r.pool.Exec(ctx, `DELETE FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, item.Mapping.ProviderID, item.Mapping.ProviderVersion); err != nil {
		t.Fatal(err)
	}
	expected.ProviderControlVersion = 0
	order, provider, probe := admissionRetirementFreshResumeProbe(t, r, ctx, item, &expected)
	return item, expected, order, provider, probe
}

func defaultProviderResumeAssert(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem, providerRows int, productEnabled bool, productVersion, executions int) {
	t.Helper()
	var rows, version, executed, outbox int
	var enabled bool
	err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2),(SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$3),(SELECT version FROM payment_product_controls WHERE product_id=$3),(SELECT count(*) FROM payment_product_resume_executions WHERE product_id=$3),(SELECT count(*) FROM outbox_events WHERE event_type='billing.payment-product.resumed.v1')`, item.Mapping.ProviderID, item.Mapping.ProviderVersion, item.ProductID).Scan(&rows, &enabled, &version, &executed, &outbox)
	if err != nil || rows != providerRows || enabled != productEnabled || version != productVersion || executed != executions || outbox != executions {
		t.Fatalf("unexpected committed control/execution/outbox: rows=%d enabled=%v version=%d executions=%d outbox=%d err=%v", rows, enabled, version, executed, outbox, err)
	}
}
