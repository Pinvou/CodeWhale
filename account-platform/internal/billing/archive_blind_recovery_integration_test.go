package billing

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestArchiveBlindEscalationHonorsProviderTerminalCapabilityAndRepairsLegacyState(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	now := time.Date(2026, time.September, 23, 8, 0, 0, 0, time.UTC)
	repository.now = func() time.Time { return now }

	requesterID := uuid.New()
	wallet, _, err := repository.EnsureWallet(ctx, "PERSONAL", requesterID)
	if err != nil {
		t.Fatal(err)
	}
	nonAuthoritative := seedArchiveBlindReviewCase(t, ctx, repository, wallet.ID, requesterID, "non-authoritative", false, now)
	authoritative := seedArchiveBlindReviewCase(t, ctx, repository, wallet.ID, requesterID, "authoritative", true, now)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason,reason_code,updated_at) VALUES($1,$2,FALSE,'operator safety pause','MANUAL_SAFETY_PAUSE',$3)`, authoritative.ProviderID, authoritative.ProviderVersion, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	escalated, err := repository.EscalateArchiveBlindCases(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if escalated != 1 {
		t.Fatalf("escalated cases = %d, want 1", escalated)
	}
	assertLateReviewStatus(t, ctx, repository, nonAuthoritative.CaseID, "REVIEW_REQUIRED_LATE_PAYMENT")
	assertLateReviewStatus(t, ctx, repository, authoritative.CaseID, "PROVIDER_BLIND")
	var preservedReasonCode string
	if err := repository.pool.QueryRow(ctx, `SELECT reason_code FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, authoritative.ProviderID, authoritative.ProviderVersion).Scan(&preservedReasonCode); err != nil {
		t.Fatal(err)
	}
	if preservedReasonCode != "MANUAL_SAFETY_PAUSE" {
		t.Fatalf("archive escalation replaced an existing safety pause with %s", preservedReasonCode)
	}

	markLegacyArchiveBlindMisclassification(t, ctx, repository, nonAuthoritative, now)
	items, err := repository.ListAutomaticallyMisclassifiedArchiveBlindCases(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].CaseID != nonAuthoritative.CaseID {
		t.Fatalf("recovery candidates = %#v", items)
	}
	probeAt := now.Add(time.Minute)
	if _, err := repository.pool.Exec(ctx, `UPDATE payment_orders SET last_successful_query_at=$2 WHERE id=$1`, nonAuthoritative.PaymentOrderID, probeAt); err != nil {
		t.Fatal(err)
	}
	repository.now = func() time.Time { return probeAt.Add(time.Minute) }
	retracted, err := repository.RetractAutomaticallyMisclassifiedArchiveBlindCase(ctx, items[0])
	if err != nil || !retracted {
		t.Fatalf("retract legacy archive blind case: retracted=%v err=%v", retracted, err)
	}
	assertLateReviewStatus(t, ctx, repository, nonAuthoritative.CaseID, "REVIEW_REQUIRED_LATE_PAYMENT")
	var enabled bool
	var reasonCode string
	if err := repository.pool.QueryRow(ctx, `SELECT new_orders_enabled,reason_code FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, nonAuthoritative.ProviderID, nonAuthoritative.ProviderVersion).Scan(&enabled, &reasonCode); err != nil {
		t.Fatal(err)
	}
	if !enabled || reasonCode != "AVAILABLE" {
		t.Fatalf("provider control after repair = enabled:%v reason:%s", enabled, reasonCode)
	}
}

func seedArchiveBlindReviewCase(t *testing.T, ctx context.Context, repository *PostgresRepository, walletID, requesterID uuid.UUID, suffix string, authoritative bool, now time.Time) ArchiveBlindRecoveryCandidate {
	t.Helper()
	orderID, caseID := uuid.New(), uuid.New()
	providerID := "archive-blind-" + suffix
	providerVersion := "v1"
	capability, _ := json.Marshal(map[string]any{"authoritative_unpaid_terminal": authoritative})
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_orders(
id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,
late_review_boundary_at,final_archive_eligible_at,last_successful_query_at,created_at,updated_at,
provider_id,provider_version,provider_capability_snapshot)
VALUES($1,$2,$3,$3,$4,'ARCHIVE_BLIND_TEST','{}','ALIPAY',100,'CNY',100000,'MANUAL_REVIEW','MANUAL_REVIEW',$5,$5,$6,$7,$8,$9,$9,$10,$11,$12)`,
		orderID, "archive-blind-"+uuid.NewString(), requesterID, walletID,
		now.Add(-8*24*time.Hour), now.Add(-5*24*time.Hour), now.Add(-time.Hour), now.Add(-2*time.Hour), now.Add(-9*24*time.Hour),
		providerID, providerVersion, capability); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'BILLING','LATE_PAYMENT','PAYMENT_ORDER',$2,'P2','OPEN',$3,$4,$4,$4,$4,$3)`, caseID, orderID, now.Add(-4*24*time.Hour), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO late_payment_review_cases(id,payment_order_id,review_status,provider_id,provider_version,merchant_order_no,provider_capability_snapshot,policy_snapshot,evidence_summary,last_query_at,last_query_result,next_review_at,created_at,updated_at) VALUES($1,$2,'REVIEW_REQUIRED_LATE_PAYMENT',$3,$4,$5,$6,'{}','{}',$7,'PENDING',$8,$9,$9)`, caseID, orderID, providerID, providerVersion, "archive-blind-"+suffix, capability, now.Add(-2*time.Hour), now.Add(time.Hour), now.Add(-4*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	lastSuccessful := now.Add(-2 * time.Hour)
	return ArchiveBlindRecoveryCandidate{CaseID: caseID, PaymentOrderID: orderID, ProviderID: providerID, ProviderVersion: providerVersion, LastSuccessfulQueryAt: &lastSuccessful}
}

func markLegacyArchiveBlindMisclassification(t *testing.T, ctx context.Context, repository *PostgresRepository, item ArchiveBlindRecoveryCandidate, now time.Time) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"reason": automaticArchiveBlindReason, "new_orders_enabled": false})
	digest := strings.Repeat("a", 64)
	if _, err := repository.pool.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='PROVIDER_BLIND',blind_reason=$2,updated_at=$3 WHERE id=$1`, item.CaseID, automaticArchiveBlindReason, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'PROVIDER_BLIND',$3,$4,$5)`, uuid.New(), item.CaseID, digest, payload, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason,reason_code,updated_at) VALUES($1,$2,FALSE,$3,'PROVIDER_BLIND',$4)`, item.ProviderID, item.ProviderVersion, automaticArchiveBlindReason, now); err != nil {
		t.Fatal(err)
	}
}

func assertLateReviewStatus(t *testing.T, ctx context.Context, repository *PostgresRepository, caseID uuid.UUID, want string) {
	t.Helper()
	var got string
	if err := repository.pool.QueryRow(ctx, `SELECT review_status FROM late_payment_review_cases WHERE id=$1`, caseID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("late review status = %s, want %s", got, want)
	}
}
