package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

// TestUsageSettlementRepairCrossDomainActivationPostgresIntegration runs the real
// Account and Billing databases with the real Billing HTTP activation and proof
// authority for the GSA-36 usage settlement repair. While the deployed v1/s1
// activation row is INACTIVE the Account entry points refuse, the proof authority
// refuses and neither side records an effect; once a schema-owner activation
// flips the row, the reader and proof authority honor it, the dedicated durable
// consumer补结算 the loss review with real entitlement capacity, the Account
// request reaches EXECUTED/TERMINAL_CONFIRMED, an idempotent replay adds nothing
// and flipping the row back closes the command again.
func TestUsageSettlementRepairCrossDomainActivationPostgresIntegration(t *testing.T) {
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if accountURL == "" || billingURL == "" {
		t.Skip("real Account and Billing PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	repository, accountConnection, relay, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, accountURL, 0)
	billingRepository, billingPool, billingConnection, loss := newUsageRepairCrossDomainBillingFixture(t, ctx, billingURL)
	proofToken, executorToken := configureEntitlementProofIdentity(t)
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepository).Handler("account-proof-test"))
	t.Cleanup(billingServer.Close)
	activationReader, err := billinggovernance.NewClient(billingServer.URL, proofToken)
	if err != nil {
		t.Fatal(err)
	}
	repository.WithBillingGovernanceActivation(activationReader)
	policy, err := approvalpolicy.Lookup("USAGE_SETTLEMENT_REPAIR")
	if err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 1 || policy.SnapshotSchemaVersion != 1 || !approvalpolicy.Executable(policy.CommandType) {
		t.Fatalf("usage settlement repair must be registered against the live Billing activation: %#v", policy)
	}
	activations, err := billingRepository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, activation := range activations.Activations {
		if activation.CommandType == policy.CommandType {
			found = true
			if activation.State != "INACTIVE" || activation.PolicyVersion != 1 || activation.SnapshotSchemaVersion != 1 {
				t.Fatalf("usage settlement repair activation is not the deployed INACTIVE v1/s1 row: %#v", activation)
			}
		}
	}
	if !found {
		t.Fatal("usage settlement repair activation row is missing")
	}

	command := billingdomain.UsageSettlementRepairCommandInput{
		UsageRequestID: loss.UsageRequestID, HoldID: loss.HoldID,
		EvidenceKind: "LOSS_REVIEW", LossReviewID: loss.LossReviewID,
	}
	frozen, err := billingRepository.UsageSettlementRepairProofInput(ctx, command)
	if err != nil {
		t.Fatalf("usage settlement repair proof input: %v", err)
	}
	if frozen.TargetResourceType != "USAGE_HOLD" || frozen.TargetResourceID != loss.HoldID.String() {
		t.Fatalf("usage settlement repair frozen target=%s/%s", frozen.TargetResourceType, frozen.TargetResourceID)
	}
	// The business page reaches owner facts only through the Account precheck
	// proxy. The closed projection it receives must equal the proof input the
	// approval later freezes, an unprovable target must be a definite refusal,
	// and the precheck itself must write no fact and issue no claim.
	precheckSource, err := NewHTTPApprovalPrecheckSource(billingServer.URL, proofToken)
	if err != nil {
		t.Fatal(err)
	}
	precheck, err := precheckSource.Precheck(ctx, ApprovalPrecheckRequest{
		CommandType: policy.CommandType, TargetResourceID: loss.HoldID.String(),
		UsageRequestID: loss.UsageRequestID.String(), LossReviewID: loss.LossReviewID.String(),
	})
	if err != nil {
		t.Fatalf("usage settlement repair precheck: %v", err)
	}
	if precheck.CommandType != frozen.CommandType || precheck.PolicyVersion != frozen.PolicyVersion ||
		precheck.SnapshotSchemaVersion != frozen.SnapshotSchemaVersion ||
		precheck.TargetResourceType != frozen.TargetResourceType || precheck.TargetResourceID != frozen.TargetResourceID ||
		string(precheck.NormalizedCommand) != string(frozen.NormalizedCommand) ||
		string(precheck.CriticalResourceVersions) != string(frozen.CriticalResourceVersions) ||
		string(precheck.FinancialSnapshot) != string(frozen.FinancialSnapshot) {
		t.Fatalf("precheck projection differs from the proof input: %#v vs %#v", precheck, frozen)
	}
	if _, err := precheckSource.Precheck(ctx, ApprovalPrecheckRequest{
		CommandType: policy.CommandType, TargetResourceID: loss.HoldID.String(),
		UsageRequestID: loss.UsageRequestID.String(), LossReviewID: uuid.NewString(),
	}); !errors.Is(err, ErrApprovalPrecheckRejected) {
		t.Fatalf("unprovable usage target must be refused, got %v", err)
	}
	var proofClaims, repairCases int64
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims`).Scan(&proofClaims); err != nil {
		t.Fatal(err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM usage_settlement_repair_cases`).Scan(&repairCases); err != nil {
		t.Fatal(err)
	}
	if proofClaims != 0 || repairCases != 0 {
		t.Fatalf("precheck wrote facts: claims=%d cases=%d", proofClaims, repairCases)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	presented := ApprovalRequest{
		CommandType: policy.CommandType, PolicyVersion: policy.PolicyVersion, SnapshotSchemaVersion: policy.SnapshotSchemaVersion,
		TargetResourceType: frozen.TargetResourceType, TargetResourceID: frozen.TargetResourceID,
		NormalizedCommand: frozen.NormalizedCommand, CriticalResourceVersions: frozen.CriticalResourceVersions,
		FinancialSnapshot: frozen.FinancialSnapshot,
	}
	if summary := approvalV2Summary(presented); summary.DetailAvailability != "AVAILABLE" {
		t.Fatalf("cross-domain presenter unavailable: %+v", summary)
	}
	detail, err := approvalV2Detail(presented, 1)
	if err != nil {
		t.Fatalf("cross-domain presenter detail: %v", err)
	}
	presentedCommand, ok := detail.Command.(approvalUsageSettlementRepairSummary)
	if !ok || presentedCommand.HoldID != loss.HoldID || presentedCommand.UsageRequestID != loss.UsageRequestID ||
		presentedCommand.WalletID != loss.WalletID || presentedCommand.EvidenceKind != "LOSS_REVIEW" ||
		presentedCommand.ExpectedMicrocredits != "100" || presentedCommand.SettledMicrocredits != "100" ||
		presentedCommand.OverageMicrocredits != "0" ||
		presentedCommand.ReasonCode != "MISSING_USAGE_SETTLEMENT" ||
		!approvalPaginationHexDigest(presentedCommand.FactsDigest) ||
		!approvalPaginationHexDigest(presentedCommand.AllocationsDigest) {
		t.Fatalf("cross-domain presenter command=%#v", detail.Command)
	}
	proofSource, err := NewHTTPApprovalProofSource("http://127.0.0.1", billingServer.URL, proofToken)
	if err != nil {
		t.Fatal(err)
	}
	repository = repository.WithApprovalProofSource(proofSource)
	if _, err := proofSource.Proof(ctx, uuid.New(), artifacts); !errors.Is(err, ErrApprovalProofUnavailable) {
		t.Fatalf("inactive cross-domain proof did not fail closed: %v", err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain advancement: %v", err)
	}
	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "usage-repair-activation-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "usage-repair-activation-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	createDraft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "usage-repair-activation-create",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "closed cross-domain activation gate",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`),
		CriticalResourceVersions: frozen.CriticalResourceVersions, FinancialSnapshot: frozen.FinancialSnapshot,
	}
	if _, err := repository.CreateApprovalDraft(ctx, requesterPrincipal, createDraft); err == nil {
		t.Fatal("inactive cross-domain create unexpectedly succeeded")
	}
	assertApprovalRequestAbsent(t, ctx, repository, requester.ID, createDraft.IdempotencyKey)
	submitID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	if err := retryWhileWriterUnavailable(t, ctx, repository, func() error {
		_, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, submitID, 1)
		return err
	}); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain submit: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, submitID, "DRAFT", 1)
	approveID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	if err := retryWhileWriterUnavailable(t, ctx, repository, func() error {
		_, err := repository.DecideApprovalRequest(ctx, approverPrincipal, approveID, 1, "APPROVED", "closed cross-domain gate", frozen.CriticalResourceVersions)
		return err
	}); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain approve: %v", err)
	}
	assertApprovalRequestState(t, ctx, repository, approveID, "SUBMITTED", 1)
	var cases, claims, repairJournals int64
	var reviewStatus string
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_settlement_repair_cases WHERE approval_request_id IS NOT NULL),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'usage-settlement-repair:%'),
		(SELECT status FROM usage_loss_reviews WHERE id=$1)`, loss.LossReviewID).
		Scan(&cases, &claims, &repairJournals, &reviewStatus); err != nil ||
		cases != 0 || claims != 0 || repairJournals != 0 || reviewStatus != "REVIEW_REQUIRED" {
		t.Fatalf("closed cross-domain gate wrote Billing effects: cases=%d claims=%d journals=%d review=%s err=%v",
			cases, claims, repairJournals, reviewStatus, err)
	}

	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1
WHERE command_type='USAGE_SETTLEMENT_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`); err != nil {
		t.Fatal(err)
	}
	// The frozen revision binds the activation version, so the request must be
	// re-assembled after the governed flip.
	frozen, err = billingRepository.UsageSettlementRepairProofInput(ctx, command)
	if err != nil {
		t.Fatalf("activated usage repair proof input: %v", err)
	}
	artifacts, err = approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); err != nil {
		t.Fatalf("activated cross-domain advancement: %v", err)
	}
	snapshot, err := activationReader.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if projected, ok := snapshot.Find(policy.CommandType, 1, 1); !ok || projected.State != "ACTIVE" {
		t.Fatalf("Account reader did not observe the activated projection: %#v", projected)
	}
	proof, err := proofSource.Proof(ctx, uuid.New(), artifacts)
	if err != nil || proof.Response.Verdict != "ALLOW" || proof.Response.ReasonCode != "PROOF_VALID" {
		t.Fatalf("activated cross-domain proof=%+v err=%v", proof.Response, err)
	}

	transitionEntitlementCrossDomainAccountControl(t, ctx, accountConnection)
	requester = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "usage-repair-chain-requester")
	approver = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "usage-repair-chain-approver")
	requesterPrincipal, approverPrincipal = approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	draft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "usage-repair-cross-domain-chain",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "cross-domain usage settlement repair",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`),
		CriticalResourceVersions: frozen.CriticalResourceVersions, FinancialSnapshot: frozen.FinancialSnapshot,
	}
	created, err := repository.CreateApprovalDraft(ctx, requesterPrincipal, draft)
	if err != nil {
		t.Fatalf("cross-domain draft create: %v", err)
	}
	submitted, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, created.ID, created.Version)
	if err != nil {
		t.Fatalf("cross-domain draft submit: %v", err)
	}
	requestID := submitted.ID
	approved, err := repository.DecideApprovalRequest(ctx, approverPrincipal, requestID, submitted.Version, "APPROVED", "cross-domain usage settlement repair", frozen.CriticalResourceVersions)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatalf("cross-domain proof approval status=%s err=%v", approved.Status, err)
	}
	var eventID uuid.UUID
	var raw []byte
	if _, err := accountConnection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	if err := accountConnection.QueryRow(ctx, `SELECT o.event_id,s.delivery_bytes
