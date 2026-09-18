package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/jackc/pgx/v5"
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
		token, signErr := signApprovalExecutorJWT(key, map[string]any{
			"iss": issuer.URL, "sub": subject, "aud": audience, "exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(),
		})
		if signErr != nil {
			t.Fatal(signErr)
		}
		return token
	}
	return mint(accountSubject), mint(executorSubject)
}

// transitionEntitlementCrossDomainAccountControl moves the isolated Account
// control into the proof-era wire the entitlement correction command requires.
// It is a test-only contract transition, not production wiring.
func transitionEntitlementCrossDomainAccountControl(t *testing.T, ctx context.Context, connection *pgx.Conn) {
	t.Helper()
	if _, err := connection.Exec(ctx, `SET ROLE account_approval_control_owner`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET mode='PROOF_REQUIRED',approval_wire_version=2,proof_required=true,approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
	} {
		if _, err := connection.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
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

// entitlementCrossDomainGateProbe logs every Account-side authorization call the
// billing consumer makes so a swallowed consumer error names its exact step.
type entitlementCrossDomainGateProbe struct {
	*billingdomain.HTTPApprovalExecutionGate
	t *testing.T
}

func (probe *entitlementCrossDomainGateProbe) ResolveAuthorizationRoute(ctx context.Context, eventID uuid.UUID) (billingdomain.ApprovalAuthorizationRoute, error) {
	value, err := probe.HTTPApprovalExecutionGate.ResolveAuthorizationRoute(ctx, eventID)
	probe.t.Logf("gate route err=%v", err)
	return value, err
}

func (probe *entitlementCrossDomainGateProbe) ResolveAuthorizationBinding(ctx context.Context, eventID uuid.UUID, durable string) (billingdomain.ApprovalAuthorizationBinding, error) {
	value, err := probe.HTTPApprovalExecutionGate.ResolveAuthorizationBinding(ctx, eventID, durable)
	probe.t.Logf("gate binding err=%v", err)
	return value, err
}

func (probe *entitlementCrossDomainGateProbe) ObserveAuthorizationDelivery(ctx context.Context, binding billingdomain.ApprovalAuthorizationBinding, stream string, sequence approvalconsumer.Sequence) (billingdomain.ApprovalAuthorizationDeliveryState, error) {
	value, err := probe.HTTPApprovalExecutionGate.ObserveAuthorizationDelivery(ctx, binding, stream, sequence)
	probe.t.Logf("gate observe state=%s err=%v", value.State, err)
	return value, err
}

func (probe *entitlementCrossDomainGateProbe) GrantAuthorizationAttempt(ctx context.Context, binding billingdomain.ApprovalAuthorizationBinding, version int64) (billingdomain.ApprovalAuthorizationDeliveryState, error) {
	value, err := probe.HTTPApprovalExecutionGate.GrantAuthorizationAttempt(ctx, binding, version)
	probe.t.Logf("gate grant state=%s attempts=%d err=%v", value.State, value.DomainAttemptCount, err)
	return value, err
}

func (probe *entitlementCrossDomainGateProbe) BeginAuthorized(ctx context.Context, binding billingdomain.ApprovalAuthorizationBinding, jobRunID string, observedVersions, observedFinancialSnapshot json.RawMessage) (billingdomain.ApprovedCommand, error) {
	value, err := probe.HTTPApprovalExecutionGate.BeginAuthorized(ctx, binding, jobRunID, observedVersions, observedFinancialSnapshot)
	probe.t.Logf("gate begin command=%s err=%v", value.CommandType, err)
	return value, err
}

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

// logEntitlementCrossDomainRouteChecks replicates the consumer's early route and
// wire validation with exported helpers so a rejection names its exact field.
func logEntitlementCrossDomainRouteChecks(t *testing.T, route billingdomain.ApprovalAuthorizationRoute, raw []byte) {
	t.Helper()
	descriptor, ok := approvalconsumer.LookupDurable(route.CanonicalDurable)
	t.Logf("route check lookupDurable=%v descriptor=%+v", ok, descriptor)
	if ok {
		t.Logf("route check matchRoute=%v", approvalconsumer.MatchRoute(descriptor, route.CommandType, route.PolicyVersion, route.ExecutorIdentity))
	}
	decoded, err := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
	})
	if err != nil {
		t.Logf("route check decode err=%v", err)
		return
	}
	digest := sha256.Sum256(raw)
	t.Logf("route check eventID=%v aggregateID=%v aggregateVersion=%v serialization=%v command=%v policy=%v executor=%v target=%s/%s digest=%v",
		decoded.EventID == route.EventID, decoded.AggregateID == route.ApprovalRequestID, decoded.AggregateVersion == route.ApprovedRequestVersion,
		decoded.SerializationVersion == route.SerializationVersion, decoded.Payload.CommandType == route.CommandType,
		decoded.Payload.PolicyVersion == route.PolicyVersion, decoded.Payload.ExecutorIdentity == route.ExecutorIdentity,
		decoded.Payload.TargetResourceType, decoded.Payload.TargetResourceID, hex.EncodeToString(digest[:]) == route.EventDigest)
}

