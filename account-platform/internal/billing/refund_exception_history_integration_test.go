package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func refundHistoryFinancialSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	facts := entitlementFinancialFactsSnapshot(t, ctx, r)
	for _, table := range []string{"refund_exception_repair_cases", "refund_grant_reservations", "payment_anomaly_cases", "payment_anomaly_commands"} {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text),'[]'::jsonb)::text FROM `+pgx.Identifier{table}.Sanitize()+` f`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		facts[table] = raw
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func historyRefundInput(plan RefundExceptionRepairPlan, activation int64) RefundExceptionRepairExecutionInput {
	request := uuid.New()
	return RefundExceptionRepairExecutionInput{
		ApprovalRequestID: request, AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ExecutionKey: "refund-repair:" + request.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation,
		Command: RefundExceptionRepairCommandInput{RefundID: plan.RefundID, PaymentEvidenceID: plan.EvidenceID, AnomalyCaseID: plan.AnomalyCaseID},
		Frozen: RefundExceptionRepairFrozenFacts{RefundUpdatedAt: plan.RefundUpdatedAt, AnomalyVersion: plan.AnomalyVersion,
			AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderRefundID: plan.ProviderRefundID,
			RevokedMicrocredits: plan.RevokedMicrocredits, ShortfallMicrocredits: plan.ShortfallMicrocredits,
			WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PackageID: plan.PackageID, FactsDigest: plan.FactsDigest},
	}
}

// The provider success baseline is controlled persisted evidence, not a cash
// operation. Subsequent case creation, claim, dismissal and repair are real
// repository operations on public migrations under billing_service.
func TestRefundExceptionDisprovedHistoryPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if raw == "" {
		t.Skip("exclusive PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, _ := newAdminPaginationIntegrationRepository(t, ctx, raw)
	activation := refundFixtureActivation(t, ctx, r.pool)("ACTIVE")
	setup := func(t *testing.T, kind string, dismiss bool) (RefundExceptionRepairPlan, uuid.UUID, uuid.UUID) {
		t.Helper()
		f := seedRefundException(t, ctx, r, false)
		command := RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: f.EvidenceID, AnomalyCaseID: f.AnomalyCaseID}
		plan, err := refundExceptionRepairPlanFrom(ctx, r.pool, command, 1, 1, activation)
		if err != nil {
			t.Fatal(err)
		}
		var caseID, anomaly uuid.UUID
		if kind == "CHARGEBACK" {
			c, err := r.RecordConfirmedChargebackForReview(ctx, plan.PaymentTransactionID, uuid.NewString(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			caseID, anomaly = c.ID, *c.ManualReviewCaseID
		} else {
			c, err := r.RecordAndFreezeForcedRefund(ctx, plan.PaymentTransactionID, uuid.NewString(), 100, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			caseID, anomaly = c.ID, *c.ManualReviewCaseID
		}
		if dismiss {
			actor := uuid.New()
			if _, err := r.ClaimPaymentAnomaly(ctx, anomaly, actor, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, anomaly, actor, uuid.NewString(), "DISMISS", "disproved independent provider review", "NONE"); err != nil {
				t.Fatal(err)
			}
		}
		return plan, caseID, anomaly
	}
	for _, kind := range []string{"CHARGEBACK", "FORCED_REFUND"} {
		t.Run(kind+"-dismissed-real-repair-and-closure", func(t *testing.T) {
			old, _, _ := setup(t, kind, true)
			command := historyRefundInput(old, activation).Command
			plan, err := refundExceptionRepairPlanFrom(ctx, r.pool, command, 1, 1, activation)
			if err != nil || plan.RevokedMicrocredits != 100 || plan.ShortfallMicrocredits != 0 {
				t.Fatal("fresh disproved-case plan", plan, err)
			}
			// The pre-history approval cannot silently acquire this new eligibility.
			if _, err := r.ApplyRefundExceptionRepair(ctx, historyRefundInput(old, activation)); !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatal("old approval facts remained valid", err)
			}
			input := historyRefundInput(plan, activation)
			result, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || result.Status != "SUCCEEDED" || result.ReceivableID != nil || result.RevokedMicrocredits != 100 {
				t.Fatal("actual repair", result, err)
			}
			replay, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || replay != result {
				t.Fatal("original receipt replay", replay, err)
			}
			var applied bool
			var owner uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT a.financial_action_status='APPLIED' AND f.status='SUCCEEDED' AND w.posted_microcredits=0,
 o.payer_user_id FROM payment_anomaly_cases a JOIN refunds f ON f.id=a.refund_id JOIN payment_orders o ON o.id=f.payment_order_id JOIN wallets w ON w.id=o.wallet_id WHERE a.id=$1`, plan.AnomalyCaseID).Scan(&applied, &owner); err != nil || !applied {
				t.Fatal("repair did not clear the actual pending money", applied, err)
			}
			assessment, err := r.AccountClosureAssessment(ctx, owner)
			if err != nil || !assessment.Ready || len(assessment.Blockers) != 0 {
				t.Fatal("repaired source still blocks closure", assessment, err)
			}
		})
		for _, negative := range []string{"open-review", "missing-receipt", "malformed-receipt", "journal", "receivable", "anomaly-pending", "repair-receipt",
			"linked-receivable-open", "linked-receivable-settled", "linked-receivable-unidentified", "unlinked-package-receivable", "unlinked-transaction-receivable", "revoked-base", "revoked-bonus", "lot-journal"} {
			if negative == "repair-receipt" && kind != "CHARGEBACK" {
				continue
			}
			t.Run(kind+"-deny-"+negative, func(t *testing.T) {
				plan, id, anomaly := setup(t, kind, negative != "open-review" && negative != "missing-receipt" && negative != "malformed-receipt")
				reference := "forced-refund:" + id.String()
				if kind == "CHARGEBACK" {
					if err := r.pool.QueryRow(ctx, `SELECT 'payment-transaction:'||payment_transaction_id::text||':chargeback:'||provider_chargeback_id FROM chargeback_cases WHERE id=$1`, id).Scan(&reference); err != nil {
						t.Fatal(err)
					}
				}
				// Deliberately incomplete historical facts are negative fixture input;
				// no incomplete row is used as a successful repair receipt.
				var err error
				switch negative {
				case "missing-receipt", "malformed-receipt":
					_, err = r.pool.Exec(ctx, `UPDATE payment_anomaly_cases SET status='RESOLVED',resolution_code='DISMISS',financial_action_status='NONE',resolved_at=now() WHERE id=$1`, anomaly)
					if err == nil && kind == "CHARGEBACK" {
						_, err = r.pool.Exec(ctx, `UPDATE chargeback_cases SET status='DISMISSED',resolution_code='DISMISS',version=version+1,resolved_at=now() WHERE id=$1`, id)
					} else if err == nil {
						_, err = r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolution_code='DISMISS',version=version+1,resolved_at=now() WHERE id=$1`, id)
					}
					if err == nil && negative == "malformed-receipt" {
						_, err = r.pool.Exec(ctx, `INSERT INTO payment_anomaly_commands(id,payment_anomaly_case_id,actor_id,idempotency_key,request_hash,action,reason,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,'DISMISS','legacy malformed receipt','{}','{}',now())`, uuid.New(), anomaly, uuid.New(), uuid.NewString(), strings.Repeat("ab", 32))
					}
				case "journal":
					_, err = r.pool.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'CHARGEBACK',$2,'historical effect',now())`, uuid.New(), reference)
				case "receivable":
					_, err = r.pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,payment_order_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,$3,'FORCED_REFUND',$4,1,1,'OPEN',now(),now())`, uuid.New(), plan.WalletID, plan.PaymentOrderID, reference)
				case "linked-receivable-open", "linked-receivable-settled", "linked-receivable-unidentified", "unlinked-package-receivable", "unlinked-transaction-receivable":
					rid := uuid.New()
					// Explicit foreign keys, not a conventional reference spelling,
					// own historical effects. Settled debt is still financial history.
					var packageID, transactionID *uuid.UUID
					if negative != "unlinked-transaction-receivable" && negative != "linked-receivable-unidentified" {
						packageID = &plan.PackageID
					}
					if negative != "unlinked-package-receivable" && negative != "linked-receivable-unidentified" {
						transactionID = &plan.PaymentTransactionID
					}
					settled := int64(0)
					status := "OPEN"
					if negative == "linked-receivable-settled" {
						settled, status = 1, "SETTLED"
					}
					_, err = r.pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,payment_order_id,payment_transaction_id,entitlement_package_id,source_type,source_reference,original_microcredits,settled_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8::bigint,1-$8::bigint,$9,now(),now())`, rid, plan.WalletID, plan.PaymentOrderID, transactionID, packageID, kind, uuid.NewString(), settled, status)
					if err == nil && strings.HasPrefix(negative, "linked-") {
						table := "chargeback_cases"
						if kind == "FORCED_REFUND" {
							table = "forced_refund_cases"
						}
						_, err = r.pool.Exec(ctx, `UPDATE `+table+` SET receivable_id=$2 WHERE id=$1`, id, rid)
					}
				case "revoked-base", "revoked-bonus":
					table, column := "chargeback_cases", "revoked_base_microcredits"
					if kind == "FORCED_REFUND" {
						table = "forced_refund_cases"
					}
					if negative == "revoked-bonus" {
						column = "revoked_bonus_microcredits"
					}
					_, err = r.pool.Exec(ctx, `UPDATE `+table+` SET `+column+`=1 WHERE id=$1`, id)
				case "lot-journal":
					tx, beginErr := r.pool.Begin(ctx)
					if beginErr != nil {
						t.Fatal(beginErr)
					}
					defer tx.Rollback(ctx)
					journal := uuid.New()
					var lot, account uuid.UUID
					err = tx.QueryRow(ctx, `SELECT component.credit_lot_id,account.id FROM entitlement_components component JOIN ledger_accounts account ON account.wallet_id=$2 AND account.account_type='WALLET_LIABILITY' WHERE component.package_id=$1 LIMIT 1`, plan.PackageID, plan.WalletID).Scan(&lot, &account)
					if err == nil {
						err = insertJournal(ctx, tx, journal, kind, uuid.NewString(), "legacy reference clawback", r.now())
					}
					if err == nil {
						err = insertPosting(ctx, tx, journal, account, -1, &lot, r.now())
					}
					if err == nil {
						err = insertPosting(ctx, tx, journal, refundClearingAccount, 1, nil, r.now())
					}
					if err == nil {
						err = tx.Commit(ctx)
					}
				case "anomaly-pending":
					_, err = r.pool.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status='PENDING' WHERE id=$1`, anomaly)
				case "repair-receipt":
					journal, receivable := uuid.New(), uuid.New()
					// Receipt identity must block even if its referenced accounting
					// rows carry an unexpected legacy business-reference spelling.
					_, err = r.pool.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'CHARGEBACK',$2,'legacy repair receipt',now())`, journal, uuid.NewString())
					if err == nil {
						_, err = r.pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,payment_order_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,$3,'CHARGEBACK',$4,1,1,'OPEN',now(),now())`, receivable, plan.WalletID, plan.PaymentOrderID, uuid.NewString())
					}
					if err == nil {
						_, err = r.pool.Exec(ctx, `INSERT INTO chargeback_financial_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,chargeback_case_id,anomaly_case_id,wallet_id,package_id,payment_evidence_id,base_shortfall_microcredits,component_digest,facts_digest,journal_entry_id,receivable_id,state) SELECT $1,$2,$3,2,$4,c.id,$5,$6,$7,c.payment_evidence_id,1,$4,$4,$8,$9,'EXECUTED' FROM chargeback_cases c WHERE c.id=$10`, uuid.New(), uuid.New(), uuid.New(), strings.Repeat("ab", 32), anomaly, plan.WalletID, plan.PackageID, journal, receivable, id)
					}
				}
				if err != nil {
					t.Fatal("negative history fixture", err)
				}
				before := refundHistoryFinancialSnapshot(t, r, ctx)
				if _, err := refundExceptionRepairPlanFrom(ctx, r.pool, historyRefundInput(plan, activation).Command, 1, 1, activation); !errors.Is(err, ErrApprovalSnapshotMismatch) {
					t.Fatal("negative history admitted into plan", err)
				}
				if _, err := r.ApplyRefundExceptionRepair(ctx, historyRefundInput(plan, activation)); !errors.Is(err, ErrApprovalSnapshotMismatch) || refundHistoryFinancialSnapshot(t, r, ctx) != before {
					t.Fatal("negative history wrote financial effects", err)
				}
			})
		}
	}
	t.Run("new-history-after-approval-requires-fresh-snapshot", func(t *testing.T) {
		old, _, _ := setup(t, "CHARGEBACK", true)
		plan, err := refundExceptionRepairPlanFrom(ctx, r.pool, historyRefundInput(old, activation).Command, 1, 1, activation)
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.RecordAndFreezeForcedRefund(ctx, plan.PaymentTransactionID, uuid.NewString(), 100, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		before := refundHistoryFinancialSnapshot(t, r, ctx)
		if _, err := r.ApplyRefundExceptionRepair(ctx, historyRefundInput(plan, activation)); !errors.Is(err, ErrApprovalSnapshotMismatch) || refundHistoryFinancialSnapshot(t, r, ctx) != before {
			t.Fatal("new open case did not fence original approval", err)
		}
		actor := uuid.New()
		if _, err := r.ClaimPaymentAnomaly(ctx, *c.ManualReviewCaseID, actor, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ResolvePaymentAnomaly(ctx, *c.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "disproved second provider review", "NONE"); err != nil {
			t.Fatal(err)
		}
		before = refundHistoryFinancialSnapshot(t, r, ctx)
		if _, err := r.ApplyRefundExceptionRepair(ctx, historyRefundInput(plan, activation)); !errors.Is(err, ErrApprovalSnapshotMismatch) || refundHistoryFinancialSnapshot(t, r, ctx) != before {
			t.Fatal("new dismissal silently widened frozen approval", err)
		}
		fresh, err := refundExceptionRepairPlanFrom(ctx, r.pool, historyRefundInput(plan, activation).Command, 1, 1, activation)
		if err != nil || fresh.FactsDigest == plan.FactsDigest {
			t.Fatal("combined history was not freshly frozen", fresh, err)
		}
		if _, err := r.ApplyRefundExceptionRepair(ctx, historyRefundInput(fresh, activation)); err != nil {
			t.Fatal("fresh combined-history approval could not repair", err)
		}
	})
	t.Run("concurrent-same-approval-has-one-effect", func(t *testing.T) {
		old, _, _ := setup(t, "CHARGEBACK", true)
		plan, err := refundExceptionRepairPlanFrom(ctx, r.pool, historyRefundInput(old, activation).Command, 1, 1, activation)
		if err != nil {
			t.Fatal(err)
		}
		input := historyRefundInput(plan, activation)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := r.ApplyRefundExceptionRepair(ctx, input)
				errs <- err
			}()
		}
		for range 2 {
			err := <-errs
			var pgErr *pgconn.PgError
			if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "40001") {
				t.Fatal(err)
			}
		}
		if _, err := r.ApplyRefundExceptionRepair(ctx, input); err != nil {
			t.Fatal("serialization retry must read original receipt", err)
		}
		var once bool
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM refund_exception_repair_cases WHERE refund_id=$1)=1 AND
 (SELECT count(*) FROM journal_entries WHERE business_reference='refund-repair:'||$1)=1 AND
 (SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.refund.succeeded.v1')=1`, plan.RefundID).Scan(&once); err != nil || !once {
			t.Fatal("duplicate repair effect", once, err)
		}
	})
}

