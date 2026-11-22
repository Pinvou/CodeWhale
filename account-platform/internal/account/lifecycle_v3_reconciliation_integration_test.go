package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func lifecycleReconciliationDraft(t *testing.T, variant string) ApprovalDraft {
	t.Helper()
	draft := tokenRecentAuthDraft("TOKEN_MODEL_LIFECYCLE_CHANGE", time.Now().UTC())
	var command, critical map[string]json.RawMessage
	if err := json.Unmarshal(draft.NormalizedCommand, &command); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(draft.CriticalResourceVersions, &critical); err != nil {
		t.Fatal(err)
	}
	// Current Token plan admission requires a distinct replacement for every
	// newly created REMOVED plan, including v2. Keep v2's protocol shape (no
	// force field), while giving this SAFE positive a producible removal intent.
	replacement := mustJSON(uuid.NewString())
	command["replacement_model_id"], critical["replacement_model_id"] = replacement, replacement
	if variant != "v2" {
		force := mustJSON(variant == "v3-true")
		command["force_cancel_inflight"], critical["force_cancel_inflight"] = force, force
		critical["policy_version"] = json.RawMessage(`3`)
	}
	draft.NormalizedCommand, draft.CriticalResourceVersions = mustJSON(command), mustJSON(critical)
	return draft
}

// This offline fixture check makes the removal-plan contract explicit before
// the PostgreSQL path is attempted. It does not prove real Token ownership,
// plan persistence, replacement readiness or external no-effect evidence.
func TestLifecycleReconciliationFixtureProofContract(t *testing.T) {
	for _, variant := range []string{"v3-false", "v3-true", "v2"} {
		t.Run(variant, func(t *testing.T) {
			draft := lifecycleReconciliationDraft(t, variant)
			policy, err := approvalpolicy.ForCommand(draft.CommandType, draft.NormalizedCommand)
			want := int64(3)
			if variant == "v2" {
				want = 2
			}
			if err != nil || policy.PolicyVersion != want || policy.SnapshotSchemaVersion != int(want) {
				t.Fatal("fixture policy selection", err)
			}
			input := approvalencoding.LegacyProofInput{CommandType: draft.CommandType, PolicyVersion: want, SnapshotSchemaVersion: int(want), TargetResourceType: draft.TargetResourceType, TargetResourceID: draft.TargetResourceID, NormalizedCommand: draft.NormalizedCommand, CriticalResourceVersions: draft.CriticalResourceVersions, FinancialSnapshot: draft.FinancialSnapshot}
			artifacts, err := approvalencoding.ConvertLegacyProofInput(input)
			if err != nil {
				t.Fatal("fixture cannot produce proof input", err)
			}
			id := uuid.New()
			wire, err := approvalencoding.EncodeProofRequest(id, artifacts)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := approvalencoding.DecodeProofRequest(wire)
			if err != nil || decoded.ApprovalRequestID != id || decoded.Artifacts.PolicyVersion != want || decoded.Artifacts.CommandDigest != artifacts.CommandDigest || decoded.Artifacts.SnapshotDigest != artifacts.SnapshotDigest {
				t.Fatal("fixture transport binding", err)
			}
			if want == 3 {
				var command, critical map[string]json.RawMessage
				if err := json.Unmarshal(input.NormalizedCommand, &command); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(input.CriticalResourceVersions, &critical); err != nil {
					t.Fatal(err)
				}
				command["replacement_model_id"], critical["replacement_model_id"] = json.RawMessage(`null`), json.RawMessage(`null`)
				input.NormalizedCommand, input.CriticalResourceVersions = mustJSON(command), mustJSON(critical)
				if _, err := approvalencoding.ConvertLegacyProofInput(input); err == nil {
					t.Fatal("v3 removal without a replacement must fail closed")
				}
			}
		})
	}
}