func TestEntitlementCorrectionCrossDomainActivationPostgresIntegration(t *testing.T) {
	accountURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	billingURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if accountURL == "" || billingURL == "" {
		t.Skip("real Account and Billing PostgreSQL URLs are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	repository, accountConnection, relay, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, accountURL, 0)
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
	activation := activations.Activations[1]
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

	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-activation-requester")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-activation-approver")
	requesterPrincipal, approverPrincipal := approvalIntegrationPrincipal(requester), approvalIntegrationPrincipal(approver)
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
	transitionEntitlementCrossDomainAccountControl(t, ctx, accountConnection)
	requester = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-chain-requester")
	approver = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "entitlement-chain-approver")
	approverPrincipal = approvalIntegrationPrincipal(approver)
	requestID := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "DRAFT")
	// The proof-era request must be seeded with the same closed typed form the
	// Account approval writer freezes onto the wire: integers are canonical
	// decimal strings, not JSON numbers.
	if _, err := approvalencoding.LegacyProofInputFromArtifacts(artifacts); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE approval_requests SET normalized_command=$2,command_fingerprint=decode($3,'hex'),
target_resource_type=$4,target_resource_id=$5 WHERE id=$1 AND status='DRAFT'`,
		requestID, frozen.NormalizedCommand, artifacts.CommandDigest, frozen.TargetResourceType, frozen.TargetResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE approval_request_snapshots SET critical_resource_versions=$2,financial_snapshot=$3,snapshot_schema_version=2 WHERE approval_request_id=$1`,
		requestID, frozen.CriticalResourceVersions, frozen.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE approval_requests SET status='SUBMITTED',submitted_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND status='DRAFT'`, requestID); err != nil {
		t.Fatal(err)
	}
	approved, err := repository.DecideApprovalRequest(ctx, approverPrincipal, requestID, 2, "APPROVED", "cross-domain entitlement correction", frozen.CriticalResourceVersions)
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
	logEntitlementCrossDomainRouteChecks(t, route, raw)
	decoded, err := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: route.ApprovedRequestVersion, ApprovalEpoch: route.ApprovalEpoch,
	})
	if err != nil {
		t.Fatalf("authorization wire decode route=%+v err=%v", route, err)
	}
	t.Logf("event critical=%s", string(decoded.Payload.CriticalResourceVersions))
	t.Logf("event financial=%s", string(decoded.Payload.FinancialSnapshot))
	{
		var criticalSame, financialSame bool
		if probeErr := repository.pool.QueryRow(ctx, `SELECT critical_resource_versions = $2::jsonb, financial_snapshot = $3::jsonb
		FROM approval_request_snapshots WHERE approval_request_id=$1`,
			requestID, string(decoded.Payload.CriticalResourceVersions), string(decoded.Payload.FinancialSnapshot)).Scan(&criticalSame, &financialSame); probeErr != nil {
			t.Logf("snapshot equality probe failed: %v", probeErr)
		}
		t.Logf("snapshot equality critical=%v financial=%v", criticalSame, financialSame)
	}
	observed, err := gate.ObserveAuthorizationDelivery(ctx, binding, "ACCOUNT_PLATFORM", 1)
	if err != nil {
		t.Fatalf("authorization delivery observation=%+v err=%v", observed, err)
	}
	t.Logf("authorization delivery state=%+v binding=%+v route=%+v", observed, binding, route)
	// Converge the approval writer before the begin contract is exercised: the
	// isolated fixture is known to need a short readiness window.
	for attempt := 0; attempt < 30; attempt++ {
		if err := repository.ApprovalWriterReady(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := repository.ApprovalWriterReady(ctx); err != nil {
		t.Fatalf("approval writer never became ready: %v", err)
	}
	{
		var status, deadline, durable, serialization, provenance, digestKind, eventDigest string
		var policyVersion, approvedVersion, epoch, snapshotSchema int64
		if probeErr := repository.pool.QueryRow(ctx, `SELECT r.status,COALESCE(r.execution_deadline::text,'NULL'),COALESCE(r.authorization_durable,''),
			COALESCE(r.authorization_serialization_version,''),COALESCE(r.authorization_provenance,''),COALESCE(r.authorization_digest_kind,''),
			COALESCE(r.authorization_event_digest,''),r.policy_version,COALESCE(r.authorization_approved_request_version,0),
			COALESCE(r.authorization_approval_epoch,0),s.snapshot_schema_version
		FROM approval_requests r JOIN approval_request_snapshots s ON s.approval_request_id=r.id WHERE r.id=$1`, requestID).
			Scan(&status, &deadline, &durable, &serialization, &provenance, &digestKind, &eventDigest, &policyVersion, &approvedVersion, &epoch, &snapshotSchema); probeErr != nil {
			t.Logf("request probe failed: %v", probeErr)
		}
		t.Logf("request probe status=%s deadline=%s durable=%s serialization=%s provenance=%s digestKind=%s digest=%s policyVersion=%d approvedVersion=%d epoch=%d snapshotSchema=%d registryExecutor=%s",
			status, deadline, durable, serialization, provenance, digestKind, eventDigest, policyVersion, approvedVersion, epoch, snapshotSchema, policy.ExecutorIdentity)
	}
	consumer, err := billingdomain.NewAuthorizationCommandConsumer(billingdomain.NewService(billingRepository), &entitlementCrossDomainGateProbe{HTTPApprovalExecutionGate: gate, t: t}, descriptor.Durable)
	if err != nil {
		t.Fatal(err)
	}
	deliveries := []*entitlementCrossDomainDelivery{
		{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 1}},
		{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 2}},
	}
	acked := 0
	for index, delivery := range deliveries {
		if acked > 0 {
			break
		}
		consumer.Handle(ctx, delivery)
		acked = delivery.ack
		var probeState string
		var probeAttempts, probeVersion int64
		if _, roleErr := accountConnection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); roleErr == nil {
			_ = accountConnection.QueryRow(ctx, `SELECT state,domain_attempt_count,version FROM account_approval_authorization.approval_delivery_states
WHERE original_event_id=$1 AND durable=$2`, eventID, descriptor.Durable).Scan(&probeState, &probeAttempts, &probeVersion)
			_, _ = accountConnection.Exec(ctx, `RESET ROLE; SET ROLE account_service`)
		}
		var probeClaims int64
		_ = billingPool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims`).Scan(&probeClaims)
		t.Logf("after handle %d: ack=%d state=%s attempts=%d version=%d claims=%d", index, delivery.ack, probeState, probeAttempts, probeVersion, probeClaims)
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
		var invalidationStatus, invalidationCode string
		_ = repository.pool.QueryRow(ctx, `SELECT status,COALESCE(invalidation_code,''),COALESCE(invalidation_details::text,'') FROM approval_requests WHERE id=$1`, requestID).
			Scan(&invalidationStatus, &invalidationCode, new(string))
		t.Logf("request after failure: status=%s invalidation=%s", invalidationStatus, invalidationCode)
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
}
