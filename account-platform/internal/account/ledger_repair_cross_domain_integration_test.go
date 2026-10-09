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

// TestLedgerRepairCrossDomainActivationPostgresIntegration runs the real Account
// and Billing databases with the real Billing HTTP activation and proof
// authority for the GSA-24 ledger repair command. While the deployed v1/s1
// activation row is INACTIVE the Account entry points refuse with the Billing
// governance error, the proof authority refuses to issue proof and neither side
// records an effect; once a schema-owner activation flips the row, the Account
// reader and proof authority honor it, the dedicated durable consumer commits
// exactly one paired REVERSAL ledger and its immutable case row, the Account
// request reaches EXECUTED/TERMINAL_CONFIRMED, an idempotent replay adds nothing
// and flipping the row back closes the command again.
func TestLedgerRepairCrossDomainActivationPostgresIntegration(t *testing.T) {
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if accountURL == "" || billingURL == "" {
		t.Skip("real Account and Billing PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	repository, accountConnection, relay, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, accountURL, 0)
	billingRepository, billingPool, billingConnection, repair := newLedgerRepairCrossDomainBillingFixture(t, ctx, billingURL)
	proofToken, executorToken := configureEntitlementProofIdentity(t)
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepository).Handler("account-proof-test"))
	t.Cleanup(billingServer.Close)
	activationReader, err := billinggovernance.NewClient(billingServer.URL, proofToken)
	if err != nil {
		t.Fatal(err)
	}
	repository.WithBillingGovernanceActivation(activationReader)
	policy, err := approvalpolicy.Lookup("LEDGER_REPAIR")
	if err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 1 || policy.SnapshotSchemaVersion != 1 || !approvalpolicy.Executable(policy.CommandType) {
		t.Fatalf("ledger repair must be registered against the live Billing activation: %#v", policy)
	}

	// The deployed projection keeps the original INACTIVE v1/s1 row.
	activations, err := billingRepository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, activation := range activations.Activations {
		if activation.CommandType == policy.CommandType {
			found = true
			if activation.State != "INACTIVE" || activation.PolicyVersion != policy.PolicyVersion || int64(activation.SnapshotSchemaVersion) != int64(policy.SnapshotSchemaVersion) {
				t.Fatalf("ledger repair activation is not the deployed INACTIVE v1 row: %#v", activation)
			}
		}
	}
	if !found {
		t.Fatal("ledger repair activation row is missing")
	}
	var ledgerRepairRows int64
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activations
WHERE command_type='LEDGER_REPAIR' AND policy_version=1 AND snapshot_schema_version=1`).Scan(&ledgerRepairRows); err != nil || ledgerRepairRows != 1 {
		t.Fatalf("Billing ledger repair activation rows=%d err=%v", ledgerRepairRows, err)
	}

	frozen, err := billingRepository.LedgerRepairProofInput(ctx, billingdomain.LedgerRepairCommandInput{
		TargetJournalEntryID: repair.JournalEntryID, TargetPostingID: repair.TargetPostingID,
		WrongLedgerAccountID: repair.WrongAccountID, CorrectLedgerAccountID: repair.CorrectAccountID,
		AmountMicrocredits: repair.AmountMicrocredits,
	}, "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT")
	if err != nil {
		t.Fatalf("ledger repair proof input: %v", err)
	}
	if frozen.TargetResourceType != "LEDGER_POSTING" || frozen.TargetResourceID != repair.TargetPostingID.String() {
		t.Fatalf("ledger repair frozen target=%s/%s", frozen.TargetResourceType, frozen.TargetResourceID)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}

	// The closed read-only presenter renders only the frozen owner projection.
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
	presentedCommand, ok := detail.Command.(approvalLedgerRepairSummary)
	if !ok || presentedCommand.TargetJournalEntryID != repair.JournalEntryID || presentedCommand.TargetPostingID != repair.TargetPostingID ||
		presentedCommand.EntryType != "USAGE" || presentedCommand.AmountMicrocredits != "30" ||
		presentedCommand.ReasonCode != "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT" || !approvalPaginationHexDigest(presentedCommand.PostingSetDigest) {
		t.Fatalf("cross-domain presenter command=%#v", detail.Command)
	}
	proofSource, err := NewHTTPApprovalProofSource(billingServer.URL, proofToken)
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

	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "ledger-repair-activation-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "ledger-repair-activation-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	createDraft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "ledger-repair-activation-create",
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

	var decisions, outbox, executions int
	if err := repository.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
		(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1),
		(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1)`, approveID).Scan(&decisions, &outbox, &executions); err != nil || decisions != 0 || outbox != 0 || executions != 0 {
		t.Fatalf("closed cross-domain approval created effects: %d/%d/%d err=%v", decisions, outbox, executions, err)
	}
	var closedCases, closedClaims, closedJournals int64
	var closedWalletBalance int64
	if err := billingPool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM ledger_repair_cases),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'ledger-repair:%'),
		(SELECT posted_microcredits FROM wallets WHERE id=$1)`, repair.WalletID).
		Scan(&closedCases, &closedClaims, &closedJournals, &closedWalletBalance); err != nil ||
		closedCases != 0 || closedClaims != 0 || closedJournals != 0 || closedWalletBalance != 100 {
		t.Fatalf("closed cross-domain gate wrote Billing effects: cases=%d claims=%d journals=%d wallet=%d err=%v",
			closedCases, closedClaims, closedJournals, closedWalletBalance, err)
	}

	// Activation capability: a governed schema-owner flip of the v1/s1 row is
	// honored by the Account reader and the real proof authority without writing
	// any domain effect by itself.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1
WHERE command_type='LEDGER_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`); err != nil {
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
	var proofClaims int64
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims`).Scan(&proofClaims); err != nil || proofClaims != 0 {
		t.Fatalf("proof issuance wrote a claim: %d err=%v", proofClaims, err)
	}

	// Real Account -> Billing authorization chain while the command is activated.
	transitionEntitlementCrossDomainAccountControl(t, ctx, accountConnection)
	requester = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "ledger-repair-chain-requester")
	approver = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "ledger-repair-chain-approver")
	requesterPrincipal, approverPrincipal = approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	draft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "ledger-repair-cross-domain-chain",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "cross-domain ledger repair",
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
	approved, err := repository.DecideApprovalRequest(ctx, approverPrincipal, requestID, submitted.Version, "APPROVED", "cross-domain ledger repair", frozen.CriticalResourceVersions)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatalf("cross-domain proof approval status=%s err=%v", approved.Status, err)
	}
	var eventID uuid.UUID
	var raw []byte
	var serialization string
	if _, err := accountConnection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	if err := accountConnection.QueryRow(ctx, `SELECT o.event_id,s.delivery_bytes,s.serialization_version
FROM outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id
WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'`, requestID).Scan(&eventID, &raw, &serialization); err != nil ||
		serialization != approvalencoding.ApprovalSerializationVersion {
		t.Fatalf("proof wire event=%v serialization=%s err=%v", eventID, serialization, err)
	}
	if _, err := accountConnection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	relayClaims, err := relay.ClaimFrozen(ctx, "ledger-repair-cross-relay", 1, 30*time.Second)
	if err != nil || len(relayClaims) != 1 || relayClaims[0].EventID != eventID {
		t.Fatalf("relay claims=%+v err=%v", relayClaims, err)
	}
	disposition, err := relay.ProcessFrozen(ctx, "ledger-repair-cross-relay", relayClaims[0], 12, time.Now().UTC(),
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
	descriptor, ok := approvalconsumer.LookupDurable("billing-ledger-repair-executor-p1-v1")
	if !ok {
		t.Fatal("ledger repair consumer durable is missing")
	}
	route, err := gate.ResolveAuthorizationRoute(ctx, eventID)
	if err != nil || route.CanonicalDurable != descriptor.Durable {
		t.Fatalf("authorization route=%+v err=%v", route, err)
	}
	binding, err := gate.ResolveAuthorizationBinding(ctx, eventID, descriptor.Durable)
	if err != nil || binding.ExecutorIdentity != "billing-command-executor" {
		t.Fatalf("authorization binding=%+v err=%v", binding, err)
	}
	decoded, err := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
	})
	if err != nil {
		t.Fatalf("authorization wire decode route=%+v err=%v", route, err)
	}
	// Pre-flight: the consumer normalises the wire event back to the domain form
	// and recomputes the proof artifacts from those frozen bytes, so the same
	// artifacts must still yield the approval's ALLOW. Without this the consumer
	// could only report an unknown outcome.
	executionInput, err := approvalencoding.LegacyExecutionInput(decoded)
	if err != nil {
		t.Fatalf("event-bound execution input: %v", err)
	}
	eventArtifacts, err := approvalencoding.ConvertLegacyProofInput(executionInput)
	if err != nil {
		t.Fatalf("event-bound proof artifacts: %v", err)
	}
	preflight, err := proofSource.Proof(ctx, requestID, eventArtifacts)
	if err != nil {
		t.Fatalf("event-bound proof preflight: %v verdict=%s reason=%s", err, preflight.Response.Verdict, preflight.Response.ReasonCode)
	}
	consumer, err := billingdomain.NewAuthorizationCommandConsumer(billingdomain.NewService(billingRepository), gate, descriptor.Durable)
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
		var requestState, executionStatus, executionClass string
		var executions int
		_ = accountConnection.QueryRow(ctx, `SELECT r.status,
			(SELECT count(*) FROM approval_executions e WHERE e.approval_request_id=r.id),
			COALESCE((SELECT e.status FROM approval_executions e WHERE e.approval_request_id=r.id ORDER BY e.id LIMIT 1),''),
			COALESCE((SELECT COALESCE(e.error_class,'') FROM approval_executions e WHERE e.approval_request_id=r.id ORDER BY e.id LIMIT 1),'')
			FROM approval_requests r WHERE r.id=$1`, requestID).Scan(&requestState, &executions, &executionStatus, &executionClass)
		var caseCount, claimCount, journalCount int64
		_ = billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger_repair_cases),
			(SELECT count(*) FROM billing_approval_proof_claims),
			(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'ledger-repair:%')`).Scan(&caseCount, &claimCount, &journalCount)
		t.Fatalf("cross-domain execution was not acknowledged: acked=%d state=%s last_error=%s attempts=%d request=%s execution=%s/%d/%s billing=%d/%d/%d",
			acked, state, lastError, attempts, requestState, executionStatus, executions, executionClass, caseCount, claimCount, journalCount)
	}

	var caseID, repairedJournalID, repairedWrongAccount, repairedCorrectAccount uuid.UUID
	var caseState, caseEntryType string
	var caseAmount int64
	if err := billingPool.QueryRow(ctx, `SELECT id,journal_entry_id,wrong_ledger_account_id,correct_ledger_account_id,entry_type,amount_microcredits,state
