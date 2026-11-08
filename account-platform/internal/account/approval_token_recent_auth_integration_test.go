package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
)

// Only the external Token observations are controlled. All authorization,
// visibility, receipt checks and writes run through the real Account repository
// installed by public migrations, with its genuine service and writer logins.
type tokenRecentAuthActivation struct{ active bool }

func (a *tokenRecentAuthActivation) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	state := "INACTIVE"
	if a.active {
		state = "ACTIVE"
	}
	return tokengovernance.Snapshot{State: state, Epoch: 1}, nil
}

type tokenRecentAuthSnapshot struct {
	draft ApprovalDraft
	calls int
	after func()
}

func (s *tokenRecentAuthSnapshot) PrecheckApprovalSnapshot(_ context.Context, input ApprovalSnapshotRequest) (ApprovalSnapshotProjection, error) {
	s.calls++
	if s.after != nil {
		s.after()
	}
	return ApprovalSnapshotProjection{CommandType: input.CommandType, TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID,
		CriticalResourceVersions: s.draft.CriticalResourceVersions, FinancialSnapshot: s.draft.FinancialSnapshot}, nil
}

type tokenRecentAuthProof struct {
	calls int
	after func()
}

func (s *tokenRecentAuthProof) Proof(_ context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
	s.calls++
	now := time.Now().UTC().Truncate(time.Second)
	response := approvalencoding.ProofResponse{ApprovalRequestID: id, CommandType: artifacts.CommandType, CommandDigest: artifacts.CommandDigest,
		SnapshotDigest: artifacts.SnapshotDigest, PolicyVersion: artifacts.PolicyVersion, ProofSchemaVersion: "1", DomainRevision: "controlled-token-revision",
		Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	digest, err := approvalencoding.DeriveProofDigest(response)
	if s.after != nil {
		s.after()
	}
	return ApprovalProofResult{Response: response, Digest: hex.EncodeToString(digest[:])}, err
}

func tokenRecentAuthDraft(command string, now time.Time) ApprovalDraft {
	id, accountID, plan := uuid.New(), uuid.New(), uuid.New()
	f := strings.Repeat("a", 64)
	draft := ApprovalDraft{CommandType: command, IdempotencyKey: uuid.NewString(), TargetResourceID: id.String(), Reason: "controlled recent authentication approval",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`)}
	switch command {
	case "TOKEN_CREDENTIAL_REVOKE":
		draft.TargetResourceType = "PROVIDER_CREDENTIAL"
		draft.NormalizedCommand = mustJSON(map[string]any{"token_account_id": accountID, "expected_account_version": 7, "credential_id": id, "credential_version": 3})
		draft.CriticalResourceVersions = mustJSON(map[string]any{"token_account_id": accountID, "token_account_version": 7, "credential_id": id, "credential_version": 3, "credential_status": "ACTIVE", "credential_fingerprint": f,
			"active_route_set_fingerprint": f, "provisioning_lease_set_fingerprint": f, "inflight_request_set_fingerprint": f, "replacement_credential_version": nil, "policy_version": 1})
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		draft.TargetResourceType, draft.TargetResourceID = "MODEL_CATALOG", "1"
		draft.NormalizedCommand = json.RawMessage(`{"source_catalog_version":1}`)
		draft.CriticalResourceVersions = mustJSON(map[string]any{"current_catalog_version": 2, "source_catalog_version": 1, "catalog_diff_fingerprint": f, "billing_price_version": 1,
			"route_set_fingerprint": f, "provisioning_set_fingerprint": f, "inflight_request_set_fingerprint": f, "policy_version": 2})
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		at := now.Add(time.Hour).Format(time.RFC3339Nano)
		draft.TargetResourceType = "MODEL"
		draft.NormalizedCommand = mustJSON(map[string]any{"model_id": id, "lifecycle_plan_id": plan, "lifecycle_plan_version": 1, "target_lifecycle": "REMOVED", "effective_at": at, "existing_session_cutoff_at": at, "replacement_model_id": nil})
		draft.CriticalResourceVersions = mustJSON(map[string]any{"model_id": id, "catalog_version": 1, "current_lifecycle": "PUBLISHED", "target_lifecycle": "REMOVED", "lifecycle_plan_version": 1, "effective_at": at,
			"existing_session_cutoff_at": at, "replacement_model_id": nil, "billing_price_version": 1, "route_set_fingerprint": f, "provisioning_set_fingerprint": f, "inflight_request_set_fingerprint": f, "policy_version": 2})
	}
	if command != "TOKEN_CREDENTIAL_REVOKE" {
		draft.FinancialSnapshot = mustJSON(map[string]any{"billing_price_revision": 1, "price_set_fingerprint": f, "models_checked": 1, "models_covered": 1, "fully_covered": true, "billing_as_of": now.Format(time.RFC3339Nano)})
	}
	return draft
}

// Compare the whole persisted approval fact set, including frozen snapshots,
// state events, decisions, execution and outbox. Runtime SELECT privileges do
// not expose the private authorization bytes; no authorization event can be
// committed without its corresponding public outbox fact.
func tokenRecentAuthFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var facts string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
	 'requests',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]') FROM approval_requests x),
	 'snapshots',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.approval_request_id),'[]') FROM approval_request_snapshots x),
	 'decisions',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]') FROM approval_decisions x),
	 'states',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.id),'[]') FROM approval_state_events x),
	 'executions',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.approval_request_id),'[]') FROM approval_executions x),
	 'outbox',(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.event_id),'[]') FROM outbox_events x)
	)::text`).Scan(&facts)
	if err != nil {
		t.Fatalf("read approval fact set: %v", err)
	}
	return facts
}

func tokenRecentAuthNoChange(t *testing.T, ctx context.Context, r *PostgresRepository, want error, call func() error) {
	t.Helper()
	before := tokenRecentAuthFacts(t, ctx, r)
	if err := call(); !errors.Is(err, want) || want == nil && err != nil {
		t.Fatalf("got %v, want %v", err, want)
	}
	if after := tokenRecentAuthFacts(t, ctx, r); after != before {
		t.Fatal("rejected operation or receipt replay changed persisted approval facts")
	}
}

func tokenRecentAuthActors(t *testing.T, ctx context.Context, r *PostgresRepository) {
	t.Helper()
	for name, pool := range map[string]queryable{"runtime": r.pool, "writer": r.approvalWriterPool} {
		var current, session string
		var super bool
		if err := pool.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&current, &session, &super); err != nil || current != session || super {
			t.Fatal("real runtime actor qualification", err)
		}
		t.Logf("%s: current_user=%s session_user=%s superuser=%v", name, current, session, super)
	}
}

func TestTokenApprovalRecentAuthenticationPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	r, _, _, _ := newEntitlementApprovalFixture(t, ctx, raw)
	tokenRecentAuthActors(t, ctx, r)
	requester := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "token-recent-requester")
	approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "token-recent-approver")
	base := time.Now().UTC().Truncate(time.Second)
	clock := base
	r.now = func() time.Time { return clock }
	activation := &tokenRecentAuthActivation{true}
	r.WithTokenGovernanceActivation(activation)
	for _, command := range []string{"TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		t.Run(command, func(t *testing.T) {
			clock = base
			owner, reviewer := entitlementApprovalPrincipal(requester), entitlementApprovalPrincipal(approver)
			owner.AuthenticatedAt, reviewer.AuthenticatedAt = base.Add(-10*time.Minute), base.Add(-10*time.Minute)
			staleOwner, staleReviewer := owner, reviewer
			staleOwner.AuthenticatedAt, staleReviewer.AuthenticatedAt = base.Add(-16*time.Minute), base.Add(-16*time.Minute)
			draft := tokenRecentAuthDraft(command, base)
			snapshot := &tokenRecentAuthSnapshot{draft: draft}
			proof := &tokenRecentAuthProof{}
			r.WithApprovalSnapshotSource(snapshot).WithApprovalProofSource(proof)
			create := func(p identity.Principal, d ApprovalDraft) error {
				_, err := r.CreateApprovalDraft(ctx, p, d)
				return err
			}
			for _, auth := range []time.Time{time.Time{}, base.Add(-16 * time.Minute), base.Add(2 * time.Minute)} {
				p := owner
				p.AuthenticatedAt = auth
				want := ErrRecentAuthenticationRequired
				if auth.IsZero() {
					want = ErrForbidden // Existing human-principal prerequisite.
				}
				tokenRecentAuthNoChange(t, ctx, r, want, func() error { return create(p, draft) })
			}
			if snapshot.calls != 0 {
				t.Fatal("stale Create called Token precheck")
			}
			// The final write must reject a session that ages while precheck runs.
			snapshot.after = func() { clock = base.Add(16 * time.Minute) }
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return create(owner, draft) })
			snapshot.after, clock = nil, base
			item, err := r.CreateApprovalDraft(ctx, owner, draft)
			if err != nil {
				t.Fatal("fresh Create", err)
			}
			calls := snapshot.calls
			replayCreate := func() error {
				receipt, err := r.CreateApprovalDraft(ctx, staleOwner, draft)
				if err == nil && (receipt.ID != item.ID || receipt.Version != item.Version) {
					t.Fatal("Create replay lost original receipt")
				}
				return err
			}
			tokenRecentAuthNoChange(t, ctx, r, nil, replayCreate)
			activation.active = false
			tokenRecentAuthNoChange(t, ctx, r, nil, replayCreate)
			activation.active = true
			changed := draft
			changed.Reason += " changed"
			tokenRecentAuthNoChange(t, ctx, r, ErrConflict, func() error { return create(staleOwner, changed) })
			if snapshot.calls != calls {
				t.Fatal("Create replay called Token precheck")
			}
			edit := draft
			edit.IdempotencyKey, edit.Reason = uuid.NewString(), draft.Reason+" edited"
			update := func(p identity.Principal) error {
				_, err := r.UpdateApprovalDraft(ctx, p, item.ID, item.Version, edit)
				return err
			}
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return update(staleOwner) })
			if snapshot.calls != calls {
				t.Fatal("stale Update called Token precheck")
			}
			snapshot.after = func() { clock = base.Add(16 * time.Minute) }
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return update(owner) })
			snapshot.after, clock = nil, base
			edited, err := r.UpdateApprovalDraft(ctx, owner, item.ID, item.Version, edit)
			if err != nil {
				t.Fatal("fresh Update", err)
			}
			calls = snapshot.calls
			replayUpdate := func() error {
				receipt, err := r.UpdateApprovalDraft(ctx, staleOwner, item.ID, item.Version, edit)
				if err == nil && (receipt.ID != edited.ID || receipt.Version != edited.Version) {
					t.Fatal("Update replay lost original receipt")
				}
				return err
			}
			tokenRecentAuthNoChange(t, ctx, r, nil, replayUpdate)
			activation.active = false
			tokenRecentAuthNoChange(t, ctx, r, ErrTokenGovernanceInactive, replayUpdate)
			activation.active = true
			if snapshot.calls != calls {
				t.Fatal("Update replay called Token precheck")
			}
			submit := func(p identity.Principal) error {
				_, err := r.SubmitApprovalRequest(ctx, p, item.ID, edited.Version)
				return err
			}
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return submit(staleOwner) })
			if snapshot.calls != calls {
				t.Fatal("stale Submit called Token precheck")
			}
			snapshot.after = func() { clock = base.Add(16 * time.Minute) }
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return submit(owner) })
			snapshot.after, clock = nil, base
			submitted, err := r.SubmitApprovalRequest(ctx, owner, item.ID, edited.Version)
			if err != nil {
				t.Fatal("fresh Submit", err)
			}
			calls = snapshot.calls
			replaySubmit := func() error {
				receipt, err := r.SubmitApprovalRequest(ctx, staleOwner, item.ID, edited.Version)
				if err == nil && (receipt.ID != submitted.ID || receipt.Version != submitted.Version) {
					t.Fatal("Submit replay lost original receipt")
				}
				return err
			}
			tokenRecentAuthNoChange(t, ctx, r, nil, replaySubmit)
			activation.active = false
			tokenRecentAuthNoChange(t, ctx, r, ErrTokenGovernanceInactive, replaySubmit)
			activation.active = true
			if snapshot.calls != calls {
				t.Fatal("Submit replay called Token precheck")
			}
			decide := func(p identity.Principal, decision string) error {
				_, err := r.DecideApprovalRequest(ctx, p, item.ID, submitted.Version, decision, "controlled independent decision", draft.CriticalResourceVersions)
				return err
			}
			for _, decision := range []string{"APPROVED", "REJECTED"} {
				tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return decide(staleReviewer, decision) })
			}
			if proof.calls != 0 {
				t.Fatal("stale approval called Token proof")
			}
			// Real checkpoint succeeds; the controlled proof advances wall time.
			// The dedicated writer must independently recheck current auth age.
			proof.after = func() { clock = base.Add(16 * time.Minute) }
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, func() error { return decide(reviewer, "APPROVED") })
			if proof.calls != 1 {
				t.Fatal("proof checkpoint was not traversed")
			}
			proof.after, clock = nil, base
			tokenRecentAuthHTTP(t, ctx, r, staleReviewer, submitted)
			if err := decide(reviewer, "APPROVED"); err != nil {
				t.Fatal("fresh proof approval", err)
			}
			assertEntitlementRequestState(t, ctx, r, item.ID, "APPROVED", submitted.Version+1)
			// Safe closure retains its prior behavior even after authentication ages.
			for _, decision := range []string{"WITHDRAWN", "REJECTED"} {
				other := draft
				other.IdempotencyKey = uuid.NewString()
				created, err := r.CreateApprovalDraft(ctx, owner, other)
				if err != nil {
					t.Fatal(err)
				}
				ready, err := r.SubmitApprovalRequest(ctx, owner, created.ID, created.Version)
				if err != nil {
					t.Fatal(err)
				}
				if decision == "WITHDRAWN" {
					activation.active = false
					closed, err := r.WithdrawApprovalRequest(ctx, staleOwner, ready.ID, ready.Version)
					activation.active = true
					if err != nil || closed.Status != decision {
						t.Fatal("stale-auth safe withdrawal", err)
					}
				} else {
					closed, err := r.DecideApprovalRequest(ctx, reviewer, ready.ID, ready.Version, decision, "controlled rejection", nil)
					if err != nil || closed.Status != decision {
						t.Fatal("fresh rejection", err)
					}
				}
			}
		})
	}
}

