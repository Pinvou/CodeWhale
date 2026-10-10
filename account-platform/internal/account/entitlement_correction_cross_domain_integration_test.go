package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/pem"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

// configureEntitlementProofIdentity gives the real Billing proof authority the
// signed Account workload identity it requires, and the Account execution gate
// the signed Billing executor identity. The rules grant only the read
// permissions the Account service needs and the exact executor permissions the
// authorization consumer calls.
func configureEntitlementProofIdentity(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		modulus := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
		exponent := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "approval-executor", "n": modulus, "e": exponent,
		}}})
	}))
	t.Cleanup(issuer.Close)
	caFile := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	audience := "entitlement-cross-domain-integration"
	accountSubject, executorSubject := uuid.NewString(), uuid.NewString()
	rules, err := json.Marshal([]map[string]any{
		{
			"subject": accountSubject, "service": "account-service",
			"permissions": []string{"internal.billing.approval-proof.read", "internal.billing.approval-activation.read"},
		},
		{
			"subject": executorSubject, "service": "billing-command-executor",
			"permissions": []string{
				"internal.authorization.check", "internal.approval-authorization.consume",
				"internal.approval-execution.begin", "internal.approval-execution.complete",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", caFile)
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer.URL)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(rules))
	mint := func(subject string) string {
		now := time.Now().UTC()
		token, signErr := signEntitlementFixtureJWT(key, map[string]any{
			"iss": issuer.URL, "sub": subject, "aud": audience, "exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(),
		})
		if signErr != nil {
			t.Fatal(signErr)
		}
		return token
	}
	return mint(accountSubject), mint(executorSubject)
}

type entitlementCrossDomainDelivery struct {
	raw  []byte
	id   string
	meta events.AuthorizationDeliveryMetadata
	ack  int
}

func (delivery *entitlementCrossDomainDelivery) DeliveryBytes() []byte {
	return append([]byte(nil), delivery.raw...)
}
func (delivery *entitlementCrossDomainDelivery) MessageID() string { return delivery.id }
func (delivery *entitlementCrossDomainDelivery) Metadata() events.AuthorizationDeliveryMetadata {
	return delivery.meta
}
func (delivery *entitlementCrossDomainDelivery) DoubleAck(context.Context) error {
	delivery.ack++
	return nil
}
func (delivery *entitlementCrossDomainDelivery) NakWithDelay(time.Duration) error { return nil }
func (delivery *entitlementCrossDomainDelivery) PublishDeadLetter(context.Context) (events.AuthorizationDLQPubAck, error) {
	return events.AuthorizationDLQPubAck{}, errors.New("no DLQ capability in this fixture")
}

// TestEntitlementCorrectionCrossDomainActivationPostgresIntegration runs the
// real Account and Billing databases with the real Billing HTTP activation and
// proof authority. It proves the capability is wired without opening anything by
// default: while the deployed activation row is INACTIVE the Account entry
// points refuse with the Billing governance error, the proof authority refuses
// to issue proof and neither domain records an effect; once a schema-owner
// activation flips the appended v2 row, the Account reader and the real proof
// authority both honor it, and flipping the row back closes the command again.

// retryWhileWriterUnavailable absorbs the isolated fixture's short approval
// writer readiness window; the assertion under test is the governance error.
func retryWhileWriterUnavailable(t *testing.T, ctx context.Context, repository *PostgresRepository, call func() error) error {
	t.Helper()
	var err error
	for attempt := 0; attempt < 40; attempt++ {
		err = call()
		if err == nil || !strings.Contains(err.Error(), "approval authorization writer is unavailable") {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func TestEntitlementCorrectionCrossDomainActivationPostgresIntegration(t *testing.T) {
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if accountURL == "" || billingURL == "" {
		t.Skip("real Account and Billing PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	repository, accountConnection, relay, _ := newEntitlementApprovalFixture(t, ctx, accountURL)
	billingRepository, billingPool, billingConnection := newEntitlementCrossDomainBillingFixture(t, ctx, billingURL)
	proofToken, executorToken := configureEntitlementProofIdentity(t)
	billingServer := httptest.NewServer(billingdomain.NewService(billingRepository).Handler("account-proof-test"))
	t.Cleanup(billingServer.Close)
	activationReader, err := billinggovernance.NewClient(billingServer.URL, proofToken)
	if err != nil {
		t.Fatal(err)
	}
	repository.WithBillingGovernanceActivation(activationReader)
	policy, err := approvalpolicy.Lookup("ENTITLEMENT_COMPONENT_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	if policy.PolicyVersion != 2 || policy.SnapshotSchemaVersion != 2 || !approvalpolicy.Executable(policy.CommandType) {
		t.Fatalf("entitlement correction must be registered against the live Billing activation: %#v", policy)
	}

	// The deployed projection stays INACTIVE and keeps the immutable v1 history.
	activations, err := billingRepository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	activation := activations.Activations[2]
	if activation.CommandType != policy.CommandType || activation.State != "INACTIVE" || activation.PolicyVersion != 2 || activation.SnapshotSchemaVersion != 2 {
		t.Fatalf("entitlement correction activation is not the deployed INACTIVE v2 row: %#v", activation)
	}
	var retainedV1 int64
	if err := billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activations
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`).Scan(&retainedV1); err != nil || retainedV1 != 1 {
		t.Fatalf("Billing retained v1 activation history=%d err=%v", retainedV1, err)
	}

	frozen, err := billingRepository.EntitlementCorrectionProofInput(ctx, entitlementCrossDomainPackageID, entitlementCrossDomainOrderID, "ORIGINAL_SPLIT_MISMATCH")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	// The closed read-only presenter must render exactly the frozen projection
	// the command was approved with: the target package and the reason code.
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
	if command, ok := detail.Command.(approvalEntitlementCorrectionSummary); !ok ||
		command.PackageID != entitlementCrossDomainPackageID || command.ReasonCode != "ORIGINAL_SPLIT_MISMATCH" {
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

	requester := seedEntitlementApprovalActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-activation-requester")
	approver := seedEntitlementApprovalActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-activation-approver")
	requesterPrincipal, approverPrincipal := entitlementApprovalPrincipal(requester), entitlementApprovalPrincipal(approver)
	createDraft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "entitlement-activation-create",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "closed cross-domain activation gate",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`),
		CriticalResourceVersions: frozen.CriticalResourceVersions, FinancialSnapshot: frozen.FinancialSnapshot,
	}
	if _, err := repository.CreateApprovalDraft(ctx, requesterPrincipal, createDraft); err == nil {
		t.Fatal("inactive cross-domain create unexpectedly succeeded")
	}
	assertEntitlementRequestAbsent(t, ctx, repository, requester.ID, createDraft.IdempotencyKey)

	submitID := seedEntitlementAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT", frozen)
	if err := retryWhileWriterUnavailable(t, ctx, repository, func() error {
		_, err := repository.SubmitApprovalRequest(ctx, requesterPrincipal, submitID, 1)
		return err
	}); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain submit: %v", err)
	}
	assertEntitlementRequestState(t, ctx, repository, submitID, "DRAFT", 1)

	approveID := seedEntitlementAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED", frozen)
	if err := retryWhileWriterUnavailable(t, ctx, repository, func() error {
		_, err := repository.DecideApprovalRequest(ctx, approverPrincipal, approveID, 1, "APPROVED", "closed cross-domain gate", frozen.CriticalResourceVersions)
		return err
	}); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("inactive cross-domain approve: %v", err)
	}
	assertEntitlementRequestState(t, ctx, repository, approveID, "SUBMITTED", 1)

	var decisions, outbox, executions int
	if err := repository.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),
		(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1),
		(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1)`, approveID).Scan(&decisions, &outbox, &executions); err != nil || decisions != 0 || outbox != 0 || executions != 0 {
		t.Fatalf("closed cross-domain approval created effects: %d/%d/%d err=%v", decisions, outbox, executions, err)
	}
	var corrections, claims, bonusComponents int64
	var sourceAvailable, sourceReclassified, sourceOriginal int64
	if err := billingPool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM entitlement_component_corrections),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM entitlement_components WHERE package_id=$1 AND component_type='BONUS'),
		COALESCE((SELECT available_microcredits FROM entitlement_components WHERE id=$2),0),
		COALESCE((SELECT reclassified_out_microcredits FROM entitlement_components WHERE id=$2),0),
		COALESCE((SELECT original_microcredits FROM entitlement_components WHERE id=$2),0)`, entitlementCrossDomainPackageID, entitlementCrossDomainComponentID).
		Scan(&corrections, &claims, &bonusComponents, &sourceAvailable, &sourceReclassified, &sourceOriginal); err != nil ||
		corrections != 0 || claims != 0 || bonusComponents != 0 || sourceAvailable != sourceOriginal || sourceReclassified != 0 || sourceOriginal != 100 {
		t.Fatalf("closed cross-domain gate wrote Billing effects: corrections=%d claims=%d bonus=%d source=%d/%d/%d err=%v",
			corrections, claims, bonusComponents, sourceAvailable, sourceReclassified, sourceOriginal, err)
	}

	// Activation capability: a governed schema-owner flip of the appended v2 row
	// is honored by the Account reader and the real proof authority, without
	// writing any domain effect by itself, and the v1 history stays untouched.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2 AND state='INACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); err != nil {
		t.Fatalf("activated cross-domain advancement: %v", err)
	}
	snapshot, err := activationReader.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projected, ok := snapshot.Find(policy.CommandType, 2, 2)
	if !ok || projected.State != "ACTIVE" {
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

	// Real Account -> Billing authorization chain while the command is activated:
	// the Account decision obtains a signed Billing proof, the authorized event is
	// published, and the dedicated durable consumer executes the paired ledger
	// effect before the terminal state is confirmed.

	requester = seedEntitlementApprovalActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-chain-requester")
	approver = seedEntitlementApprovalActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-chain-approver")
	requesterPrincipal, approverPrincipal = entitlementApprovalPrincipal(requester), entitlementApprovalPrincipal(approver)
	// The request is created through the same Account entry points the Portal
	// business entry will call: closed draft -> submit -> independent approval.
	draft := ApprovalDraft{
		CommandType: policy.CommandType, IdempotencyKey: "entitlement-cross-domain-chain",
		NormalizedCommand: frozen.NormalizedCommand, TargetResourceType: frozen.TargetResourceType,
		TargetResourceID: frozen.TargetResourceID, Reason: "cross-domain entitlement correction",
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
	approved, err := repository.DecideApprovalRequest(ctx, approverPrincipal, requestID, submitted.Version, "APPROVED", "cross-domain entitlement correction", frozen.CriticalResourceVersions)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatalf("cross-domain proof approval status=%s err=%v", approved.Status, err)
	}
	var eventID uuid.UUID
	var raw []byte
	var serialization string
	// The authorization sidecar schema is not readable by the Account runtime
	// identity; the verification read runs as its dedicated owner.
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
	relayClaims, err := relay.ClaimFrozen(ctx, "entitlement-cross-relay", 1, 30*time.Second)
	if err != nil || len(relayClaims) != 1 || relayClaims[0].EventID != eventID {
		t.Fatalf("relay claims=%+v err=%v", relayClaims, err)
	}
	disposition, err := relay.ProcessFrozen(ctx, "entitlement-cross-relay", relayClaims[0], 12, time.Now().UTC(),
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
	descriptor, ok := approvalconsumer.LookupDurable("billing-entitlement-component-adjustment-executor-p2-v1")
	if !ok {
		t.Fatal("entitlement consumer durable is missing")
	}
	route, err := gate.ResolveAuthorizationRoute(ctx, eventID)
	if err != nil || route.CanonicalDurable != descriptor.Durable {
		t.Fatalf("authorization route=%+v err=%v", route, err)
	}
	binding, err := gate.ResolveAuthorizationBinding(ctx, eventID, descriptor.Durable)
	if err != nil || binding.ExecutorIdentity != "billing-command-executor" {
		t.Fatalf("authorization binding=%+v err=%v", binding, err)
	}
	if _, err := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
	}); err != nil {
		t.Fatalf("authorization wire decode route=%+v err=%v", route, err)
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
		var correctionCount, claimCount, proofAttempts int64
		_ = billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_component_corrections),
			(SELECT count(*) FROM billing_approval_proof_claims),
			(SELECT count(*) FROM billing_approval_proof_claim_attempts)`).Scan(&correctionCount, &claimCount, &proofAttempts)
		t.Fatalf("cross-domain execution was not acknowledged: acked=%d state=%s last_error=%s attempts=%d billing=%d/%d/%d",
			acked, state, lastError, attempts, correctionCount, claimCount, proofAttempts)
	}
	var executedSourceAvailable, executedSourceReclassified, targetAvailable int64
	var targetComponentID, targetLotID uuid.UUID
	if err := billingPool.QueryRow(ctx, `SELECT
		(SELECT available_microcredits FROM entitlement_components WHERE id=$1),
		(SELECT reclassified_out_microcredits FROM entitlement_components WHERE id=$1),
		(SELECT available_microcredits FROM entitlement_components WHERE package_id=$2 AND component_type='BONUS'),
		(SELECT id FROM entitlement_components WHERE package_id=$2 AND component_type='BONUS'),
		(SELECT credit_lot_id FROM entitlement_components WHERE package_id=$2 AND component_type='BONUS')`,
		entitlementCrossDomainComponentID, entitlementCrossDomainPackageID).
		Scan(&executedSourceAvailable, &executedSourceReclassified, &targetAvailable, &targetComponentID, &targetLotID); err != nil ||
		executedSourceAvailable != 80 || executedSourceReclassified != 20 || targetAvailable != 20 || targetComponentID == uuid.Nil || targetLotID == uuid.Nil {
		t.Fatalf("cross-domain correction source=%d/%d target=%d/%v/%v err=%v",
			executedSourceAvailable, executedSourceReclassified, targetAvailable, targetComponentID, targetLotID, err)
	}
	var executedCorrections, claimsAfterExecution int64
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_component_corrections),
		(SELECT count(*) FROM billing_approval_proof_claims)`).Scan(&executedCorrections, &claimsAfterExecution); err != nil || executedCorrections != 1 || claimsAfterExecution != 1 {
		t.Fatalf("cross-domain effects corrections=%d claims=%d err=%v", executedCorrections, claimsAfterExecution, err)
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
	// must not create a second correction, journal or claim.
	replay := &entitlementCrossDomainDelivery{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 3}}
	consumer.Handle(ctx, replay)
	if replay.ack != 1 {
		t.Fatalf("idempotent replay was not acknowledged: ack=%d", replay.ack)
	}
	var replayCorrections, replayClaims, adjustmentJournals int64
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_component_corrections),
		(SELECT count(*) FROM billing_approval_proof_claims),
		(SELECT count(*) FROM journal_entries WHERE entry_type='ADJUSTMENT')`).Scan(&replayCorrections, &replayClaims, &adjustmentJournals); err != nil ||
		replayCorrections != 1 || replayClaims != 1 || adjustmentJournals != 1 {
		t.Fatalf("idempotent replay effects corrections=%d claims=%d journals=%d err=%v", replayCorrections, replayClaims, adjustmentJournals, err)
	}

	// Rolling the activation row back closes the command again with no history
	// rewrite: the same v1 row and the same v2 identity stay in place.
	if _, err := billingConnection.Exec(ctx, `SET ROLE billing_schema_owner;
UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2 AND state='ACTIVE'`); err != nil {
		t.Fatal(err)
	}
	if err := repository.requireApprovalAdvancement(ctx, policy); !errors.Is(err, ErrBillingGovernanceInactive) {
		t.Fatalf("rolled-back cross-domain advancement: %v", err)
	}
	rolledBack, err := activationReader.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if projected, ok := rolledBack.Find(policy.CommandType, 2, 2); !ok || projected.State != "INACTIVE" {
		t.Fatalf("Account reader did not observe the rolled-back projection: %#v", projected)
	}
	// Operator deactivation cannot invalidate an already committed receipt.
	deactivatedReplay := &entitlementCrossDomainDelivery{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 4}}
	consumer.Handle(ctx, deactivatedReplay)
	if deactivatedReplay.ack != 1 {
		t.Fatal("committed authorization receipt did not replay after deactivation")
	}
	if err := billingPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_component_corrections), (SELECT count(*) FROM billing_approval_proof_claims), (SELECT count(*) FROM journal_entries WHERE entry_type='ADJUSTMENT')`).Scan(&replayCorrections, &replayClaims, &adjustmentJournals); err != nil || replayCorrections != 1 || replayClaims != 1 || adjustmentJournals != 1 {
		t.Fatal("deactivated receipt replay changed financial facts")
	}
}
