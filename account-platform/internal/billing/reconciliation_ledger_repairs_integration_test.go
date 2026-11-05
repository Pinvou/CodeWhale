package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Uses a fresh owned UUID database, real public Billing migrations and the
// restricted runtime identity. Provider facts are controlled test fixtures.
func TestReconciliationLedgerRepairPublicPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	originalCoordinator, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	providerFact := originalCoordinator.provider.(*retainedFinancialProvider).result
	if providerFact.ChannelTransactionID == nil || providerFact.PaidAt == nil {
		t.Fatal("fixture lacks original provider facts")
	}
	var journalID, postingID uuid.UUID
	var amount int64
	if err := r.pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, "payment-transaction:"+transaction.String(), paymentClearingAccount).Scan(&journalID, &postingID, &amount); err != nil {
		t.Fatal("original clearing posting", err)
	}
	// Controlled historic wrong attribution in the test's own UUID database.
	// The immutable trigger is disabled and restored atomically in one owned
	// migration-actor transaction; normal runtime actions never bypass it.
	statement := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE public.ledger_postings SET ledger_account_id='%s' WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT`, adjustmentAccount.String(), postingID.String())
	if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal("owned historical fixture", err)
	}
	snapshot := ledgerRepairZeroChangeSnapshot(t, ctx, r.pool)
	financialFacts := func() string {
		t.Helper()
		var result string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		  'wallets',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),'[]'),
		  'lots',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),'[]'),
		  'packages',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),'[]'),
		  'components',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),'[]'),
		  'original_journal',(SELECT to_jsonb(x) FROM journal_entries x WHERE id=$1),
		  'original_postings',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x WHERE journal_entry_id=$1),'[]')
		)::text`, journalID).Scan(&result); err != nil {
			t.Fatal("read exact original financial facts", err)
		}
		return result
	}
	originalFinancialFacts := financialFacts()
	operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
	activation := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	input := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, AmountMicrocredits: absInt64(amount)}
	plan, err := r.LedgerRepairPlanForCommand(ctx, input, 1, 1, activation)
	if err != nil {
		t.Fatal("actual repair plan", err)
	}
	row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: *providerFact.ChannelTransactionID, ChannelTransactionID: *providerFact.ChannelTransactionID, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: *providerFact.PaidAt}
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: retainedFinancialProvider{result: providerFact}, rows: []ProviderBillEntry{row}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	date := reconciliationFixtureDateForPaidAt(*providerFact.PaidAt)
	actor := uuid.New()
	service := NewService(r).WithPayments(c)
	call := func(handler http.HandlerFunc, path, body, caseID, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.SetPathValue("case_id", caseID)
		request.Header.Set("Authorization", "Bearer controlled-human")
		request.Header.Set("Idempotency-Key", key)
		out := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
		}, handler).ServeHTTP(out, request)
		return out
	}
	requestBody, err := json.Marshal(map[string]string{"channel": order.Channel, "business_date": date.Format("2006-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	runResponse := call(service.adminRunReconciliation, "/admin/v1/reconciliation-runs", string(requestBody), "", "")
	var before ReconciliationRun
	if runResponse.Code != http.StatusOK || json.Unmarshal(runResponse.Body.Bytes(), &before) != nil {
		t.Fatal("actual HTTP audit failed", runResponse.Code)
	}
	cases, err := r.AdminReconciliationCases(ctx, before.ID, 10)
	if err != nil || len(cases) != 1 {
		t.Fatal("expected original invalid case", len(cases), err)
	}
	if cases[0].CaseType != "PAYMENT_LEDGER_INVALID" || before.SourceMode != "MANUAL_API" {
		t.Fatal("unexpected original case or source mode", cases[0].CaseType, before.SourceMode)
	}
	var storedRows int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1 AND entry_type='PAYMENT' AND provider_reference_id=$2`, before.ID, row.ProviderReferenceID).Scan(&storedRows); err != nil || storedRows != 1 {
		t.Fatal("actual API bill row was not retained", storedRows, err)
	}
	start, end := reconciliationBusinessDateRange(date)
	facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
	if err != nil || len(facts) != 1 {
		t.Fatal("original date/list", len(facts), err)
	}
	if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil || facts[0].LedgerValid || facts[0].IntegrityCaseType != "PAYMENT_LEDGER_INVALID" {
		t.Fatal("original audit did not expose wrong attribution", err)
	}
	originalDigest := facts[0].IntegrityDigest
	closureBefore, err := r.AccountClosureAssessment(ctx, order.RequesterID)
	if err != nil || !f13HasClosureBlocker(closureBefore, "FINANCIAL_RECONCILIATION_OPEN") || !f13HasClosureBlocker(closureBefore, "PERSONAL_BALANCE_NONZERO") {
		t.Fatal("original closure blockers missing", err)
	}
	caseID := cases[0].ID
	path := "/admin/v1/reconciliation-cases/" + caseID.String() + "/commands"
	claim := call(service.adminUpdateReconciliationCase, path, `{"action":"CLAIM","reason":"controlled financial reconciliation claim"}`, caseID.String(), "claim-"+uuid.NewString())
	if claim.Code != http.StatusOK {
		t.Fatal("HTTP claim failed", claim.Code)
	}
	resolveKey := "resolve-" + uuid.NewString()
	resolveBody := `{"action":"RESOLVE","reason":"controlled resolution after exact ledger correction","review_result":"controlled evidence"}`
	beforeResolve := call(service.adminUpdateReconciliationCase, path, resolveBody, caseID.String(), resolveKey)
	if beforeResolve.Code != http.StatusConflict {
		t.Fatal("original invalid case resolved through HTTP", beforeResolve.Code)
	}
	execution := LedgerRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "ledger-repair-test-" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: input, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
	result, err := r.ApplyLedgerRepair(ctx, execution)
	if err != nil {
		t.Fatal("actual immutable correction", err)
	}
	ledgerRepairAssertZeroChange(t, ctx, r.pool, snapshot)
	if financialFacts() != originalFinancialFacts {
		t.Fatal("immutable original ledger or financial facts changed")
	}
	setLedgerActivationFixtureState(t, ctx, operator, "INACTIVE")
	facts, err = c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
	if err != nil || len(facts) != 1 {
		t.Fatal("actual date/list", len(facts), err)
	}
	if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil || !facts[0].LedgerValid || !strings.Contains(string(facts[0].LedgerSnapshot), result.CaseID.String()) {
		t.Fatal("corrected audit invalid or evidence missing", err)
	}
	if facts[0].IntegrityDigest == originalDigest {
		t.Fatal("correction did not enter stable audit identity")
	}
	correctedDigest := facts[0].IntegrityDigest
	if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil || !facts[0].LedgerValid || facts[0].IntegrityDigest != correctedDigest {
		t.Fatal("unchanged correction audit was unstable", err)
	}
	// This fixture deliberately invokes the low-level repository with random
	// authorization IDs and no Account claim. The correction is structurally
	// sound but is not authoritative proof and cannot close the original case.
	afterResolve := call(service.adminUpdateReconciliationCase, path, resolveBody, caseID.String(), resolveKey)
	if afterResolve.Code != http.StatusConflict {
		t.Fatal("unproven original case resolved through HTTP", afterResolve.Code)
	}
	blocked := []ReconciliationInternalFact{facts[0]}
	applyLedgerRepairAuthority(blocked, nil)
	if blocked[0].LedgerValid || blocked[0].IntegrityCaseType != ledgerRepairProofUnavailableCase {
		t.Fatal("unproven correction was not classified as unavailable")
	}
	closureAfter, err := r.AccountClosureAssessment(ctx, order.RequesterID)
	if err != nil || !f13HasClosureBlocker(closureAfter, "FINANCIAL_RECONCILIATION_OPEN") || !f13HasClosureBlocker(closureAfter, "PERSONAL_BALANCE_NONZERO") {
		t.Fatal("unproven correction removed closure blockers", err)
	}
	if replay, err := r.ApplyLedgerRepair(ctx, execution); err != nil || replay != result {
		t.Fatal("correction replay changed", err)
	}
	ledgerRepairAssertZeroChange(t, ctx, r.pool, snapshot)
	if financialFacts() != originalFinancialFacts {
		t.Fatal("immutable original ledger or financial facts changed")
	}
}

// A corrected peer must be included in each cumulative forced-refund audit.
func TestReconciliationLedgerRepairForcedPeerPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	ids := make([]uuid.UUID, 0, 2)
	for i := 0; i < 2; i++ {
		item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, fmt.Sprintf("repair-peer-%d-%s", i, uuid.NewString()), 50, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ApplyReviewedForcedRefund(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	var journalID, postingID uuid.UUID
	var amount int64
	if err := r.pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, "forced-refund:"+ids[1].String(), refundClearingAccount).Scan(&journalID, &postingID, &amount); err != nil {
		t.Fatal(err)
	}
	statement := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE public.ledger_postings SET ledger_account_id='%s' WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT`, adjustmentAccount.String(), postingID.String())
	if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
	before := auditCumulativeForcedCase(t, r, ctx, order, ids[0])
	if before.LedgerValid || before.IntegrityCaseType != "REFUND_LEDGER_INVALID" {
		t.Fatal("invalid peer was accepted", before.IntegrityCaseType)
	}
	snapshot := ledgerRepairZeroChangeSnapshot(t, ctx, r.pool)
	operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
	activation := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	input := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: refundClearingAccount, AmountMicrocredits: absInt64(amount)}
	plan, err := r.LedgerRepairPlanForCommand(ctx, input, 1, 1, activation)
	if err != nil {
		t.Fatal("peer repair plan", err)
	}
	execution := LedgerRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "ledger-repair-peer-" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: input, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: refundClearingAccount, EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
	result, err := r.ApplyLedgerRepair(ctx, execution)
	if err != nil {
		t.Fatal("peer correction", err)
	}
	ledgerRepairAssertZeroChange(t, ctx, r.pool, snapshot)
	for _, id := range ids {
		after := auditCumulativeForcedCase(t, r, ctx, order, id)
		if !after.LedgerValid || !strings.Contains(string(after.LedgerSnapshot), result.CaseID.String()) {
			t.Fatal("corrected cumulative peer rejected or missing evidence", id, after.IntegrityCaseType)
		}
		if id == ids[0] && after.IntegrityDigest == before.IntegrityDigest {
			t.Fatal("peer correction omitted from digest")
		}
		again := auditCumulativeForcedCase(t, r, ctx, order, id)
		if !again.LedgerValid || again.IntegrityDigest != after.IntegrityDigest {
			t.Fatal("peer correction audit unstable", id)
		}
		blocked := []ReconciliationInternalFact{after}
		applyLedgerRepairAuthority(blocked, nil)
		if blocked[0].LedgerValid || blocked[0].IntegrityCaseType != ledgerRepairProofUnavailableCase {
			t.Fatal("unproven peer correction was accepted", id)
		}
	}
}