func tokenRecentAuthHTTP(t *testing.T, ctx context.Context, r *PostgresRepository, reviewer identity.Principal, submitted ApprovalRequest) {
	t.Helper()
	control, err := r.ApprovalActionControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1", "v2"} {
		for _, decision := range []string{"APPROVED", "REJECTED"} {
			s := NewService(r, nil)
			handler := s.Handler(func(context.Context, string) (identity.Principal, error) { return reviewer, nil })
			body := map[string]any{"expected_version": submitted.Version, "decision": decision, "reason": "controlled HTTP recent authentication"}
			if version == "v2" {
				body["action_control_version"] = control.Version
			} else {
				body["observed_versions"] = json.RawMessage(submitted.CriticalResourceVersions)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/"+version+"/approval-requests/"+submitted.ID.String()+"/actions/decide", strings.NewReader(string(mustJSON(body))))
			request.Header.Set("Authorization", "Bearer controlled-fixture")
			request.Header.Set("Content-Type", "application/json")
			before := tokenRecentAuthFacts(t, ctx, r)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != 401 || !strings.Contains(recorder.Body.String(), "RECENT_AUTHENTICATION_REQUIRED") || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s: status=%d body=%s", version, decision, recorder.Code, recorder.Body.String())
			}
			if tokenRecentAuthFacts(t, ctx, r) != before {
				t.Fatal("HTTP rejection changed approval facts")
			}
		}
	}
}