func TestRefundExceptionHistoryWithoutChargebackRepairFacilityPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if raw == "" {
		t.Skip("exclusive PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The existing F14 absent-facility fixture pins this published membership;
	// taking only numeric <=68 would omit required activation capabilities.
	r, _ := entitlementRepositoryWithStage(t, ctx, raw, func(loaded []migrations.Migration) ([]migrations.Migration, error) {
		absent := map[int64]string{70: "chargeback_financial_repair_cases", 71: "chargeback_financial_repair_proof_claim_allowlist",
			72: "reconciliation_repair_cases", 73: "reconciliation_financial_repair_proof_claim_allowlist",
			74: "usage_settlement_repair_cases", 75: "usage_loss_review_settled_state", 76: "usage_settlement_repair_proof_claim_allowlist",
			900019: "chargeback_activation_execution_lock", 900020: "reconciliation_activation_execution_lock", 900021: "usage_activation_execution_lock"}
		selected := make([]migrations.Migration, 0, len(loaded))
		removed := 0
		for _, migration := range loaded {
			if name, exclude := absent[migration.Version]; exclude {
				if migration.Name != name {
					return nil, errors.New("absent-facility fixture migration identity changed")
				}
				removed++
				continue
			}
			selected = append(selected, migration)
		}
		if removed != len(absent) {
			return nil, errors.New("absent-facility fixture requires complete dependent phases")
		}
		return selected, nil
	})
	f := seedRefundException(t, ctx, r, false)
	var installed bool
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('public.chargeback_financial_repair_cases') IS NOT NULL`).Scan(&installed); err != nil || installed {
		t.Fatal("fixture must precede optional chargeback repair facility", installed, err)
	}
	if _, err := refundExceptionRepairPlanFrom(ctx, r.pool, RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: f.EvidenceID, AnomalyCaseID: f.AnomalyCaseID}, 1, 1, 1); err != nil {
		t.Fatal("history-free old stage acquired a new mandatory facility", err)
	}
}