// Account uses public migrations and genuine service/writer/operator logins.
// Token activation and snapshot are controlled projections, and proof/effect
// authority is a controlled HTTP server, not a real Token database or executor.
// The incomplete-history cases below deliberately model damaged/imported data;
// they are NOT normal production timeout flows or timeout recovery positives.
func TestLifecycleV3ApprovalReconciliationPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if raw == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	userCodeLookupTestGuard(t)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	r, operator, relay, _ := newEntitlementApprovalFixture(t, ctx, raw)
	tokenRecentAuthActors(t, ctx, r)
	requester := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "lifecycle-reconciliation-requester")
	approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "lifecycle-reconciliation-approver")
	r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{true})
	var proofCalls, effectCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if req.Method == http.MethodPost && req.URL.Path == "/internal/v1/approval-command-proofs" {
			body, err := io.ReadAll(io.LimitReader(req.Body, approvalencoding.MaximumDocumentBytes+1))
			input, decodeErr := approvalencoding.DecodeProofRequest(body)
			if err != nil || decodeErr != nil || input.Artifacts.CommandType != "TOKEN_MODEL_LIFECYCLE_CHANGE" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			now := time.Now().UTC().Truncate(time.Second)
			response, err := approvalencoding.EncodeProofResponse(approvalencoding.ProofResponse{
				ApprovalRequestID: input.ApprovalRequestID, CommandType: input.Artifacts.CommandType,
				CommandDigest: input.Artifacts.CommandDigest, SnapshotDigest: input.Artifacts.SnapshotDigest,
				PolicyVersion: input.Artifacts.PolicyVersion, ProofSchemaVersion: "1", DomainRevision: "1:" + strings.Repeat("a", 64),
				Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
			})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			proofCalls.Add(1)
			_, _ = w.Write(response)
			return
		}
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/internal/v1/approval-effects/") {
			id, err := uuid.Parse(strings.TrimPrefix(req.URL.Path, "/internal/v1/approval-effects/"))
			if err != nil || req.URL.Query().Get("command_type") != "TOKEN_MODEL_LIFECYCLE_CHANGE" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			effectCalls.Add(1)
			_ = json.NewEncoder(w).Encode(approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityToken,
				ApprovalRequestID: id, CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", CommandFingerprint: req.URL.Query().Get("command_fingerprint"),
				State: approvaleffect.StateAbsent, ObservedAt: time.Now().UTC()})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	proof, err := NewHTTPApprovalProofSourceWithToken(server.URL, server.URL, "controlled-test-credential")
	if err != nil {
		t.Fatal(err)
	}
	r.WithApprovalProofSource(proof)
	effects, err := approvaleffect.NewClientWithToken(server.URL, server.URL, "controlled-test-credential")
	if err != nil {
		t.Fatal(err)
	}
	var approved []ApprovalRequest
	for _, variant := range []string{"v3-false", "v3-true", "v2"} {
		draft := lifecycleReconciliationDraft(t, variant)
		r.WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft})
		var created ApprovalRequest
		if err := retryWhileWriterUnavailable(t, ctx, r, func() error {
			var err error
			created, err = r.CreateApprovalDraft(ctx, entitlementApprovalPrincipal(requester), draft)
			return err
		}); err != nil {
			t.Fatal("create", variant, err)
		}
		submitted, err := r.SubmitApprovalRequest(ctx, entitlementApprovalPrincipal(requester), created.ID, created.Version)
		if err != nil {
			t.Fatal("submit", variant, err)
		}
		item, err := r.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), created.ID, submitted.Version, "APPROVED", "independent lifecycle recovery approval", draft.CriticalResourceVersions)
		wantPolicy := int64(3)
		if variant == "v2" {
			wantPolicy = 2
		}
		if err != nil || item.Status != "APPROVED" || item.PolicyVersion != wantPolicy {
			t.Fatal("approve", variant, item.Status, item.PolicyVersion, err)
		}
		approved = append(approved, item)
	}
	if proofCalls.Load() != 3 {
		t.Fatal("real HTTP proof path not exercised for all variants")
	}
	t.Run("wrong_durable_fail_closed", func(t *testing.T) {
		// Binding resolution is a consumer operation: the real resolver requires
		// a committed publication receipt. Publish through the genuine relay;
		// only broker PubAck is controlled, without invoking any domain executor.
		control, err := r.ApprovalActionControl(ctx)
		if err != nil {
			t.Fatal(err)
		}
		worker := "lifecycle-reconciliation-" + uuid.NewString()
		claims, err := relay.ClaimFrozen(ctx, worker, 3, 30*time.Second)
		if err != nil || len(claims) != 3 {
			t.Fatal("claim actual frozen authorizations", len(claims), err)
		}
		published := make(map[uuid.UUID]bool)
		for _, claim := range claims {
			calls := 0
			disposition, err := relay.ProcessFrozen(ctx, worker, claim, 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
				calls++
				decoded, err := approvalencoding.DecodeAuthorizationEvent(message.Body, approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: approved[0].Version, ApprovalEpoch: control.ApprovalEpoch})
				if err != nil || decoded.EventID != message.EventID || message.EventID != claim.EventID || message.MsgID != message.EventID.String() || message.Subject != ApprovalAuthorizationEventType {
					t.Fatal("wrong frozen publication", err)
				}
				var original *ApprovalRequest
				for i := range approved {
					if approved[i].ID == decoded.AggregateID {
						original = &approved[i]
					}
				}
				if original == nil || decoded.AggregateVersion != original.Version || decoded.Payload.PolicyVersion != original.PolicyVersion || published[original.ID] {
					t.Fatal("published identity/policy differs from actual approval")
				}
				published[original.ID] = true
				return events.FrozenPublishResult{Disposition: events.FrozenPublished}
			})
			if err != nil || disposition != events.FrozenPublished || calls != 1 {
				t.Fatal("real frozen relay publication", disposition, calls, err)
			}
		}
		var event uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, approved[0].ID).Scan(&event); err != nil {
			t.Fatal(err)
		}
		before := tokenRecentAuthFacts(t, ctx, r)
		for _, durable := range []string{"token-catalog-rollback-executor-v1", "unregistered-lifecycle-executor-v3"} {
			if _, err := r.ResolveApprovalAuthorizationBinding(ctx, event, durable, approvalconsumer.TokenWorkload); err == nil {
				t.Fatal("wrong durable accepted", durable)
			}
		}
		binding, err := r.ResolveApprovalAuthorizationBinding(ctx, event, "token-model-lifecycle-executor-v1", approvalconsumer.TokenWorkload)
		if err != nil || binding.EventID != event || binding.PolicyVersion != 3 || binding.ApprovalRequestID != approved[0].ID || binding.ApprovedRequestVersion != approved[0].Version {
			t.Fatal("canonical physical lifecycle durable rejected published v3 event", err)
		}
		if tokenRecentAuthFacts(t, ctx, r) != before {
			t.Fatal("binding lookup altered frozen facts")
		}
	})
	// This is the same real control-owner transition used by existing SAFE
	// integration fixtures. No test bypasses control or immutability triggers.
	tx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET mode='SAFE_ROLLBACK',approval_wire_version=1,proof_required=true,approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run("safe_v3_false_true_and_v2_dry_apply_next_tick", func(t *testing.T) {
		plan, err := r.PlanApprovalReconciliation(ctx, 20)
		if err != nil || len(plan.Candidates) != 3 || plan.ApprovedCount != 3 {
			t.Fatalf("known v3/v2 SAFE plan: %+v %v", plan, err)
		}
		for _, item := range approved {
			var coherent bool
			if err := r.pool.QueryRow(ctx, `SELECT r.execution_deadline>clock_timestamp() AND d.decided_at=r.decided_at AND o.occurred_at<=r.execution_deadline AND (o.payload->>'execution_deadline')::timestamptz=r.execution_deadline AND o.approval_control_epoch=$2-1 FROM approval_requests r JOIN approval_decisions d ON d.approval_request_id=r.id JOIN outbox_events o ON o.aggregate_id=r.id AND o.event_type='account.approval-command.authorized.v1' WHERE r.id=$1`, item.ID, plan.Control.ApprovalEpoch).Scan(&coherent); err != nil || !coherent {
				t.Fatal("inconsistent original frozen authorization", err)
			}
		}
		before := tokenRecentAuthFacts(t, ctx, r)
		dry, err := RunApprovalReconciliation(ctx, r, effects, 20, false)
		if err != nil || dry.EligibleCount != 3 || dry.SafeCandidateCount != 3 || dry.InvalidatedCount != 0 || !dry.CompleteWithoutBlocker {
			t.Fatalf("dry: %+v %v", dry, err)
		}
		if tokenRecentAuthFacts(t, ctx, r) != before {
			t.Fatal("dry run changed facts")
		}
		applied, err := RunApprovalReconciliation(ctx, r, effects, 20, true)
		if err != nil || applied.InvalidatedCount != 3 || applied.SafeCandidateCount != 3 || !applied.CompleteWithoutBlocker {
			t.Fatalf("apply: %+v %v", applied, err)
		}
		for _, item := range approved {
			var status, code string
			var version int64
			var executions, outboxes, states int
			if err := r.pool.QueryRow(ctx, `SELECT status,invalidation_code,version,(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'),(SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND to_status='INVALIDATED' AND reason_code='APPROVAL_SAFE_ROLLBACK_INVALIDATED') FROM approval_requests WHERE id=$1`, item.ID).Scan(&status, &code, &version, &executions, &outboxes, &states); err != nil || status != "INVALIDATED" || code != "APPROVAL_SAFE_ROLLBACK_INVALIDATED" || version != item.Version+1 || executions != 0 || outboxes != 1 || states != 1 {
				t.Fatal("SAFE result did not converge exactly once", err)
			}
		}
		before = tokenRecentAuthFacts(t, ctx, r)
		calls := effectCalls.Load()
		next, err := RunApprovalReconciliation(ctx, r, effects, 20, true)
		if err != nil || next.CandidateCount != 0 || !next.CompleteWithoutBlocker || effectCalls.Load() != calls || tokenRecentAuthFacts(t, ctx, r) != before {
			t.Fatalf("next tick: %+v %v", next, err)
		}
	})
	// Copy only a request with historical timestamps two hours in the past
	// and the real five-minute execution window. Deliberately OMIT its snapshot,
	// decision and authorization/execution history (an incomplete import):
	// this tests recognition followed by fail-closed damaged-history handling.
	seedMissingHistory := func(t *testing.T, status string, policy int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := r.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,version,expires_at,execution_deadline,submitted_at,decided_at,created_at,updated_at) SELECT $1::uuid,command_type,$3::bigint,requester_user_id,requester_binding_id,requester_authorization_version,($1::uuid)::text,normalized_command,command_fingerprint,target_resource_type,target_resource_id,'controlled incomplete historical recovery','[]','{}',$2::text,3,now()+interval '22 hours',now()-interval '115 minutes',now()-interval '121 minutes',now()-interval '120 minutes',now()-interval '122 minutes',now()-interval '114 minutes' FROM approval_requests WHERE id=$4::uuid`, id, status, policy, approved[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for _, status := range []string{"APPROVED", "EXECUTION_FAILED"} {
		t.Run("expired_"+strings.ToLower(status)+"_v3_missing_history", func(t *testing.T) {
			id := seedMissingHistory(t, status, 3)
			plan, err := r.PlanApprovalReconciliation(ctx, 20)
			if err != nil {
				t.Fatal("known v3 must reach history validation", err)
			}
			var candidate ApprovalReconciliationCandidate
			for _, item := range plan.Candidates {
				if item.RequestID == id {
					candidate = item
				}
			}
			if candidate.RequestID != id || candidate.Status != status || candidate.PolicyVersion != 3 || candidate.InvalidationCode != "EXECUTION_WINDOW_EXPIRED" {
				t.Fatalf("historical candidate missing: %+v", candidate)
			}
			before := tokenRecentAuthFacts(t, ctx, r)
			observation, err := effects.Observe(ctx, id, candidate.CommandType, candidate.CommandFingerprint)
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.ReconcileApproval(ctx, plan.Control, candidate, observation, true)
			if err != nil || result != ApprovalReconciliationBlockedHistory || tokenRecentAuthFacts(t, ctx, r) != before {
				t.Fatalf("missing history must remain blocked without effects or republication: %s %v", result, err)
			}
		})
	}
	t.Run("unknown_version_fail_closed", func(t *testing.T) {
		// An appended future policy models a newer installation. It must not
		// expand this binary's closed supported recovery version set.
		if _, err := operator.Exec(ctx, `INSERT INTO approval_command_policies SELECT command_type,4,approval_mode,requester_permission,approver_permission,risk_class,approval_count,self_approval_forbidden,snapshot_schema_version,critical_resource_fields,expires_after,execution_window,executor_identity,created_at FROM approval_command_policies WHERE command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND policy_version=3`); err != nil {
			t.Fatal(err)
		}
		seedMissingHistory(t, "APPROVED", 4)
		before := tokenRecentAuthFacts(t, ctx, r)
		calls := effectCalls.Load()
		if _, err := r.PlanApprovalReconciliation(ctx, 20); !errors.Is(err, ErrApprovalReconciliationBlocked) {
			t.Fatal("unknown policy admitted", err)
		}
		if _, err := RunApprovalReconciliation(ctx, r, effects, 20, true); !errors.Is(err, ErrApprovalReconciliationBlocked) {
			t.Fatal("unknown policy runner admitted", err)
		}
		if effectCalls.Load() != calls || tokenRecentAuthFacts(t, ctx, r) != before {
			t.Fatal("unknown version caused effect query or mutation")
		}
	})
}
