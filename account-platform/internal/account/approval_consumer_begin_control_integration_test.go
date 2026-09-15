package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestApprovalConsumerBeginControlActionMatrixIntegration(t *testing.T) {
	db, _, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cases := []struct {
		name, mode, relay, serialization, provenance, digestKind string
		wire                                                     int16
		proof, existing                                          bool
		currentEpoch, eventEpoch                                 int64
		want                                                     bool
	}{
		{"legacy running first", "LEGACY", "RUNNING", "LEGACY_TYPED_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, false, false, 1, 1, true},
		{"legacy fenced first", "LEGACY", "FENCED", "LEGACY_TYPED_V1", "RECONSTRUCTED_AFTER_PUBLISH", "LEGACY_SEMANTIC", 1, false, false, 1, 1, true},
		{"quiescing running first", "QUIESCING", "RUNNING", "LEGACY_TYPED_V1", "RECONSTRUCTED_FOR_DELIVERY", "DELIVERY_BYTES", 1, false, false, 2, 1, true},
		{"quiescing fenced first", "QUIESCING", "FENCED", "LEGACY_TYPED_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, false, false, 2, 1, false},
		{"quiescing fenced existing", "QUIESCING", "FENCED", "LEGACY_TYPED_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, false, true, 2, 1, true},
		{"proof running first", "PROOF_REQUIRED", "RUNNING", "APPROVAL_RFC8785_DECIMAL_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 2, true, false, 3, 3, true},
		{"proof fenced first", "PROOF_REQUIRED", "FENCED", "APPROVAL_RFC8785_DECIMAL_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 2, true, false, 3, 3, true},
		{"safe running first", "SAFE_ROLLBACK", "RUNNING", "APPROVAL_RFC8785_DECIMAL_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, true, false, 4, 3, false},
		{"safe running existing", "SAFE_ROLLBACK", "RUNNING", "APPROVAL_RFC8785_DECIMAL_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, true, true, 4, 3, false},
		{"safe fenced existing", "SAFE_ROLLBACK", "FENCED", "APPROVAL_RFC8785_DECIMAL_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, true, true, 4, 3, false},
		{"legacy old epoch after roundtrip", "LEGACY", "RUNNING", "LEGACY_TYPED_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 1, false, true, 3, 1, false},
		{"proof old epoch", "PROOF_REQUIRED", "RUNNING", "APPROVAL_RFC8785_DECIMAL_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 2, true, true, 4, 3, false},
		{"proof reconstructed provenance", "PROOF_REQUIRED", "RUNNING", "APPROVAL_RFC8785_DECIMAL_V1", "RECONSTRUCTED_FOR_DELIVERY", "DELIVERY_BYTES", 2, true, true, 3, 3, false},
		{"proof legacy serialization", "PROOF_REQUIRED", "RUNNING", "LEGACY_TYPED_V1", "FROZEN_AT_APPROVAL", "DELIVERY_BYTES", 2, true, true, 3, 3, false},
	}
	for _, test := range cases {
		var got bool
		if err := db.adminPool.QueryRow(ctx, `SELECT account_approval_authorization.authorization_begin_action_allows(
$1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, test.mode, test.relay, test.wire, test.proof, test.currentEpoch,
			test.eventEpoch, test.serialization, test.provenance, test.digestKind, test.existing).Scan(&got); err != nil || got != test.want {
			t.Fatalf("%s got=%t want=%t err=%v", test.name, got, test.want, err)
		}
	}
}

func TestApprovalConsumerBeginQuiescingFencedExistingOnlyIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	state := observeApprovalDelivery(t, ctx, repository, binding, approvalconsumer.Sequence(61))
	state, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version)
	if err != nil {
		t.Fatal(err)
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, binding.ApprovalRequestID)
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "q-first", critical, financial, binding); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("QUIESCING/FENCED first execution err=%v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 0)
	setApprovalConsumerRelay(t, ctx, db, "RUNNING")
	execution, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "q-existing", critical, financial, binding)
	if err != nil || !execution.FirstClaim {
		t.Fatalf("QUIESCING/RUNNING first execution=%+v err=%v", execution, err)
	}
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	repeated, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "q-existing-retry", critical, financial, binding)
	if err != nil || repeated.FirstClaim || repeated.ExecutionIdempotencyKey != execution.ExecutionIdempotencyKey {
		t.Fatalf("QUIESCING/FENCED existing execution=%+v err=%v", repeated, err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 1)
	_ = state
}

func TestApprovalConsumerLegacyBeginControlIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	critical, financial := approvalExecutionSnapshots(t, ctx, db, binding.ApprovalRequestID)
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	if _, err := repository.BeginApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, "legacy-q-first", critical, financial); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("QUIESCING/FENCED legacy first execution err=%v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 0)

	setApprovalConsumerRelay(t, ctx, db, "RUNNING")
	execution, err := repository.BeginApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, "legacy-q-existing", critical, financial)
	if err != nil || !execution.FirstClaim {
		t.Fatalf("QUIESCING/RUNNING legacy first execution=%+v err=%v", execution, err)
	}
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	repeated, err := repository.BeginApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, "legacy-q-existing-retry", critical, financial)
	if err != nil || repeated.FirstClaim || repeated.ExecutionIdempotencyKey != execution.ExecutionIdempotencyKey {
		t.Fatalf("QUIESCING/FENCED legacy existing execution=%+v err=%v", repeated, err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 1)

	setApprovalConsumerControl(t, ctx, db, "SAFE_ROLLBACK", "FENCED", 1, true, 3)
	if _, err := repository.BeginApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, "legacy-safe-existing", critical, financial); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("SAFE_ROLLBACK admitted legacy existing execution err=%v", err)
	}
	setApprovalConsumerControl(t, ctx, db, "PROOF_REQUIRED", "FENCED", 2, true, 4)
	if _, err := repository.BeginApprovalExecution(ctx, binding.ApprovalRequestID, binding.Workload, "legacy-proof-existing", critical, financial); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("PROOF_REQUIRED admitted legacy existing execution err=%v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 1)
}

func TestApprovalConsumerBeginOldEpochCannotReviveIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	state := observeApprovalDelivery(t, ctx, repository, binding, approvalconsumer.Sequence(62))
	if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version); err != nil {
		t.Fatal(err)
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, binding.ApprovalRequestID)
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	setApprovalConsumerControl(t, ctx, db, "LEGACY", "RUNNING", 1, false, 3)
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, binding.ApprovalRequestID, binding.Workload, "revived", critical, financial, binding); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("old approval epoch after LEGACY round trip err=%v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, binding.ApprovalRequestID, 0)
}

func TestApprovalConsumerBeginProofAndSafeIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	setApprovalConsumerControl(t, ctx, db, "QUIESCING", "RUNNING", 1, false, 2)
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	setApprovalConsumerControl(t, ctx, db, "PROOF_REQUIRED", "FENCED", 2, true, 3)
	setApprovalConsumerRelay(t, ctx, db, "RUNNING")
	first := publishedProofApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	second := publishedProofApprovalDeliveryBinding(t, ctx, db, repository, relay, "billing-wallet-adjustment-executor-p1-v1")
	for index, binding := range []ApprovalAuthorizationBinding{first, second} {
		state := observeApprovalDelivery(t, ctx, repository, binding, approvalconsumer.Sequence(70+index))
		if _, err := repository.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version); err != nil {
			t.Fatal(err)
		}
	}
	critical, financial := approvalExecutionSnapshots(t, ctx, db, first.ApprovalRequestID)
	execution, err := repository.BeginApprovalExecutionWithAuthorization(ctx, first.ApprovalRequestID, first.Workload, "proof-running", critical, financial, first)
	if err != nil || !execution.FirstClaim {
		t.Fatalf("PROOF_REQUIRED/RUNNING execution=%+v err=%v", execution, err)
	}
	setApprovalConsumerRelay(t, ctx, db, "FENCED")
	critical, financial = approvalExecutionSnapshots(t, ctx, db, second.ApprovalRequestID)
	fenced, err := repository.BeginApprovalExecutionWithAuthorization(ctx, second.ApprovalRequestID, second.Workload, "proof-fenced", critical, financial, second)
	if err != nil || !fenced.FirstClaim {
		t.Fatalf("PROOF_REQUIRED/FENCED first execution=%+v err=%v", fenced, err)
	}
	setApprovalConsumerControl(t, ctx, db, "SAFE_ROLLBACK", "FENCED", 1, true, 4)
	critical, financial = approvalExecutionSnapshots(t, ctx, db, first.ApprovalRequestID)
	if _, err := repository.BeginApprovalExecutionWithAuthorization(ctx, first.ApprovalRequestID, first.Workload, "safe-existing", critical, financial, first); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("SAFE_ROLLBACK existing execution err=%v", err)
	}
	assertApprovalExecutionCount(t, ctx, db, first.ApprovalRequestID, 1)
}

func publishedProofApprovalDeliveryBinding(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, repository *PostgresRepository, relay *ApprovalRelayStore, durable string) ApprovalAuthorizationBinding {
	t.Helper()
	raw, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []authorizationProofVector `json:"proof_vectors"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Vectors) == 0 {
		t.Fatalf("proof fixture: %v", err)
	}
	vector := fixture.Vectors[0]
	request := seedAuthorizationProofRequest(t, ctx, db, vector)
	policy, err := approvalPolicyForItem(request)
	if err != nil {
		t.Fatal(err)
	}
	approver := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "consumer-proof-approver-"+uuid.NewString())
	approverBinding, approverVersion, _, err := strongPlatformAuthorization(ctx, db.adminPool, approver.ID, policy.ApproverPermission)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion, SnapshotSchemaVersion: vector.SnapshotSchemaVersion,
		TargetResourceType: vector.Legacy.TargetType, TargetResourceID: vector.Legacy.TargetID,
		NormalizedCommand: json.RawMessage(vector.Legacy.Normalized), CriticalResourceVersions: json.RawMessage(vector.Legacy.Critical), FinancialSnapshot: json.RawMessage(vector.Legacy.Financial),
	})
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Now().UTC()
	proof := ApprovalAuthorizationProof{Artifacts: artifacts, Response: approvalencoding.ProofResponse{
		ApprovalRequestID: request.ID, CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion,
		CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
		ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "consumer-begin-proof-revision",
		Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Minute),
	}}
	proofDigest, err := approvalencoding.DeriveProofDigest(proof.Response)
	if err != nil || len(hex.EncodeToString(proofDigest[:])) != 64 {
		t.Fatalf("proof digest: %v", err)
	}
	tx, err := db.writerPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	control, err := lockApprovalAuthorizationControl(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.approval_decisions
(id,approval_request_id,approver_user_id,approver_binding_id,approver_authorization_version,decision,reason,authorization_snapshot,decided_at)
VALUES($1,$2,$3,$4,$5,'APPROVED','proof consumer gate fixture','{}',transaction_timestamp())`,
		uuid.New(), request.ID, approver.ID, approverBinding, approverVersion); err != nil {
		t.Fatal(err)
	}
	_ = authorizationPrepareLegacyApproval(t, ctx, tx, request)
	if err := appendApprovalStateEvent(ctx, tx, request.ID, "SUBMITTED", "APPROVED", "USER", approver.ID.String(), "INDEPENDENT_DECISION", map[string]any{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := appendProofApprovalAuthorization(ctx, tx, control, request.ID, eventID, proof); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	claims, err := relay.ClaimFrozen(ctx, "proof-consumer-fixture", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID {
		t.Fatalf("proof claims=%+v err=%v", claims, err)
	}
	if disposition, err := relay.ProcessFrozen(ctx, "proof-consumer-fixture", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != nil || disposition != events.FrozenPublished {
		t.Fatalf("proof publish disposition=%s err=%v", disposition, err)
	}
	binding, err := repository.ResolveApprovalAuthorizationBinding(ctx, eventID, durable, "billing-command-executor")
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func approvalExecutionSnapshots(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, requestID uuid.UUID) (json.RawMessage, json.RawMessage) {
	t.Helper()
	var critical, financial json.RawMessage
	if err := db.adminPool.QueryRow(ctx, `SELECT critical_resource_versions,financial_snapshot
FROM public.approval_request_snapshots WHERE approval_request_id=$1`, requestID).Scan(&critical, &financial); err != nil {
		t.Fatal(err)
	}
	return critical, financial
}

func setApprovalConsumerControl(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, mode, relay string, wire int16, proof bool, epoch int64) {
	t.Helper()
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE account_approval_control.admin_approval_control DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET mode=$1,authorization_relay_state=$2,approval_wire_version=$3,proof_required=$4,approval_epoch=$5,
    version=version+1,updated_at=transaction_timestamp() WHERE control_id='ADMIN_APPROVAL'`, mode, relay, wire, proof, epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE account_approval_control.admin_approval_control ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func setApprovalConsumerRelay(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, relay string) {
	t.Helper()
	var mode string
	var wire int16
	var proof bool
	var epoch int64
	if err := db.adminPool.QueryRow(ctx, `SELECT mode,approval_wire_version,proof_required,approval_epoch
FROM account_approval_control.admin_approval_control WHERE control_id='ADMIN_APPROVAL'`).Scan(&mode, &wire, &proof, &epoch); err != nil {
		t.Fatal(err)
	}
	setApprovalConsumerControl(t, ctx, db, mode, relay, wire, proof, epoch)
}

func assertApprovalExecutionCount(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, requestID uuid.UUID, want int64) {
	t.Helper()
	var got int64
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.approval_executions WHERE approval_request_id=$1`, requestID).Scan(&got); err != nil || got != want {
		t.Fatalf("execution count=%d want=%d err=%v", got, want, err)
	}
}
