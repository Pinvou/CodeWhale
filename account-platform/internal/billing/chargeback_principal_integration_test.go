package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type principalReconciliationProvider struct{ payment.Provider }

func (p principalReconciliationProvider) Capabilities() PaymentCapabilities {
	caps := p.Provider.Capabilities()
	caps.BillFileImportSupported = true
	return caps
}

func principalWireValid(t *testing.T, ctx context.Context, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "node", filepath.Join("..", "..", "test", "fixtures", "chargeback-principal-validator.mjs"), name)
	command.Stdin = bytes.NewReader(raw)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "VALID\n" {
		t.Fatal("actual principal DTO violates published wire contract; payload suppressed")
	}
}

func principalMoneySnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var snapshot string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),
 'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),
 'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p),
 'receivables',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM receivables v),
 'lots',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM credit_lots l))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestChargebackPrincipalIndependentCashPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	signed := tokenWalletWorkload(t)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"supplier","service":"controlled-supplier-bridge","permissions":["internal.billing.chargeback.record"]},{"subject":"wrong","service":"other-service","permissions":["internal.billing.wallet.read"]}]`)
	h := NewService(r).Handler("")
	call := func(method, path, subject string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request = request.WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		if subject != "" {
			request.Header.Set("Authorization", "Bearer "+signed(subject))
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		return response
	}
	record := func(transaction uuid.UUID, id, amount, ref, currency string, want int) ChargebackCase {
		t.Helper()
		response := call("POST", "/internal/v1/chargebacks/confirmed", "supplier", map[string]any{
			"payment_transaction_id": transaction, "provider_chargeback_id": id, "payload_snapshot": map[string]any{"supplier_receipt": "controlled-independent-cash"},
			"principal_evidence": ChargebackPrincipalInput{AmountFen: amount, Currency: currency, ProviderTransactionID: ref, SupplierEvidenceReference: "controlled-receipt-" + amount},
		})
		if response.Code != want {
			t.Fatalf("principal record HTTP %d want %d", response.Code, want)
		}
		var item ChargebackCase
		if want == 200 && json.Unmarshal(response.Body.Bytes(), &item) != nil {
			t.Fatal("invalid chargeback DTO")
		}
		if want == 200 {
			principalWireValid(t, ctx, "ChargebackCase", item)
		}
		return item
	}
	newPaid := func() (*PaymentCoordinator, PaymentOrder, uuid.UUID, string) {
		t.Helper()
		c, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
		c, err := NewPaymentCoordinatorWithProvider(r, principalReconciliationProvider{c.provider}, "test")
		if err != nil {
			t.Fatal(err)
		}
		var reference string
		if err := r.pool.QueryRow(ctx, `SELECT provider_transaction_id FROM payment_transactions WHERE id=$1`, transaction).Scan(&reference); err != nil {
			t.Fatal(err)
		}
		return c, order, transaction, reference
	}
	t.Run("authorized-original-writer-partial-and-replay", func(t *testing.T) {
		c, order, transaction, ref := newPaid()
		before := principalMoneySnapshot(t, r, ctx)
		item := record(transaction, "partial-cash", "40", ref, "CNY", 200)
		if item.CashPrincipal == nil || item.CashPrincipal.Status != "CONFIRMED" || item.CashPrincipal.AmountFen == nil || *item.CashPrincipal.AmountFen != "40" || item.CashPrincipal.ObservationCount != 1 || item.Status != "REVIEW_REQUIRED" {
			t.Fatal("independent partial cash was not retained", item.CashPrincipal)
		}
		same := record(transaction, "partial-cash", "40", ref, "CNY", 200)
		if !reflect.DeepEqual(item, same) {
			t.Fatal("same receipt changed case, version or principal")
		}
		if principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("cash recording/replay moved funds")
		}
		if _, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); !errors.Is(err, ErrConflict) {
			t.Fatal("partial cash entered whole-payment executor", err)
		}
		if _, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, ChargebackFinancialRepairCommandInput{ChargebackCaseID: item.ID, PaymentEvidenceID: item.PaymentEvidenceID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("partial cash entered whole BASE repair", err)
		}
		row := ProviderBillEntry{EntryType: "CHARGEBACK", ProviderReferenceID: item.ProviderChargebackID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: 40, Currency: "CNY", Status: "CONFIRMED", PaidAt: r.now()}
		run, err := c.ImportProviderBill(ctx, uuid.New(), order.Channel, reconciliationFixtureDateForPaidAt(row.PaidAt), []ProviderBillEntry{row})
		if err != nil || run.Status != "COMPLETED" {
			t.Fatal("actual reconciliation did not complete", err)
		}
		var missing, mismatch, ledger int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE c.case_type IN ('CHARGEBACK_PRINCIPAL_MISSING','CHARGEBACK_PRINCIPAL_CONFLICT')),count(*) FILTER(WHERE c.case_type='AMOUNT_MISMATCH'),count(*) FILTER(WHERE c.case_type='CHARGEBACK_LEDGER_MISSING') FROM reconciliation_cases c JOIN reconciliation_case_runs cr ON cr.case_id=c.id WHERE cr.run_id=$1`, run.ID).Scan(&missing, &mismatch, &ledger); err != nil || missing != 0 || mismatch != 0 || ledger != 1 {
			t.Fatal("partial cash compared as original gross or hid missing points ledger", missing, mismatch, ledger, err)
		}
		var lost, subtotal, status string
		if err := r.pool.QueryRow(ctx, `SELECT chargeback_summary->>'lost_amount_fen',chargeback_summary->>'verified_lost_amount_fen',chargeback_summary->>'cash_principal_status' FROM payment_orders WHERE id=$1`, order.ID).Scan(&lost, &subtotal, &status); err != nil || lost != "40" || subtotal != "40" || status != "CONFIRMED" {
			t.Fatal("cash summary inferred from original gross", lost, subtotal, status, err)
		}
		if principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("reconciliation or refused executor moved funds")
		}
	})
	t.Run("conflicts-retained-paginated-and-fresh-whole-blocked", func(t *testing.T) {
		_, order, transaction, ref := newPaid()
		item := record(transaction, "conflicting-cash", "100", ref, "CNY", 200)
		firstDigest := item.CashPrincipal.EvidenceDigest
		item = record(transaction, "conflicting-cash", "60", ref, "CNY", 200)
		if item.CashPrincipal.Status != "CONFLICT" || item.CashPrincipal.AmountFen != nil || item.CashPrincipal.ObservationCount != 2 || firstDigest == item.CashPrincipal.EvidenceDigest {
			t.Fatal("conflict overwritten, exposed as cash, or lost digest")
		}
		before := principalMoneySnapshot(t, r, ctx)
		if _, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); !errors.Is(err, ErrConflict) {
			t.Fatal("conflicting cash applied whole payment", err)
		}
		response := call("GET", "/internal/v1/chargebacks/"+item.ID.String()+"/principal-evidence?limit=1", "supplier", nil)
		var page ChargebackPrincipalEvidencePage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.NextAfter == nil || len(page.Observations) != 1 || page.Principal.Status != "CONFLICT" {
			t.Fatal("first evidence page unavailable")
		}
		response = call("GET", "/internal/v1/chargebacks/"+item.ID.String()+"/principal-evidence?limit=1&after="+page.NextAfter.String(), "supplier", nil)
		var next ChargebackPrincipalEvidencePage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &next) != nil || next.NextAfter != nil || len(next.Observations) != 1 || bytes.Equal(page.Observations[0], next.Observations[0]) {
			t.Fatal("second evidence page repeated or lost")
		}
		principalWireValid(t, ctx, "EvidencePage", page)
		principalWireValid(t, ctx, "EvidencePage", next)
		for _, raw := range append(page.Observations, next.Observations...) {
			var receipt map[string]json.RawMessage
			json.Unmarshal(raw, &receipt)
			if receipt["payload_snapshot"] != nil || string(receipt["recorded_by_service"]) != `"controlled-supplier-bridge"` || string(receipt["recorded_by_subject"]) != `"supplier"` {
				t.Fatal("receipt provenance or bounded metadata projection differs")
			}
		}
		var lost *string
		if err := r.pool.QueryRow(ctx, `SELECT chargeback_summary->>'lost_amount_fen' FROM payment_orders WHERE id=$1`, order.ID).Scan(&lost); err != nil || lost != nil || principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("conflict treated as known cash or moved funds", err)
		}
	})
	t.Run("legacy-missing-cash-and-completed-receipt-stable", func(t *testing.T) {
		c, order, transaction, ref := newPaid()
		item, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "historical-whole", json.RawMessage(`{"legacy":true}`))
		if err != nil || item.CashPrincipal.Status != "MISSING" || item.CashPrincipal.AmountFen != nil {
			t.Fatal("legacy gross certified as independent cash", err)
		}
		done, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID)
		if err != nil || done.RevokedBaseMicrocredits != 1000000 {
			t.Fatal("existing manual whole-payment path regressed", err)
		}
		row := ProviderBillEntry{EntryType: "CHARGEBACK", ProviderReferenceID: item.ProviderChargebackID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: 100, Currency: "CNY", Status: "CONFIRMED", PaidAt: r.now()}
		run, err := c.ImportProviderBill(ctx, uuid.New(), order.Channel, reconciliationFixtureDateForPaidAt(row.PaidAt), []ProviderBillEntry{row})
		if err != nil {
			t.Fatal(err)
		}
		var missing int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases c JOIN reconciliation_case_runs cr ON cr.case_id=c.id WHERE cr.run_id=$1 AND c.case_type='CHARGEBACK_PRINCIPAL_MISSING'`, run.ID).Scan(&missing); err != nil || missing != 1 {
			t.Fatal("legacy cash absence hidden by matching gross", missing, err)
		}
		record(transaction, item.ProviderChargebackID, "40", ref, "CNY", 200)
		var lateReviews int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_anomaly_cases WHERE resource_snapshot->>'chargeback_case_id'=$1::text AND anomaly_type='PAYMENT_FACT_CONFLICT' AND dedupe_key LIKE 'chargeback-principal:%' AND status='OPEN'`, item.ID).Scan(&lateReviews); err != nil || lateReviews != 1 {
			t.Fatal("late cash did not reach active human review", err)
		}
		before := principalMoneySnapshot(t, r, ctx)
		replay, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID)
		if err != nil || replay.ID != done.ID || replay.RevokedBaseMicrocredits != done.RevokedBaseMicrocredits || principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("late cash facts repeated or erased completed money", err)
		}
	})
	t.Run("F14-partial-cash-fences-fresh-plan-and-retains-old-receipt", func(t *testing.T) {
		activation := chargebackFixtureActivation(t, ctx, r.pool)("ACTIVE")
		partial := seedChargebackShortfall(t, ctx, r, true)
		partialCommand := ChargebackFinancialRepairCommandInput{ChargebackCaseID: partial.ChargebackCaseID, PaymentEvidenceID: partial.EvidenceID}
		if _, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, partialCommand, 1, 1, activation); err != nil {
			t.Fatal("consumed BASE fixture was not otherwise eligible", err)
		}
		record(partial.TransactionID, partial.ProviderChargebackID, "40", "trade-"+partial.TransactionID.String(), "CNY", 200)
		before := principalMoneySnapshot(t, r, ctx)
		if _, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, partialCommand, 1, 1, activation); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("partial cash entered otherwise eligible whole BASE plan", err)
		}
		if before != principalMoneySnapshot(t, r, ctx) {
			t.Fatal("refused partial plan moved money")
		}
		completed := seedChargebackShortfall(t, ctx, r, true)
		command := ChargebackFinancialRepairCommandInput{ChargebackCaseID: completed.ChargebackCaseID, PaymentEvidenceID: completed.EvidenceID}
		plan, err := chargebackFinancialRepairPlanFrom(ctx, r.pool, command, 1, 1, activation)
		if err != nil {
			t.Fatal(err)
		}
		input := chargebackFrozenInput(plan, activation)
		done, err := r.ApplyChargebackFinancialRepair(ctx, input)
		if err != nil {
			t.Fatal("legacy owner repair fixture", err)
		}
		record(completed.TransactionID, completed.ProviderChargebackID, "40", "trade-"+completed.TransactionID.String(), "CNY", 200)
		before = principalMoneySnapshot(t, r, ctx)
		replay, err := r.ApplyChargebackFinancialRepair(ctx, input)
		if err != nil || replay != done || before != principalMoneySnapshot(t, r, ctx) {
			t.Fatal("late cash changed completed F14 receipt or money", err)
		}
	})
	t.Run("binding-auth-input-rejections-zero-write-and-recovery", func(t *testing.T) {
		_, _, transaction, ref := newPaid()
		before := principalMoneySnapshot(t, r, ctx)
		for _, test := range []struct{ amount, reference, currency string }{{"0", ref, "CNY"}, {"-1", ref, "CNY"}, {"01", ref, "CNY"}, {"9223372036854775808", ref, "CNY"}, {"20", "foreign-transaction", "CNY"}, {"20", ref, "USD"}} {
			record(transaction, "invalid-cash", test.amount, test.reference, test.currency, 409)
		}
		for _, subject := range []string{"", "wrong"} {
			for _, method := range []string{"POST", "GET"} {
				path := "/internal/v1/chargebacks/confirmed"
				var body any = map[string]any{}
				if method == "GET" {
					path = "/internal/v1/chargebacks/" + uuid.NewString() + "/principal-evidence"
					body = nil
				}
				response := call(method, path, subject, body)
				want := 401
				if subject != "" {
					want = 403
				}
				if response.Code != want {
					t.Fatal("unauthorized principal entry accepted", method, response.Code)
				}
			}
		}
		var cases, observations int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM chargeback_cases WHERE payment_transaction_id=$1),(SELECT count(*) FROM chargeback_principal_evidence WHERE payment_transaction_id=$1)`, transaction).Scan(&cases, &observations); err != nil || cases != 0 || observations != 0 || principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("rejected evidence left partial case/freeze/money", err)
		}
		item := record(transaction, "recovered-cash", "100", ref, "CNY", 200)
		if _, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); err != nil {
			t.Fatal("valid full cash failed after refusals", err)
		}
	})
	t.Run("combined-and-over-gross-cash-requires-review", func(t *testing.T) {
		_, _, transaction, ref := newPaid()
		a := record(transaction, "cash-a", "60", ref, "CNY", 200)
		b := record(transaction, "cash-b", "50", ref, "CNY", 200)
		aFact, err := readChargebackCashPrincipal(ctx, r.pool, a.ID)
		if err != nil || b.CashPrincipal.Status != "CONFLICT" || aFact.Status != "CONFLICT" || aFact.AmountFen != nil {
			t.Fatal("combined cash exceeded original without manual conflict", err)
		}
		if _, err := r.ApplyConfirmedChargeback(ctx, a.PaymentEvidenceID, a.ProviderChargebackID); !errors.Is(err, ErrConflict) {
			t.Fatal("peer cash conflict bypassed whole gate", err)
		}
		_, _, overTransaction, overRef := newPaid()
		before := principalMoneySnapshot(t, r, ctx)
		over := record(overTransaction, "over-gross", "101", overRef, "CNY", 200)
		if over.CashPrincipal.Status != "CONFLICT" || over.CashPrincipal.AmountFen != nil || principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("over-gross claim lost or accepted as funds")
		}

	})
	t.Run("immutable-receipts-runtime-and-migration-owner", func(t *testing.T) {
		for _, sql := range []string{`UPDATE chargeback_principal_evidence SET amount_fen=1`, `DELETE FROM chargeback_principal_evidence`, `TRUNCATE chargeback_principal_evidence`} {
			if _, err := r.pool.Exec(ctx, sql); err == nil {
				t.Fatal("runtime could mutate cash evidence")
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, sql); err == nil {
				t.Fatal("owner could mutate cash evidence")
			}
		}
	})
	t.Run("shared-supplier-evidence-cannot-bind-another-payment", func(t *testing.T) {
		_, _, firstTransaction, firstRef := newPaid()
		_, _, otherTransaction, otherRef := newPaid()
		record(firstTransaction, "shared-cash-receipt", "40", firstRef, "CNY", 200)
		before := principalMoneySnapshot(t, r, ctx)
		record(otherTransaction, "shared-cash-receipt", "40", otherRef, "CNY", 409)
		var cases, claims int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM chargeback_cases WHERE payment_transaction_id=$1),(SELECT count(*) FROM chargeback_principal_evidence WHERE payment_transaction_id=$1)`, otherTransaction).Scan(&cases, &claims); err != nil || cases != 0 || claims != 0 || before != principalMoneySnapshot(t, r, ctx) {
			t.Fatal("shared supplier evidence rebound across original payments", err)
		}
	})
	t.Run("storage-failure-rolls-back-and-next-attempt-recovers", func(t *testing.T) {
		_, _, transaction, ref := newPaid()
		item := record(transaction, "cash-storage", "100", ref, "CNY", 200)
		before := principalMoneySnapshot(t, r, ctx)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `REVOKE SELECT ON chargeback_cash_principals FROM billing_service`); err != nil {
			t.Fatal(err)
		}
		record(transaction, "cash-storage", "50", ref, "CNY", 500)
		if _, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); err == nil || errors.Is(err, ErrConflict) {
			t.Fatal("storage failure was accepted or mislabeled a business conflict", err)
		}
		if principalMoneySnapshot(t, r, ctx) != before {
			t.Fatal("failed principal read moved money")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `GRANT SELECT ON chargeback_cash_principals TO billing_service`); err != nil {
			t.Fatal(err)
		}
		current := record(transaction, "cash-storage", "100", ref, "CNY", 200)
		if current.Version != item.Version || current.CashPrincipal.ObservationCount != 1 {
			t.Fatal("failed request committed partial evidence or version")
		}
		if _, err := r.ApplyConfirmedChargeback(ctx, item.PaymentEvidenceID, item.ProviderChargebackID); err != nil {
			t.Fatal("next valid whole-payment attempt did not recover", err)
		}
	})
	t.Run("concurrent-peer-cash-serializes-and-retries-without-money", func(t *testing.T) {
		_, order, transaction, ref := newPaid()
		a, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "parallel-a", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "parallel-b", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		before := principalMoneySnapshot(t, r, ctx)
		barrier, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer barrier.Rollback(context.Background())
		if _, err := barrier.Exec(ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, order.WalletID); err != nil {
			t.Fatal(err)
		}
		requests := make([][]byte, 2)
		token := signed("supplier")
		for index, claim := range []struct{ id, amount string }{{"parallel-a", "60"}, {"parallel-b", "50"}} {
			requests[index], _ = json.Marshal(map[string]any{"payment_transaction_id": transaction, "provider_chargeback_id": claim.id, "payload_snapshot": map[string]any{"receipt": "controlled-parallel"}, "principal_evidence": ChargebackPrincipalInput{AmountFen: claim.amount, Currency: "CNY", ProviderTransactionID: ref, SupplierEvidenceReference: "parallel-" + claim.id}})
		}
		send := func(raw []byte) int {
			request := httptest.NewRequest("POST", "/internal/v1/chargebacks/confirmed", bytes.NewReader(raw)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, request)
			return out.Code
		}
		type outcome struct{ index, status int }
		done := make(chan outcome, 2)
		for index, raw := range requests {
			go func(index int, raw []byte) { done <- outcome{index, send(raw)} }(index, raw)
		}
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var waiting int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM wallets%'`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting == 2 {
				break
			}
			select {
			case <-deadline.C:
				t.Fatal("both actual writers never reached held wallet barrier")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-tick.C:
			}
		}
		if err := barrier.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		for range requests {
			result := <-done
			if result.status == 500 {
				result.status = send(requests[result.index])
			}
			if result.status != 200 {
				t.Fatal("same evidence did not recover after concurrent wait", result.status)
			}
		}
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			fact, err := readChargebackCashPrincipal(ctx, r.pool, id)
			if err != nil || fact.Status != "CONFLICT" || fact.AmountFen != nil || fact.ObservationCount != 1 {
				t.Fatal("peer cash was lost, duplicated or accepted above original", err)
			}
		}
		if before != principalMoneySnapshot(t, r, ctx) {
			t.Fatal("concurrent evidence moved money")
		}
	})
	t.Run("retained-038-forward-upgrade-and-down-refusal", chargebackPrincipalUpgrade)
}

