package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type authorizationProofVector struct {
	Name                  string `json:"name"`
	CommandType           string `json:"command_type"`
	PolicyVersion         int64  `json:"policy_version"`
	SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
	CommandDigest         string `json:"command_digest"`
	SnapshotDigest        string `json:"snapshot_digest"`
	ProofJSON             string `json:"proof_json"`
	Legacy                struct {
		TargetType string `json:"target_resource_type"`
		TargetID   string `json:"target_resource_id"`
		Normalized string `json:"normalized_json"`
		Critical   string `json:"critical_json"`
		Financial  string `json:"financial_json"`
	} `json:"legacy"`
}

func TestApprovalAuthorizationProofSixCommandsIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	// This is an isolated test-only control transition, not production wiring.
	for _, statement := range []string{
		`UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET mode='PROOF_REQUIRED',approval_wire_version=2,proof_required=true,approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`,
	} {
		tx := authorizationControlOwnerTransaction(t, ctx, db.migratorPool)
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []authorizationProofVector `json:"proof_vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || len(fixture.Vectors) != 6 {
		t.Fatalf("expected six independent GSA05 vectors: %v", err)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.CommandType, func(t *testing.T) {
			request := seedAuthorizationProofRequest(t, ctx, db, vector)
			artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
				CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion, SnapshotSchemaVersion: vector.SnapshotSchemaVersion,
				TargetResourceType: vector.Legacy.TargetType, TargetResourceID: vector.Legacy.TargetID,
				NormalizedCommand: json.RawMessage(vector.Legacy.Normalized), CriticalResourceVersions: json.RawMessage(vector.Legacy.Critical), FinancialSnapshot: json.RawMessage(vector.Legacy.Financial),
			})
			if err != nil || artifacts.CommandDigest != vector.CommandDigest || artifacts.SnapshotDigest != vector.SnapshotDigest || string(artifacts.ProofInputJSON) != vector.ProofJSON {
				t.Fatalf("independent golden conversion changed: %v", err)
			}
			issuedAt := time.Now().UTC()
			proof := ApprovalAuthorizationProof{Artifacts: artifacts, Response: approvalencoding.ProofResponse{
				ApprovalRequestID: request.ID, CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion,
				CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
				ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "test-domain-revision-1",
				Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Minute),
			}}
			proofDigest, err := approvalencoding.DeriveProofDigest(proof.Response)
			if err != nil {
				t.Fatal(err)
			}
			// Reject mismatched caller artifacts/control and roll the entire request
			// transition back before the valid commit below.
			for _, fault := range []string{"command_digest", "snapshot_digest", "response_request", "response_command", "response_digest", "deny", "epoch", "control_version", "rollback"} {
				tx, err := db.writerPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
				if err != nil {
					t.Fatal(err)
				}
				// Also release the connection if a helper aborts this subtest.
				defer tx.Rollback(context.Background())
				control, err := lockApprovalAuthorizationControl(ctx, tx)
				if err != nil {
					_ = tx.Rollback(ctx)
					t.Fatal(err)
				}
				_ = authorizationPrepareLegacyApproval(t, ctx, tx, request)
				badProof := proof
				switch fault {
				case "command_digest":
					badProof.Artifacts.CommandDigest = strings.Repeat("b", 64)
				case "snapshot_digest":
					badProof.Artifacts.SnapshotDigest = strings.Repeat("c", 64)
				case "response_request":
					badProof.Response.ApprovalRequestID = uuid.New()
				case "response_command":
					badProof.Response.CommandType = "UNKNOWN_COMMAND"
				case "response_digest":
					badProof.Response.CommandDigest = strings.Repeat("d", 64)
				case "deny":
					badProof.Response.Verdict = "DENY"
					badProof.Response.ReasonCode = "RESOURCE_STATE_CHANGED"
					if _, err := approvalencoding.DeriveProofDigest(badProof.Response); err != nil {
						t.Fatalf("DENY fixture must be transport-valid: %v", err)
					}
				case "epoch":
					control.ApprovalEpoch++
				case "control_version":
					control.Version++
				}
				_, appendErr := appendProofApprovalAuthorization(ctx, tx, control, request.ID, uuid.New(), badProof)
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if (fault == "rollback") != (appendErr == nil) {
					t.Fatalf("%s proof boundary: %v", fault, appendErr)
				}
				var count int64
				if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.outbox_events WHERE aggregate_id=$1`, request.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s leaked authorization event: %d %v", fault, count, err)
				}
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
			approved := authorizationPrepareLegacyApproval(t, ctx, tx, request)
			eventID := uuid.New()
			record, err := appendProofApprovalAuthorization(ctx, tx, control, request.ID, eventID, proof)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var version, epoch int64
			var raw, outboxPayload []byte
			var digest, serialization, provenance string
			if err := db.adminPool.QueryRow(ctx, `SELECT o.aggregate_version,o.approval_control_epoch,o.payload,s.delivery_bytes,s.event_digest,s.serialization_version,s.provenance FROM public.outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id WHERE o.event_id=$1`, eventID).Scan(&version, &epoch, &outboxPayload, &raw, &digest, &serialization, &provenance); err != nil {
				t.Fatal(err)
			}
			decoded, err := approvalencoding.DecodeAuthorizationEvent(raw, approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: approved.Version, ApprovalEpoch: control.ApprovalEpoch})
			if err != nil {
				t.Fatal(err)
			}
			if version != approved.Version || epoch != control.ApprovalEpoch || decoded.EventID != eventID || decoded.AggregateID != request.ID || decoded.Payload.ApprovalRequestID != request.ID || decoded.Payload.CommandDigest != vector.CommandDigest || decoded.Payload.SnapshotDigest != vector.SnapshotDigest || serialization != approvalencoding.ApprovalSerializationVersion || provenance != "FROZEN_AT_APPROVAL" || !bytes.Equal(raw, record.DeliveryBytes) {
				t.Fatal("request, control, Outbox, and frozen wire bindings diverged")
			}
			if decoded.Payload.ProofDigest != hex.EncodeToString(proofDigest[:]) || decoded.Payload.DomainRevision != proof.Response.DomainRevision {
				t.Fatal("writer did not derive the proof digest from the bound typed response")
			}
			actualDigest := sha256.Sum256(raw)
			if digest != hex.EncodeToString(actualDigest[:]) || digest != record.EventDigest {
				t.Fatal("frozen bytes digest mismatch")
			}
			var envelope struct {
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil || !sameJSONObject(envelope.Payload, outboxPayload) {
				t.Fatal("Outbox payload differs from canonical wire")
			}
			// The int64 legacy storage must remain numeric and unchanged.
			var storedCommand json.RawMessage
			if err := db.adminPool.QueryRow(ctx, `SELECT normalized_command FROM public.approval_requests WHERE id=$1`, request.ID).Scan(&storedCommand); err != nil || !sameJSONObject(storedCommand, json.RawMessage(vector.Legacy.Normalized)) {
				t.Fatal("proof writer modified legacy request storage")
			}
		})
	}
}

