package token

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

// Actual public Token and Billing migrations, service-role repositories and
// Billing HTTP. Only the historical execution/response facts are controlled.
func TestReconciliationEvidenceMatchesPublicSchema(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, billingPool := retainedManagementBillingRepository(t)
	server := httptest.NewServer(billing.NewService(funds).Handler("controlled-reconciliation-evidence"))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-reconciliation-evidence")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy split", "persisted pre-generation rejection", "ambiguous response does not release"} {
		t.Run(name, func(t *testing.T) {
			user := uuid.New()
			wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = funds.Credit(ctx, wallet.ID, "reconciliation-evidence-"+user.String(), 1000, false); err != nil {
				t.Fatal(err)
			}
			id := retainedCancellationUsage(t, r, ctx, user, wallet.ID, nil, "SETTLEMENT_PENDING")
			hold, err := funds.Reserve(ctx, id, wallet.ID, 100, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET status='UPSTREAM_ACTIVE',settlement_status='USAGE_PENDING',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL,hold_id=$2 WHERE id=$1`, id, hold.ID); err != nil {
				t.Fatal(err)
			}
			if name == "legacy split" {
				before := f05DeadlineBillingFacts(t, ctx, billingPool)
				count, err := r.RecoverLegacyResponseStateSplits(ctx, 10)
				if err != nil || count != 1 {
					t.Fatalf("real legacy recovery: count=%d err=%v", count, err)
				}
				got, err := r.GetUsage(ctx, id)
				if err != nil || got.Status != "REVIEW" || got.ExecutionStatus != "SETTLEMENT_PENDING" || got.SettlementStatus != "USAGE_PENDING" {
					t.Fatalf("legacy recovery changed wrong states: %+v err=%v", got, err)
				}
				var kind, source, decision string
				var snapshot []byte
				if err := r.pool.QueryRow(ctx, `SELECT evidence_type,source_type,decision,sanitized_snapshot FROM usage_convergence_evidence WHERE usage_request_id=$1`, id).Scan(&kind, &source, &decision, &snapshot); err != nil {
					t.Fatal(err)
				}
				if kind != "UNCERTAINTY" || source != "RECONCILER" || decision != "REVIEW" || !strings.Contains(string(snapshot), "LEGACY_RESPONSE_STATE_SPLIT") {
					t.Fatalf("legacy reason/evidence lost: %s/%s/%s %s", kind, source, decision, snapshot)
				}
				if count, err = r.RecoverLegacyResponseStateSplits(ctx, 10); err != nil || count != 0 {
					t.Fatalf("legacy replay not idempotent: %d %v", count, err)
				}
				var evidenceCount int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1`, id).Scan(&evidenceCount); err != nil || evidenceCount != 1 {
					t.Fatalf("duplicate evidence: %d %v", evidenceCount, err)
				}
				if !reflect.DeepEqual(before, f05DeadlineBillingFacts(t, ctx, billingPool)) {
					t.Fatal("legacy state repair wrote financial facts")
				}
				return
			}
			response := replayResponseObservation{ResponseDisposition: responseDispositionRejectedBeforeGeneration, HTTPStatus: 400}
			if name == "ambiguous response does not release" {
				response = replayResponseObservation{HTTPStatus: 500}
			}
			observation, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE usage_requests SET status='REVIEW',execution_status='REVIEW',usage_snapshot=$2 WHERE id=$1`, id, observation); err != nil {
				t.Fatal(err)
			}
			current, err := r.GetUsage(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			item := ReconciliationItem{UsageRequest: current, UsageSnapshot: observation}
			reconciler := NewUsageReconciler(r, client, nil, time.Second)
			if name == "ambiguous response does not release" {
				before := f05DeadlineBillingFacts(t, ctx, billingPool)
				if err := reconciler.reconcile(ctx, item); err == nil {
					t.Fatal("ambiguous observation treated as conclusive rejection")
				}
				if !reflect.DeepEqual(before, f05DeadlineBillingFacts(t, ctx, billingPool)) {
					t.Fatal("ambiguous observation released or changed financial facts")
				}
				return
			}
			if err := reconciler.reconcile(ctx, item); err != nil {
				t.Fatalf("persisted rejection could not release real hold: %v", err)
			}
			got, err := r.GetUsage(ctx, id)
			if err != nil || got.Status != "RELEASED" || got.SettlementStatus != "RELEASED" {
				t.Fatalf("Token did not converge release: %+v %v", got, err)
			}
			balance, err := funds.GetWallet(ctx, wallet.ID)
			if err != nil || balance.ReservedMicrocredits != 0 || balance.AvailableMicrocredits != 1000 {
				t.Fatalf("real wallet release incorrect: %+v %v", balance, err)
			}
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_type='RESULT_OBSERVED' AND source_type='GATEWAY' AND decision='RECORDED' AND evidence_key='http:400'`, id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("rejection proof missing: %d %v", count, err)
			}
			after := f05DeadlineBillingFacts(t, ctx, billingPool)
			if err := reconciler.reconcile(ctx, item); err != nil {
				t.Fatalf("same rejection replay failed: %v", err)
			}
			if !reflect.DeepEqual(after, f05DeadlineBillingFacts(t, ctx, billingPool)) {
				t.Fatal("rejection replay changed financial facts")
			}
		})
	}
}