func chargebackPrincipalUpgrade(t *testing.T) {
	r, ctx := retainedFinanceRepository(t, 900038)
	// This retained fixture represents historical owner facts, not a new supplier
	// assertion. No original cash proof can be recovered from its points.
	fixture := seedChargebackShortfall(t, ctx, r, false)
	if _, err := r.pool.Exec(ctx, `UPDATE chargeback_cases SET status='CONFIRMED' WHERE id=$1;`, fixture.ChargebackCaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET chargeback_summary='{"lost_amount_fen":"100","open_count":1}' WHERE id=$1`, fixture.OrderID); err != nil {
		t.Fatal(err)
	}
	before := principalMoneySnapshot(t, r, ctx)
	config := r.pool.Config().ConnConfig.Copy()
	config.User = "billing_migrator"
	config.Password = ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	fact, err := readChargebackCashPrincipal(ctx, r.pool, fixture.ChargebackCaseID)
	if err != nil || fact.Status != "MISSING" || fact.AmountFen != nil || fact.ObservationCount != 0 {
		t.Fatal("migration invented historical cash", err)
	}
	var loss *string
	if err := r.pool.QueryRow(ctx, `SELECT chargeback_summary->>'lost_amount_fen' FROM payment_orders WHERE id=$1`, fixture.OrderID).Scan(&loss); err != nil || loss != nil {
		t.Fatal("retained gross loss still certified", err)
	}
	if before != principalMoneySnapshot(t, r, ctx) {
		t.Fatal("migration changed financial facts")
	}
	if err := billingschema.DownOne(ctx, connection, loaded); err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatal("cash evidence down was not refused", err)
	}
	if before != principalMoneySnapshot(t, r, ctx) {
		t.Fatal("refused down changed financial facts")
	}
	if err := billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal("up replay after rejected down", err)
	}
	var receipts int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=900039`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("migration ledger changed after down refusal", strconv.Itoa(receipts), err)
	}
}