func TestTokenApprovalRecentAuthenticationLegacyWriterPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL") == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	parsed, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + r.pool.Config().ConnConfig.Database
	parsed.User = url.User("account_approval_writer")
	if err := r.ConfigureApprovalWriter(ctx, parsed.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.approvalWriterPool.Close)
	tokenRecentAuthActors(t, ctx, r)
	control, err := r.ApprovalActionControl(ctx)
	if err != nil || control.Mode != AdminApprovalModeLegacy {
		t.Fatal("fixture must exercise actual LEGACY mode", err)
	}
	requester := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "token-legacy-requester")
	approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "token-legacy-approver")
	base := time.Now().UTC()
	r.now = func() time.Time { return base }
	r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{true})
	for _, command := range []string{"TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		t.Run(command, func(t *testing.T) {
			draft := tokenRecentAuthDraft(command, base)
			r.WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft})
			owner, reviewer := entitlementApprovalPrincipal(requester), entitlementApprovalPrincipal(approver)
			owner.AuthenticatedAt, reviewer.AuthenticatedAt = base, base.Add(-16*time.Minute)
			created, err := r.CreateApprovalDraft(ctx, owner, draft)
			if err != nil {
				t.Fatal(err)
			}
			submitted, err := r.SubmitApprovalRequest(ctx, owner, created.ID, created.Version)
			if err != nil {
				t.Fatal(err)
			}
			decide := func() error {
				_, err := r.DecideApprovalRequest(ctx, reviewer, created.ID, submitted.Version, "APPROVED", "controlled legacy approval", draft.CriticalResourceVersions)
				return err
			}
			tokenRecentAuthNoChange(t, ctx, r, ErrRecentAuthenticationRequired, decide)
			reviewer.AuthenticatedAt = base.Add(-10 * time.Minute)
			if err := decide(); err != nil {
				t.Fatal("fresh legacy writer", command, err)
			}
			assertEntitlementRequestState(t, ctx, r, created.ID, "APPROVED", submitted.Version+1)
		})
	}
}