FROM ledger_repair_cases WHERE approval_request_id=$1`, requestID).
		Scan(&caseID, &repairedJournalID, &repairedWrongAccount, &repairedCorrectAccount, &caseEntryType, &caseAmount, &caseState); err != nil ||
		caseID == uuid.Nil || repairedJournalID == uuid.Nil || repairedWrongAccount != repair.WrongAccountID ||
		repairedCorrectAccount != repair.CorrectAccountID || caseEntryType != "USAGE" || caseAmount != repair.AmountMicrocredits || caseState != "EXECUTED" {
		t.Fatalf("cross-domain repair case=%v/%v/%v/%v/%s/%d/%s err=%v",
			caseID, repairedJournalID, repairedWrongAccount, repairedCorrectAccount, caseEntryType, caseAmount, caseState, err)
	}
	var repairedEntryType, repairedReference string
	var postingCount int
	var wrongDelta, correctDelta int64
	if err := billingPool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, repairedJournalID).
		Scan(&repairedEntryType, &repairedReference); err != nil ||
		repairedEntryType != "REVERSAL" || repairedReference != "ledger-repair:"+repair.TargetPostingID.String() {
		t.Fatalf("cross-domain repair journal=%s/%s err=%v", repairedEntryType, repairedReference, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT count(*),
		COALESCE(max(delta_microcredits) FILTER (WHERE ledger_account_id=$2),0),
		COALESCE(max(delta_microcredits) FILTER (WHERE ledger_account_id=$3),0)
	FROM ledger_postings WHERE journal_entry_id=$1`, repairedJournalID, repair.WrongAccountID, repair.CorrectAccountID).
		Scan(&postingCount, &wrongDelta, &correctDelta); err != nil ||
		postingCount != 2 || wrongDelta != -repair.AmountMicrocredits || correctDelta != repair.AmountMicrocredits {
		t.Fatalf("cross-domain repair postings=%d wrong=%d correct=%d err=%v", postingCount, wrongDelta, correctDelta, err)
	}
	var targetAccount uuid.UUID
	var targetDelta int64
	if err := billingPool.QueryRow(ctx, `SELECT ledger_account_id,delta_microcredits FROM ledger_postings WHERE id=$1`, repair.TargetPostingID).
		Scan(&targetAccount, &targetDelta); err != nil || targetAccount != repair.WrongAccountID || targetDelta != repair.AmountMicrocredits {
		t.Fatalf("target posting was rewritten: %v/%d err=%v", targetAccount, targetDelta, err)
	}
	var executedCases, executedClaims, executedJournals, walletBalance int64
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger_repair_cases),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'ledger-repair:%'),
		(SELECT posted_microcredits FROM wallets WHERE id=$1)`, repair.WalletID).
		Scan(&executedCases, &executedClaims, &executedJournals, &walletBalance); err != nil ||
		executedCases != 1 || executedClaims != 1 || executedJournals != 1 || walletBalance != 100 {
		t.Fatalf("cross-domain effects cases=%d claims=%d journals=%d wallet=%d err=%v",
			executedCases, executedClaims, executedJournals, walletBalance, err)
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

	// Re-delivering the same authorized event must read back the same effect and
	// must not create a second case, journal or claim.
	replay := &entitlementCrossDomainDelivery{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 3}}
	consumer.Handle(ctx, replay)
	if replay.ack != 1 {
		t.Fatalf("idempotent replay was not acknowledged: ack=%d", replay.ack)
	}
	var replayCases, replayClaims, replayJournals int64
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger_repair_cases),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'ledger-repair:%')`).Scan(&replayCases, &replayClaims, &replayJournals); err != nil ||
		replayCases != 1 || replayClaims != 1 || replayJournals != 1 {
		t.Fatalf("idempotent replay effects cases=%d claims=%d journals=%d err=%v", replayCases, replayClaims, replayJournals, err)
	}

	// Rolling the activation row back closes the command again without rewriting
	// the same v1/s1 identity.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1
WHERE command_type='LEDGER_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 AND state='ACTIVE'`); err != nil {
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
}