// TestRealRepairOnRefundAndChargeback uses actual payment/refund/chargeback
// producers and the immutable correction on each kind in an owned UUID DB.
func TestRealRepairOnRefundAndChargeback(t *testing.T) {
	for _, kind := range []string{"REFUND", "CHARGEBACK"} {
		t.Run(kind, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			var reference string
			var expected uuid.UUID
			if kind == "REFUND" {
				c.AllowNormalRefundCreation(true)
				refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled ledger repair refund")
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen, Status: "SUCCESS", ChannelRefundID: "controlled-repair-refund"}, false)
				if err != nil {
					t.Fatal(err)
				}
				reference = "refund:" + refund.ID.String()
				expected = refundClearingAccount
			} else {
				providerID := "controlled-repair-chargeback"
				chargeback, err := r.RecordConfirmedChargebackForReview(ctx, transaction, providerID, json.RawMessage(`{"controlled":true}`))
				if err != nil {
					t.Fatal(err)
				}
				actor, key := uuid.New(), uuid.NewString()
				retainedClaimAnomaly(t, r, ctx, *chargeback.ManualReviewCaseID, actor)
				if _, _, err = r.PreparePaymentAnomalyResolution(ctx, *chargeback.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", "controlled audit"); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyConfirmedChargeback(ctx, chargeback.PaymentEvidenceID, providerID); err != nil {
					t.Fatal(err)
				}
				if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *chargeback.ManualReviewCaseID, actor, key, "CONFIRM_CHARGEBACK", "controlled audit", "APPLIED"); err != nil {
					t.Fatal(err)
				}
				reference = "payment-transaction:" + transaction.String() + ":chargeback:" + providerID
				expected = chargebackClearingAccount
			}
			var journalID, postingID uuid.UUID
			var amount int64
			if err := r.pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, reference, expected).Scan(&journalID, &postingID, &amount); err != nil {
				t.Fatal("original clearing", err)
			}
			statement := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE public.ledger_postings SET ledger_account_id='%s' WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT`, adjustmentAccount.String(), postingID.String())
			if _, err := retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
				t.Fatal("owned historic fixture", err)
			}
			audit := func() ReconciliationInternalFact {
				t.Helper()
				start, end := reconciliationBusinessDateRange(normalizedReconciliationBusinessDate(r.now().In(time.FixedZone("Asia/Shanghai", 8*60*60))))
				facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
				if err != nil {
					t.Fatal(err)
				}
				if err = auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
					t.Fatal(err)
				}
				var selected *ReconciliationInternalFact
				for i := range facts {
					if facts[i].EntryType == kind {
						if selected != nil {
							t.Fatal("ambiguous kind")
						}
						selected = &facts[i]
					}
				}
				if selected == nil {
					t.Fatal("missing actual source fact", kind)
				}
				return *selected
			}
			before := audit()
			if before.LedgerValid || before.IntegrityCaseType != kind+"_LEDGER_INVALID" {
				t.Fatal("invalid original accepted", kind, before.IntegrityCaseType)
			}
			snap := ledgerRepairZeroChangeSnapshot(t, ctx, r.pool)
			operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
			activation := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
			input := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: expected, AmountMicrocredits: absInt64(amount)}
			plan, err := r.LedgerRepairPlanForCommand(ctx, input, 1, 1, activation)
			if err != nil {
				t.Fatal("real repair plan", err)
			}
			execution := LedgerRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ExecutionKey: "repair-kind-" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: input, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: expected, EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
			result, err := r.ApplyLedgerRepair(ctx, execution)
			if err != nil {
				t.Fatal("real repair", err)
			}
			ledgerRepairAssertZeroChange(t, ctx, r.pool, snap)
			after := audit()
			if !after.LedgerValid || !strings.Contains(string(after.LedgerSnapshot), result.CaseID.String()) {
				t.Fatal("corrected source rejected", kind, after.IntegrityCaseType)
			}
			blocked := []ReconciliationInternalFact{after}
			applyLedgerRepairAuthority(blocked, nil)
			if blocked[0].LedgerValid || blocked[0].IntegrityCaseType != ledgerRepairProofUnavailableCase {
				t.Fatal("unproven correction accepted", kind)
			}
			if before.IntegrityDigest == after.IntegrityDigest || audit().IntegrityDigest != after.IntegrityDigest {
				t.Fatal("digest change/stability", kind)
			}
			if replay, err := r.ApplyLedgerRepair(ctx, execution); err != nil || replay != result {
				t.Fatal("repair replay", err)
			}
			ledgerRepairAssertZeroChange(t, ctx, r.pool, snap)
			// Controlled corruption of immutable receipt in owned test database.
			corrupt := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_repair_cases DISABLE TRIGGER ledger_repair_cases_immutable; UPDATE public.ledger_repair_cases SET amount_microcredits=amount_microcredits+1 WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_repair_cases ENABLE TRIGGER ledger_repair_cases_immutable; COMMIT`, result.CaseID.String())
			if _, err = retainedFinanceOwnerExec(t, r, ctx, corrupt); err != nil {
				t.Fatal("owned corruption fixture", err)
			}
			bad := audit()
			if bad.LedgerValid || bad.IntegrityCaseType != kind+"_LEDGER_INVALID" {
				t.Fatal("corrupt receipt accepted", kind, bad.IntegrityCaseType)
			}
		})
	}
}