FROM outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'`, requestID).Scan(&eventID, &raw); err != nil {
		t.Fatalf("proof wire event=%v err=%v", eventID, err)
	}
	if _, err := accountConnection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	relayClaims, err := relay.ClaimFrozen(ctx, "usage-repair-cross-relay", 1, 30*time.Second)
	if err != nil || len(relayClaims) != 1 || relayClaims[0].EventID != eventID {
		t.Fatalf("relay claims=%+v err=%v", relayClaims, err)
	}
	disposition, err := relay.ProcessFrozen(ctx, "usage-repair-cross-relay", relayClaims[0], 12, time.Now().UTC(),
		func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
			if message.EventID != eventID || !bytes.Equal(message.Body, raw) {
				t.Fatal("frozen authorization publication changed")
			}
			return events.FrozenPublishResult{Disposition: events.FrozenPublished}
		})
	if err != nil || disposition != events.FrozenPublished {
		t.Fatalf("relay publication disposition=%s err=%v", disposition, err)
	}
	accountServer := httptest.NewServer(NewService(repository, nil).WithServiceToken("unused-static-token").Handler(nil))
	t.Cleanup(accountServer.Close)
	gate, err := billingdomain.NewHTTPApprovalExecutionGate(accountServer.URL, executorToken)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, ok := approvalconsumer.LookupDurable("billing-usage-settlement-repair-executor-p1-v1")
	if !ok {
		t.Fatal("usage settlement repair consumer durable is missing")
	}
	route, err := gate.ResolveAuthorizationRoute(ctx, eventID)
	if err != nil || route.CanonicalDurable != descriptor.Durable {
		t.Fatalf("authorization route=%+v err=%v", route, err)
	}
	if _, err := gate.ResolveAuthorizationBinding(ctx, eventID, descriptor.Durable); err != nil {
		t.Fatalf("authorization binding err=%v", err)
	}
	decoded, err := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
	})
	if err != nil {
		t.Fatalf("authorization wire decode route=%+v err=%v", route, err)
	}
	executionInput, err := approvalencoding.LegacyExecutionInput(decoded)
	if err != nil {
		t.Fatalf("event-bound execution input: %v", err)
	}
	eventArtifacts, err := approvalencoding.ConvertLegacyProofInput(executionInput)
	if err != nil {
		t.Fatalf("event-bound proof artifacts: %v", err)
	}
	if preflight, err := proofSource.Proof(ctx, requestID, eventArtifacts); err != nil {
		t.Fatalf("event-bound proof preflight: %v verdict=%s reason=%s", err, preflight.Response.Verdict, preflight.Response.ReasonCode)
	}
	provider := &paymentRepairE2EProvider{}
	coordinator, err := billingdomain.NewPaymentCoordinatorWithProvider(billingRepository, provider)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := billingdomain.NewAuthorizationCommandConsumer(billingdomain.NewService(billingRepository).WithPayments(coordinator), gate, descriptor.Durable)
	if err != nil {
		t.Fatal(err)
	}
	deliveries := []*entitlementCrossDomainDelivery{
		{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 1}},
		{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 2}},
	}
	acked := 0
	for _, delivery := range deliveries {
		if acked > 0 {
			break
		}
		consumer.Handle(ctx, delivery)
		acked = delivery.ack
	}
	if acked != 1 {
		var state, lastError string
		var attempts int
		if _, err := accountConnection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
			t.Fatal(err)
		}
		_ = accountConnection.QueryRow(ctx, `SELECT state,COALESCE(last_error_code,''),domain_attempt_count
FROM account_approval_authorization.approval_delivery_states WHERE approval_request_id=$1`, requestID).
			Scan(&state, &lastError, &attempts)
		_, _ = accountConnection.Exec(ctx, `RESET ROLE; SET ROLE account_service`)
		var caseCount, claimCount, journalCount int64
		_ = billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_settlement_repair_cases WHERE approval_request_id IS NOT NULL),
			(SELECT count(*) FROM billing_approval_proof_claims),
			(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'usage-settlement-repair:%')`).
			Scan(&caseCount, &claimCount, &journalCount)
		t.Fatalf("cross-domain execution was not acknowledged: acked=%d state=%s last_error=%s attempts=%d billing=%d/%d/%d",
			acked, state, lastError, attempts, caseCount, claimCount, journalCount)
	}
	var expected, settled, overage, componentAvailable, componentUsed, lotRemaining, walletPosted, allocationSettled int64
	if err := billingPool.QueryRow(ctx, `SELECT expected_microcredits,settled_microcredits,overage_microcredits