func seedAuthorizationProofRequest(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, vector authorizationProofVector) ApprovalRequest {
	t.Helper()
	policy, err := approvalpolicy.Lookup(vector.CommandType)
	if err != nil || policy.PolicyVersion != vector.PolicyVersion {
		t.Fatalf("golden policy drift: %v", err)
	}
	actor := seedTokenOwnerCandidateActor(t, ctx, db.adminPool, "PLATFORM_SUPER_ADMIN", "proof-"+uuid.NewString())
	repository := &PostgresRepository{pool: db.adminPool, now: func() time.Time { return time.Now().UTC() }}
	id := seedAdvancementRequest(t, ctx, repository, actor.ID, policy, "DRAFT")
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET normalized_command=$2,target_resource_type=$3,target_resource_id=$4 WHERE id=$1`, id, json.RawMessage(vector.Legacy.Normalized), vector.Legacy.TargetType, vector.Legacy.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_request_snapshots SET critical_resource_versions=$2,financial_snapshot=$3,snapshot_schema_version=$4 WHERE approval_request_id=$1`, id, json.RawMessage(vector.Legacy.Critical), json.RawMessage(vector.Legacy.Financial), vector.SnapshotSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.approval_requests SET status='SUBMITTED',submitted_at=now(),version=version+1,updated_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	request, err := approvalRequestByID(ctx, db.adminPool, id)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
