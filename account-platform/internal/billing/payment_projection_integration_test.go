package billing

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestPostgresPaymentReplayPreservesFinancialAttention(t *testing.T) {
	for _, kind := range []string{"forced_review", "chargeback_review", "chargeback_receivable"} {
		t.Run(kind, func(t *testing.T) {
			r, w, tid := forcedRefundPaidSource(t)
			ctx := context.Background()
			var oid uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT payment_order_id FROM payment_transactions WHERE id=$1`, tid).Scan(&oid); err != nil {
				t.Fatal(err)
			}
			if kind == "forced_review" {
				if _, err := r.RecordAndFreezeForcedRefund(ctx, tid, "controlled-summary-forced", 100, json.RawMessage(`{"confirmed":true}`)); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "chargeback_receivable" {
					h, err := r.Reserve(ctx, uuid.New(), w.ID, w.PostedMicrocredits, nil, time.Now().Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					if _, err = r.Commit(ctx, h.UsageRequestID, h.ID, w.PostedMicrocredits, map[string]any{"fixture": true}); err != nil {
						t.Fatal(err)
					}
				}
				cb, err := r.RecordConfirmedChargebackForReview(ctx, tid, "controlled-summary-chargeback", json.RawMessage(`{"confirmed":true}`))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "chargeback_receivable" {
					if _, err = r.ApplyConfirmedChargeback(ctx, cb.PaymentEvidenceID, "controlled-summary-chargeback"); err != nil {
						t.Fatal(err)
					}
				}
			}
			var beforeAttention string
			var beforeRefund json.RawMessage
			if err := r.pool.QueryRow(ctx, `SELECT attention_summary,refund_summary FROM payment_orders WHERE id=$1`, oid).Scan(&beforeAttention, &beforeRefund); err != nil {
				t.Fatal(err)
			}
			expected := "REVIEW_REQUIRED"
			if kind == "chargeback_receivable" {
				expected = "RECEIVABLE_OPEN"
			}
			if beforeAttention != expected {
				t.Fatalf("controlled starting facts not present: attention=%s", beforeAttention)
			}
			mappings, err := r.ActiveExternalProducts(ctx, "development_simulator", "development-simulator-v1", true)
			if err != nil {
				t.Fatal(err)
			}
			c, err := NewPaymentCoordinatorWithProvider(r, &legacyMappingProvider{}, mappings, "test")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			order, err := getPaymentOrderByID(ctx, tx, oid, true)
			if err != nil {
				t.Fatal(err)
			}
			var transactionID string
			var paidAt time.Time
			if err := tx.QueryRow(ctx, `SELECT provider_transaction_id,provider_paid_at FROM payment_transactions WHERE id=$1`, tid).Scan(&transactionID, &paidAt); err != nil {
				t.Fatal(err)
			}
			provider := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", ChannelTransactionID: &transactionID, PaidAt: &paidAt}
			if _, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "controlled-summary-repeat", nil, provider, false, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var afterAttention string
			var afterRefund json.RawMessage
			var reviews, debt int
			if err = r.pool.QueryRow(ctx, `SELECT attention_summary,refund_summary FROM payment_orders WHERE id=$1`, oid).Scan(&afterAttention, &afterRefund); err != nil {
				t.Fatal(err)
			}
			if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM forced_refund_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED')+(SELECT count(*) FROM chargeback_cases WHERE payment_order_id=$1 AND status='REVIEW_REQUIRED'),(SELECT count(*) FROM receivables WHERE payment_order_id=$1 AND outstanding_microcredits>0)`, oid).Scan(&reviews, &debt); err != nil {
				t.Fatal(err)
			}
			if afterAttention != expected {
				t.Errorf("valid duplicate payment erased financial attention: before=%s after=%s pending_reviews=%d open_debts=%d", beforeAttention, afterAttention, reviews, debt)
			}
			if kind == "forced_review" {
				var before, after map[string]json.RawMessage
				json.Unmarshal(beforeRefund, &before)
				json.Unmarshal(afterRefund, &after)
				if string(before["forced_review_count"]) != "1" {
					t.Fatal("initial forced count absent")
				}
				if string(after["forced_review_count"]) != "1" {
					t.Error("valid duplicate payment erased forced_review_count")
				}
			}
		})
	}
}