FROM usage_settlement_repair_cases WHERE approval_request_id=$1`, requestID).Scan(&expected, &settled, &overage); err != nil ||
		expected != 100 || settled != 100 || overage != 0 {
		t.Fatalf("repair case=%d/%d/%d err=%v", expected, settled, overage, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT component.available_microcredits,component.used_microcredits,
lot.remaining_microcredits FROM entitlement_components component
JOIN credit_lots lot ON lot.id=component.credit_lot_id WHERE component.id=$1`, loss.ComponentID).
		Scan(&componentAvailable, &componentUsed, &lotRemaining); err != nil ||
		componentAvailable != 0 || componentUsed != 100 || lotRemaining != 0 {
		t.Fatalf("component=%d/%d lot=%d err=%v", componentAvailable, componentUsed, lotRemaining, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, loss.WalletID).Scan(&walletPosted); err != nil ||
		walletPosted != 0 {
		t.Fatalf("wallet posted=%d err=%v", walletPosted, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT settled_microcredits FROM usage_hold_allocations
WHERE hold_id=$1 AND component_id=$2`, loss.HoldID, loss.ComponentID).Scan(&allocationSettled); err != nil ||
		allocationSettled != 100 {
		t.Fatalf("allocation settled=%d err=%v", allocationSettled, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT status FROM usage_loss_reviews WHERE id=$1`, loss.LossReviewID).Scan(&reviewStatus); err != nil ||
		reviewStatus != "RESOLVED_SETTLED" {
		t.Fatalf("loss review=%s err=%v", reviewStatus, err)
	}
	var entries, postings, ledgerSum int64
	if err := billingPool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM journal_entries WHERE business_reference=$1),
 (SELECT count(*) FROM ledger_postings posting JOIN journal_entries journal ON journal.id=posting.journal_entry_id
   WHERE journal.business_reference=$1),
 (SELECT COALESCE(sum(posting.delta_microcredits),0) FROM ledger_postings posting
   JOIN journal_entries journal ON journal.id=posting.journal_entry_id WHERE journal.business_reference=$1)`,
		"usage-settlement-repair:"+loss.UsageRequestID.String()).Scan(&entries, &postings, &ledgerSum); err != nil {
		t.Fatalf("journal read err=%v", err)
	}
	if entries != 1 || postings != 2 || ledgerSum != 0 {
		t.Fatalf("journal entries=%d postings=%d sum=%d", entries, postings, ledgerSum)
	}
	var requestStatus, deliveryState string
	if _, err := accountConnection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	if err := accountConnection.QueryRow(ctx, `SELECT a.status,d.state FROM approval_requests a
JOIN account_approval_authorization.approval_delivery_states d ON d.approval_request_id=a.id WHERE a.id=$1`, requestID).Scan(&requestStatus, &deliveryState); err != nil ||
		requestStatus != "EXECUTED" || deliveryState != "TERMINAL_CONFIRMED" {
		t.Fatalf("cross-domain terminal state request=%s delivery=%s err=%v", requestStatus, deliveryState, err)
	}
	if _, err := accountConnection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	// The recovery authority must confirm the same committed result the delivery
	// path accepted; a claimed but unprovable execution may only ever answer
	// UNCONFIRMED (never a zero-effect proof).
	recovery, err := billingRepository.ObserveBillingApprovalResult(ctx, requestID, eventID, route.ApprovalEpoch, "USAGE_SETTLEMENT_REPAIR")
	if err != nil || recovery.State != "SUCCEEDED" || len(recovery.DomainResult) == 0 || len(recovery.LedgerReferences) == 0 {
		t.Fatalf("cross-domain recovery result state=%s domain=%s ledger=%s err=%v", recovery.State, recovery.DomainResult, recovery.LedgerReferences, err)
	}
	var recoveryPayload struct {
		CaseID               uuid.UUID `json:"case_id"`
		JournalEntryID       uuid.UUID `json:"journal_entry_id"`
		ExpectedMicrocredits int64     `json:"expected_microcredits"`
		Status               string    `json:"status"`
	}
	var recoveryLedger struct {
		JournalEntryID uuid.UUID `json:"journal_entry_id"`
	}
	if json.Unmarshal(recovery.DomainResult, &recoveryPayload) != nil || json.Unmarshal(recovery.LedgerReferences, &recoveryLedger) != nil ||
		recoveryPayload.CaseID == uuid.Nil || recoveryPayload.ExpectedMicrocredits != 100 || recoveryPayload.Status != "SUCCEEDED" ||
		recoveryLedger.JournalEntryID != recoveryPayload.JournalEntryID {
		t.Fatalf("cross-domain recovery payload=%s ledger=%s", recovery.DomainResult, recovery.LedgerReferences)
	}
	// An unknown command type stays a definite refusal instead of a fake result.
	if _, err := billingRepository.ObserveBillingApprovalResult(ctx, requestID, eventID, route.ApprovalEpoch, "FUTURE_REPAIR"); err == nil {
		t.Fatal("an unregistered command must not produce a recovery result")
	}
	replay := &entitlementCrossDomainDelivery{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 3}}
	consumer.Handle(ctx, replay)
	if replay.ack != 1 {
		t.Fatalf("idempotent replay was not acknowledged: ack=%d", replay.ack)
	}
	var replayCases, replayClaims, replayJournals int64
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_settlement_repair_cases WHERE approval_request_id IS NOT NULL),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'usage-settlement-repair:%')`).
		Scan(&replayCases, &replayClaims, &replayJournals); err != nil ||
		replayCases != 1 || replayClaims != 1 || replayJournals != 1 {
		t.Fatalf("idempotent replay effects cases=%d claims=%d journals=%d err=%v", replayCases, replayClaims, replayJournals, err)
	}
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1
WHERE command_type='USAGE_SETTLEMENT_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 AND state='ACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("rolled-back cross-domain advancement: %v", err)
	}
	rolledBack, err := activationReader.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if projected, ok := rolledBack.Find(policy.CommandType, 1, 1); !ok || projected.State != "INACTIVE" {
		t.Fatalf("Account reader did not observe the rolled-back projection: %#v", projected)
	}
	if provider.calls != 0 {
		t.Fatalf("usage settlement repair called the provider %d times", provider.calls)
	}
}
