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

// TestReceivableWriteOffCrossDomainActivationPostgresIntegration runs the real
// Account and Billing databases with the real Billing HTTP activation and proof
// authority for the GSA-26 receivable write-off command. While the deployed
// v1/s1 activation row is INACTIVE the Account entry points refuse with the
// Billing governance error, the proof authority refuses to issue proof and
// neither side records an effect; once a schema-owner activation flips the row,
// the reader and proof authority honor it, the dedicated durable consumer
// commits exactly one write-off with the receivable/wallet/ledger projections,
// the Account request reaches EXECUTED/TERMINAL_CONFIRMED, an idempotent replay
// adds nothing and flipping the row back closes the command again.
func TestReceivableWriteOffCrossDomainActivationPostgresIntegration(t *testing.T) {
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if accountURL == "" || billingURL == "" {
		t.Skip("real Account and Billing PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	repository, accountConnection, relay, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, accountURL, 0)
	billingRepository, billingPool, billingConnection, writeOff := newReceivableWriteOffCrossDomainBillingFixture(t, ctx, billingURL)
	proofToken, executorToken := configureEntitlementProofIdentity(t)
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepository).Handler("account-proof-test"))
	t.Cleanup(billingServer.Close)
	activationReader, err := billinggovernance.NewClient(billingServer.URL, proofToken)
	if err != nil {
		t.Fatal(err)
	}
	repository.WithBillingGovernanceActivation(activationReader)
	policy, err := approvalpolicy.Lookup("RECEIVABLE_WRITE_OFF")
	if err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 1 || policy.SnapshotSchemaVersion != 1 || !approvalpolicy.Executable(policy.CommandType) {
		t.Fatalf("receivable write-off must be registered against the live Billing activation: %#v", policy)
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
				t.Fatalf("write-off activation is not the deployed INACTIVE v1/s1 row: %#v", activation)
			}
		}
	}
	if !found {
		t.Fatal("write-off activation row is missing")
	}

	command := billingdomain.ReceivableWriteOffCommandInput{
		ReceivableID: writeOff.ReceivableID, WriteOffMicrocredits: writeOff.WriteOffMicrocredits, ExpectedReceivableVersion: 1,
	}
	frozen, err := billingRepository.ReceivableWriteOffProofInput(ctx, command)
	if err != nil {
		t.Fatalf("write-off proof input: %v", err)
	}
	if frozen.TargetResourceType != "RECEIVABLE" || frozen.TargetResourceID != writeOff.ReceivableID.String() {
		t.Fatalf("write-off frozen target=%s/%s", frozen.TargetResourceType, frozen.TargetResourceID)
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
	presentedCommand, ok := detail.Command.(approvalReceivableWriteOffSummary)
	if !ok || presentedCommand.ReceivableID != writeOff.ReceivableID || presentedCommand.SourceType != "USAGE_OVERAGE" ||
		presentedCommand.SourceReference != writeOff.SourceReference || presentedCommand.OriginalMicrocredits != "30" ||
		presentedCommand.SettledMicrocredits != "0" || presentedCommand.OutstandingMicrocredits != "30" ||
		presentedCommand.WriteOffMicrocredits != "30" || presentedCommand.WalletReceivableMicrocredits != "30" ||
		presentedCommand.ReasonCode != "APPROVED_RECEIVABLE_WRITE_OFF" || !approvalPaginationHexDigest(presentedCommand.FactsDigest) {
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
	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "write-off-activation-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "write-off-activation-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	createDraft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "write-off-activation-create",
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
	var cases, claims, writeOffJournals, walletReceivable, walletPosted int64
	var financialStatus string
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM receivable_write_off_cases),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%'),
		(SELECT receivable_microcredits FROM wallets WHERE id=$1),
		(SELECT posted_microcredits FROM wallets WHERE id=$1),
		(SELECT financial_status FROM wallets WHERE id=$1)`, writeOff.WalletID).
		Scan(&cases, &claims, &writeOffJournals, &walletReceivable, &walletPosted, &financialStatus); err != nil ||
		cases != 0 || claims != 0 || writeOffJournals != 0 || walletReceivable != writeOff.WalletReceivableCredits ||
		walletPosted != 100 || financialStatus != "RECEIVABLE_OPEN" {
		t.Fatalf("closed cross-domain gate wrote Billing effects: cases=%d claims=%d journals=%d wallet=%d/%d/%s err=%v",
			cases, claims, writeOffJournals, walletReceivable, walletPosted, financialStatus, err)
	}

	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1
WHERE command_type='RECEIVABLE_WRITE_OFF' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`); err != nil {
		t.Fatal(err)
	}
	// The frozen revision binds the activation version, so a request assembled
	// while the command was INACTIVE must be re-assembled after the governed
	// flip; activation itself must never be applied to a frozen request.
	frozen, err = billingRepository.ReceivableWriteOffProofInput(ctx, command)
	if err != nil {
		t.Fatalf("activated write-off proof input: %v", err)
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
	requester = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "write-off-chain-requester")
	approver = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "write-off-chain-approver")
	requesterPrincipal, approverPrincipal = approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
	draft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "write-off-cross-domain-chain",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "cross-domain receivable write-off",
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
	approved, err := repository.DecideApprovalRequest(ctx, approverPrincipal, requestID, submitted.Version, "APPROVED", "cross-domain receivable write-off", frozen.CriticalResourceVersions)
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
	relayClaims, err := relay.ClaimFrozen(ctx, "write-off-cross-relay", 1, 30*time.Second)
	if err != nil || len(relayClaims) != 1 || relayClaims[0].EventID != eventID {
		t.Fatalf("relay claims=%+v err=%v", relayClaims, err)
	}
	disposition, err := relay.ProcessFrozen(ctx, "write-off-cross-relay", relayClaims[0], 12, time.Now().UTC(),
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
	descriptor, ok := approvalconsumer.LookupDurable("billing-receivable-write-off-executor-p1-v1")
	if !ok {
		t.Fatal("write-off consumer durable is missing")
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
		var caseCount, claimCount, journalCount int64
		_ = billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM receivable_write_off_cases),
			(SELECT count(*) FROM billing_approval_proof_claims),
			(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%')`).Scan(&caseCount, &claimCount, &journalCount)
		t.Fatalf("cross-domain execution was not acknowledged: acked=%d state=%s last_error=%s attempts=%d billing=%d/%d/%d",
			acked, state, lastError, attempts, caseCount, claimCount, journalCount)
	}
	var settled, outstanding, receivableVersion int64
	var statusAfter string
	if err := billingPool.QueryRow(ctx, `SELECT settled_microcredits,outstanding_microcredits,status,version FROM receivables WHERE id=$1`, writeOff.ReceivableID).
		Scan(&settled, &outstanding, &statusAfter, &receivableVersion); err != nil ||
		settled != 30 || outstanding != 0 || statusAfter != "WRITTEN_OFF" || receivableVersion != 2 {
		t.Fatalf("cross-domain receivable=%d/%d/%s/%d err=%v", settled, outstanding, statusAfter, receivableVersion, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT receivable_microcredits,posted_microcredits,financial_status FROM wallets WHERE id=$1`, writeOff.WalletID).
		Scan(&walletReceivable, &walletPosted, &financialStatus); err != nil ||
		walletReceivable != 0 || walletPosted != 100 || financialStatus != "CLEAR" {
		t.Fatalf("cross-domain wallet=%d/%d/%s err=%v", walletReceivable, walletPosted, financialStatus, err)
	}
	var executedCases, executedClaims, executedJournals int64
	var journalID uuid.UUID
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM receivable_write_off_cases),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%'),
		(SELECT journal_entry_id FROM receivable_write_off_cases WHERE approval_request_id=$1)`, requestID).
		Scan(&executedCases, &executedClaims, &executedJournals, &journalID); err != nil ||
		executedCases != 1 || executedClaims != 1 || executedJournals != 1 || journalID == uuid.Nil {
		t.Fatalf("cross-domain effects cases=%d claims=%d journals=%d journal=%v err=%v",
			executedCases, executedClaims, executedJournals, journalID, err)
	}
	var entryType, reference string
	var postingCount int
	var receivableDelta, adjustmentDelta int64
	if err := billingPool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, journalID).
		Scan(&entryType, &reference); err != nil || entryType != "ADJUSTMENT" || !strings.HasPrefix(reference, "write-off:") {
		t.Fatalf("cross-domain journal=%s/%s err=%v", entryType, reference, err)
	}
	if err := billingPool.QueryRow(ctx, `SELECT count(*),
		COALESCE(max(delta_microcredits) FILTER (WHERE delta_microcredits>0),0),
		COALESCE(max(delta_microcredits) FILTER (WHERE delta_microcredits<0),0)
	FROM ledger_postings WHERE journal_entry_id=$1`, journalID).
		Scan(&postingCount, &receivableDelta, &adjustmentDelta); err != nil ||
		postingCount != 2 || receivableDelta != 30 || adjustmentDelta != -30 {
		t.Fatalf("cross-domain postings=%d/%d/%d err=%v", postingCount, receivableDelta, adjustmentDelta, err)
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
	replay := &entitlementCrossDomainDelivery{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 3}}
	consumer.Handle(ctx, replay)
	if replay.ack != 1 {
		t.Fatalf("idempotent replay was not acknowledged: ack=%d", replay.ack)
	}
	var replayCases, replayClaims, replayJournals int64
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM receivable_write_off_cases),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE business_reference LIKE 'write-off:%')`).Scan(&replayCases, &replayClaims, &replayJournals); err != nil ||
		replayCases != 1 || replayClaims != 1 || replayJournals != 1 {
		t.Fatalf("idempotent replay effects cases=%d claims=%d journals=%d err=%v", replayCases, replayClaims, replayJournals, err)
	}
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1
WHERE command_type='RECEIVABLE_WRITE_OFF' AND policy_version=1 AND snapshot_schema_version=1 AND state='ACTIVE'`); err != nil {
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
