package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Public migrations and genuine service/writer logins own all admission facts.
// A separate observer proves the actual blocker PID before wall-clock expiry;
// no concurrent role UPDATE is used to manufacture a serialization failure.
func wallClockWait(t *testing.T, ctx context.Context, observer queryable, predicate string, args ...any) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		var ready bool
		if err := observer.QueryRow(ctx, predicate, args...).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatal("actual PostgreSQL barrier was not observed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type wallClockActivation struct{ after func() }

func (a *wallClockActivation) Snapshot(ctx context.Context) (tokengovernance.Snapshot, error) {
	if a.after != nil {
		a.after()
	}
	return (&tokenRecentAuthActivation{active: true}).Snapshot(ctx)
}

type wallClockProof struct{ age time.Duration }

func (p *wallClockProof) Proof(_ context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
	now := time.Now().UTC().Truncate(time.Second).Add(-p.age)
	response := approvalencoding.ProofResponse{ApprovalRequestID: id, CommandType: artifacts.CommandType, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, PolicyVersion: artifacts.PolicyVersion, ProofSchemaVersion: "1", DomainRevision: "controlled-wall-clock-proof", Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	digest, err := approvalencoding.DeriveProofDigest(response)
	return ApprovalProofResult{Response: response, Digest: hex.EncodeToString(digest[:])}, err
}

func wallClockLegacyRepository(t *testing.T) (*PostgresRepository, *retainedWalletFenceFixture) {
	t.Helper()
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	r.approvalPresenterReady = approvalPolicyPresenterReady
	parsed, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + r.pool.Config().ConnConfig.Database
	parsed.User = url.User("account_approval_writer")
	if err := r.ConfigureApprovalWriter(f.ctx, parsed.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.approvalWriterPool.Close)
	r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
	return r, f
}

func wallClockSubmitted(t *testing.T, r *PostgresRepository, ctx context.Context, principal identity.Principal) (ApprovalRequest, ApprovalDraft) {
	t.Helper()
	draft := tokenRecentAuthDraft("TOKEN_CREDENTIAL_REVOKE", time.Now().UTC())
	return wallClockSubmitDraft(t, r, ctx, principal, draft), draft
}

func wallClockNoAuthorization(t *testing.T, ctx context.Context, r *PostgresRepository, id uuid.UUID) {
	t.Helper()
	var empty bool
	if err := r.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM approval_decisions WHERE approval_request_id=$1)
AND NOT EXISTS(SELECT 1 FROM approval_executions WHERE approval_request_id=$1)
AND NOT EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1')`, id).Scan(&empty); err != nil || !empty {
		t.Fatal("refused request acquired a decision, execution or authorization event", err)
	}
}

func wallClockSubmitDraft(t *testing.T, r *PostgresRepository, ctx context.Context, principal identity.Principal, draft ApprovalDraft) ApprovalRequest {
	t.Helper()
	r.WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft})
	created, err := r.CreateApprovalDraft(ctx, principal, draft)
	if err != nil {
		t.Fatal("create", err)
	}
	submitted, err := r.SubmitApprovalRequest(ctx, principal, created.ID, created.Version)
	if err != nil {
		t.Fatal("submit", err)
	}
	return submitted
}

func wallClockPublishOriginal(t *testing.T, ctx context.Context, r *PostgresRepository, id uuid.UUID) ApprovalAuthorizationBinding {
	t.Helper()
	parsed, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + r.pool.Config().ConnConfig.Database
	parsed.User = url.User("account_approval_relay")
	relay, err := NewApprovalRelayStore(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	var event uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, id).Scan(&event); err != nil {
		t.Fatal(err)
	}
	worker := "wall-clock-" + uuid.NewString()
	claims, err := relay.ClaimFrozen(ctx, worker, 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != event {
		t.Fatal("original frozen event claim", err)
	}
	disposition, err := relay.ProcessFrozen(ctx, worker, claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
		if message.EventID != event || len(message.Body) == 0 {
			t.Error("wrong original frozen event")
		}
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenPublished {
		t.Fatal("controlled original publication", err)
	}
	binding, err := r.ResolveApprovalAuthorizationBinding(ctx, event, "token-credential-revoke-executor-v1", "token-command-executor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: approvalconsumer.Sequence(1)}); err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestApprovalMutationWallClockPostgres(t *testing.T) {
	r, f := wallClockLegacyRepository(t)
	ctx := f.ctx
	// Keep a permanent second super administrator: expiry tests must not weaken
	// the last-super-admin guard or rely on removing its trigger.
	seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "clock-permanent-super")
	t.Run("reject-final-db-clock-over-application-preflight", func(t *testing.T) {
		owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "db-clock-owner")
		approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "db-clock-approver")
		submitted, draft := wallClockSubmitted(t, r, ctx, entitlementApprovalPrincipal(owner))
		principal := entitlementApprovalPrincipal(approver)
		var databaseNow time.Time
		if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
			t.Fatal(err)
		}
		principal.AuthenticatedAt = databaseNow.Add(-16 * time.Minute)
		previousClock := r.now
		r.now = func() time.Time { return principal.AuthenticatedAt.Add(time.Minute) }
		defer func() { r.now = previousClock }()
		before := tokenRecentAuthFacts(t, ctx, r)
		_, err := r.DecideApprovalRequest(ctx, principal, submitted.ID, submitted.Version, "REJECTED", "controlled authoritative database clock", draft.CriticalResourceVersions)
		if !errors.Is(err, ErrRecentAuthenticationRequired) || tokenRecentAuthFacts(t, ctx, r) != before {
			t.Fatal("application preflight clock replaced final database authentication time", err)
		}
	})
	for _, action := range []string{"create", "update", "submit"} {
		t.Run(action+"-domain-wait-expires-role", func(t *testing.T) {
			actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "clock-"+action)
			principal := entitlementApprovalPrincipal(actor)
			draft := tokenRecentAuthDraft("TOKEN_CREDENTIAL_REVOKE", time.Now().UTC())
			source := &tokenRecentAuthSnapshot{draft: draft}
			r.WithApprovalSnapshotSource(source)
			var created ApprovalRequest
			var err error
			if action != "create" {
				created, err = r.CreateApprovalDraft(ctx, principal, draft)
				if err != nil {
					t.Fatal(err)
				}
			}
			until := wallClockRoleExpiry(t, ctx, r, actor.ID)
			source.after = func() { wallClockWait(t, ctx, f.admin, `SELECT clock_timestamp()>=$1::timestamptz`, until) }
			before := tokenRecentAuthFacts(t, ctx, r)
			switch action {
			case "create":
				_, err = r.CreateApprovalDraft(ctx, principal, draft)
			case "update":
				draft.IdempotencyKey = uuid.NewString()
				draft.Reason = "controlled changed intent"
				_, err = r.UpdateApprovalDraft(ctx, principal, created.ID, created.Version, draft)
			case "submit":
				_, err = r.SubmitApprovalRequest(ctx, principal, created.ID, created.Version)
			}
			if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrApprovalInvalidated) {
				t.Fatal("expired requester was not refused after domain read", err)
			}
			if tokenRecentAuthFacts(t, ctx, r) != before {
				t.Fatal("requester expiry changed approval facts")
			}
		})
	}
	for _, kind := range []string{"role", "request"} {
		t.Run("legacy-activation-wait-expires-"+kind, func(t *testing.T) {
			owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "activation-owner")
			approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "activation-approver")
			submitted, draft := wallClockSubmitted(t, r, ctx, entitlementApprovalPrincipal(owner))
			var until time.Time
			if kind == "role" {
				until = wallClockRoleExpiry(t, ctx, r, approver.ID)
			} else {
				if err := r.pool.QueryRow(ctx, `UPDATE approval_requests SET expires_at=clock_timestamp()+interval '1200 milliseconds' WHERE id=$1 RETURNING expires_at`, submitted.ID).Scan(&until); err != nil {
					t.Fatal(err)
				}
			}
			r.WithTokenGovernanceActivation(&wallClockActivation{after: func() { wallClockWait(t, ctx, f.admin, `SELECT clock_timestamp()>=$1::timestamptz`, until) }})
			before := tokenRecentAuthFacts(t, ctx, r)
			_, err := r.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), submitted.ID, submitted.Version, "APPROVED", "controlled legacy activation delay", draft.CriticalResourceVersions)
			r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
			if kind == "role" {
				if !errors.Is(err, ErrApprovalRequestNotFound) || tokenRecentAuthFacts(t, ctx, r) != before {
					t.Fatal("legacy activation wait approved an expired role or changed facts", err)
				}
			} else {
				if !errors.Is(err, ErrApprovalExpired) {
					t.Fatal("legacy activation wait approved an expired request", err)
				}
				assertEntitlementRequestState(t, ctx, r, submitted.ID, "EXPIRED", submitted.Version+1)
				wallClockNoAuthorization(t, ctx, r, submitted.ID)
				var decisions int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1`, submitted.ID).Scan(&decisions); err != nil || decisions != 0 {
					t.Fatal("expired request gained decision", err)
				}
			}
		})
	}
	t.Run("reject-expired-request-after-authority-wait", func(t *testing.T) {
		owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "reject-owner")
		approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "reject-approver")
		submitted, draft := wallClockSubmitted(t, r, ctx, entitlementApprovalPrincipal(owner))
		var until time.Time
		if err := r.pool.QueryRow(ctx, `UPDATE approval_requests SET expires_at=clock_timestamp()+interval '1200 milliseconds' WHERE id=$1 RETURNING expires_at`, submitted.ID).Scan(&until); err != nil {
			t.Fatal(err)
		}
		err := wallClockAcrossLock(t, ctx, r, f.admin, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' FOR UPDATE`, approver.ID, until, func() error {
			_, e := r.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), submitted.ID, submitted.Version, "REJECTED", "controlled rejection wait", draft.CriticalResourceVersions)
			return e
		})
		if !errors.Is(err, ErrApprovalExpired) {
			t.Fatal("REJECT accepted expired request after authority lock", err)
		}
		assertEntitlementRequestState(t, ctx, r, submitted.ID, "EXPIRED", submitted.Version+1)
		wallClockNoAuthorization(t, ctx, r, submitted.ID)
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1`, submitted.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("expired request acquired rejection decision", err)
		}
	})
	for _, kind := range []string{"requester", "approver", "execution-window", "valid-replay"} {
		t.Run("begin-final-"+kind, func(t *testing.T) {
			owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "begin-owner")
			approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "begin-approver")
			var until time.Time
			if kind == "requester" || kind == "approver" {
				user := owner.ID
				if kind == "approver" {
					user = approver.ID
				}
				// Schedule before Create/Submit/APPROVE freeze their binding and
				// version. Only natural time advances during the subsequent wait.
				if err := r.pool.QueryRow(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()+interval '2 seconds' WHERE user_id=$1 AND organization_id IS NULL RETURNING expires_at`, user).Scan(&until); err != nil {
					t.Fatal(err)
				}
			}
			submitted, draft := wallClockSubmitted(t, r, ctx, entitlementApprovalPrincipal(owner))
			approved, err := r.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), submitted.ID, submitted.Version, "APPROVED", "controlled execution admission", draft.CriticalResourceVersions)
			if err != nil {
				t.Fatal(err)
			}
			binding := wallClockPublishOriginal(t, ctx, r, approved.ID)
			job := uuid.NewString()
			operation := func() error {
				_, e := r.BeginApprovalExecutionWithAuthorization(ctx, approved.ID, "token-command-executor", job, draft.CriticalResourceVersions, draft.FinancialSnapshot, binding)
				return e
			}
			if kind == "valid-replay" {
				if err := operation(); err != nil {
					t.Fatal("valid bound Begin", err)
				}
				if _, err := r.pool.Exec(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1 AND organization_id IS NULL`, owner.ID); err != nil {
					t.Fatal(err)
				}
				before := tokenRecentAuthFacts(t, ctx, r)
				r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: false})
				err := operation()
				r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
				if err != nil || tokenRecentAuthFacts(t, ctx, r) != before {
					t.Fatal("existing execution read-only replay changed", err)
				}
				return
			}
			switch kind {
			case "execution-window":
				if err := r.pool.QueryRow(ctx, `UPDATE approval_requests SET execution_deadline=clock_timestamp()+interval '1200 milliseconds' WHERE id=$1 RETURNING execution_deadline`, approved.ID).Scan(&until); err != nil {
					t.Fatal(err)
				}
			}
			var stillValid bool
			if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()<$1::timestamptz`, until).Scan(&stillValid); err != nil || !stillValid {
				t.Fatal("scheduled boundary passed before operation began", err)
			}
			err = wallClockAcrossLock(t, ctx, r, f.admin, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, approver.ID, until, operation)
			if !errors.Is(err, ErrApprovalInvalidated) {
				t.Fatal("Begin accepted expired combined authority/window", err)
			}
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM approval_executions WHERE approval_request_id=$1`, approved.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("expired admission acquired execution", err)
			}
		})
	}
}

func TestApprovalProofWallClockPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	r, _, _, _ := newEntitlementApprovalFixture(t, ctx, os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	observerConfig := r.pool.Config().ConnConfig.Copy()
	observerConfig.User = "postgres"
	observer, err := pgx.ConnectConfig(ctx, observerConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(context.Background())
	r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
	for _, kind := range []string{"request", "proof-age", "recent-auth", "billing-recent-auth", "valid"} {
		t.Run(kind, func(t *testing.T) {
			owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "proof-clock-owner")
			approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "proof-clock-approver")
			principal := entitlementApprovalPrincipal(approver)
			draft := tokenRecentAuthDraft("TOKEN_CREDENTIAL_REVOKE", time.Now().UTC())
			if kind == "billing-recent-auth" {
				raw, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
				if err != nil {
					t.Fatal(err)
				}
				var inputs map[string]approvalencoding.LegacyProofInput
				if err := json.Unmarshal(raw, &inputs); err != nil {
					t.Fatal(err)
				}
				input := inputs["ENTITLEMENT_COMPONENT_ADJUSTMENT"]
				draft = ApprovalDraft{CommandType: input.CommandType, IdempotencyKey: uuid.NewString(), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand, CriticalResourceVersions: input.CriticalResourceVersions, FinancialSnapshot: input.FinancialSnapshot, Reason: "controlled existing Billing proof age", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
				r.WithBillingGovernanceActivation(queryLeaseBillingActivation{})
			}
			submitted := wallClockSubmitDraft(t, r, ctx, entitlementApprovalPrincipal(owner), draft)
			proof := &wallClockProof{}
			r.WithApprovalProofSource(proof)
			var until time.Time
			if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()+interval '1500 milliseconds'`).Scan(&until); err != nil {
				t.Fatal(err)
			}
			if kind == "request" {
				if _, err := r.pool.Exec(ctx, `UPDATE approval_requests SET expires_at=$2 WHERE id=$1`, submitted.ID, until); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "proof-age" {
				proof.age = 29 * time.Second
				until = until.Add(time.Second)
			}
			if kind == "recent-auth" || kind == "billing-recent-auth" {
				principal.AuthenticatedAt = until.Add(-15 * time.Minute)
			}
			before := tokenRecentAuthFacts(t, ctx, r)
			operation := func() error {
				_, e := r.DecideApprovalRequest(ctx, principal, submitted.ID, submitted.Version, "APPROVED", "controlled proof request wait", draft.CriticalResourceVersions)
				return e
			}
			var err error
			if kind == "valid" {
				err = operation()
			} else {
				err = wallClockAcrossLock(t, ctx, r, observer, `SELECT id FROM approval_requests WHERE id=$1 FOR UPDATE`, submitted.ID, until, operation)
			}
			switch kind {
			case "valid":
				if err != nil {
					t.Fatal("valid proof rejected", err)
				}
			case "request":
				if !errors.Is(err, ErrApprovalExpired) {
					t.Fatal("expired request approved after real request lock", err)
				}
				assertEntitlementRequestState(t, ctx, r, submitted.ID, "EXPIRED", submitted.Version+1)
				wallClockNoAuthorization(t, ctx, r, submitted.ID)
			case "proof-age":
				if !errors.Is(err, ErrApprovalProofInvalid) {
					t.Fatal("aged proof accepted after real request lock", err)
				}
			case "recent-auth", "billing-recent-auth":
				if !errors.Is(err, ErrRecentAuthenticationRequired) && !errors.Is(err, ErrApprovalRequestChanged) {
					t.Fatal("recent authentication survived final lock wait", err)
				}
			}
			if (kind == "proof-age" || kind == "recent-auth" || kind == "billing-recent-auth") && tokenRecentAuthFacts(t, ctx, r) != before {
				t.Fatal("proof/auth refusal changed persistent approval facts")
			}
		})
	}
}

func wallClockUnitFacts(t *testing.T, ctx context.Context, r *PostgresRepository, organization uuid.UUID) string {
	t.Helper()
	var facts string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'organization',(SELECT to_jsonb(o) FROM organizations o WHERE id=$1),
'units',(SELECT COALESCE(jsonb_agg(to_jsonb(u) ORDER BY id),'[]') FROM organization_units u WHERE organization_id=$1),
'closure',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY ancestor_id,descendant_id),'[]') FROM organization_unit_closure c WHERE organization_id=$1),
'commands',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY idempotency_key),'[]') FROM organization_unit_commands c WHERE organization_id=$1),
'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events))::text`, organization).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}

func TestOrganizationUnitAuthorityWallClockPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	ownerActor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "unit-clock-owner")
	owner := entitlementApprovalPrincipal(ownerActor)
	for _, name := range []string{"create", "move-source", "move-destination", "move-noop"} {
		t.Run(name, func(t *testing.T) {
			org, err := r.AdminCreateOrganization(ctx, owner, ownerActor.ID, "Controlled wall clock "+name, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			a, err := r.CreateUnit(ctx, owner, org.ID, nil, "A", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			b, err := r.CreateUnit(ctx, owner, org.ID, nil, "B", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			moving, err := r.CreateUnit(ctx, owner, org.ID, &a.Unit.ID, "moving", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_BILLING_OPERATOR", "unit-clock-"+name)
			principal := entitlementApprovalPrincipal(actor)
			if _, err := r.AdminAddMemberWithUnits(ctx, owner, org.ID, actor.ID, []string{"MEMBER"}, nil, nil, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			var until time.Time
			if err := r.pool.QueryRow(ctx, `SELECT clock_timestamp()+interval '1500 milliseconds'`).Scan(&until); err != nil {
				t.Fatal(err)
			}
			grant := func(unit uuid.UUID, expires *time.Time) {
				_, err := r.GrantOrganizationRoleBinding(ctx, owner, RoleBindingGrant{UserID: actor.ID, RoleCode: "UNIT_ADMIN", OrganizationID: &org.ID, UnitID: &unit, ExpiresAt: expires, IdempotencyKey: uuid.NewString(), Reason: "Controlled scheduled unit authority"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if name == "move-destination" {
				grant(a.Unit.ID, nil)
				grant(b.Unit.ID, &until)
			} else {
				grant(a.Unit.ID, &until)
				grant(b.Unit.ID, nil)
			}
			var version int64
			if err := r.pool.QueryRow(ctx, `SELECT structure_version FROM organizations WHERE id=$1`, org.ID).Scan(&version); err != nil {
				t.Fatal(err)
			}
			before := wallClockUnitFacts(t, ctx, r, org.ID)
			err = wallClockAcrossLock(t, ctx, r, f.admin, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, org.ID, until, func() error {
				if name == "create" {
					_, e := r.CreateUnit(ctx, principal, org.ID, &a.Unit.ID, "unauthorized child", uuid.NewString())
					return e
				}
				parent := b.Unit.ID
				if name == "move-noop" {
					parent = a.Unit.ID
				}
				_, e := r.MoveUnit(ctx, principal, org.ID, moving.Unit.ID, &parent, version, uuid.NewString())
				return e
			})
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s accepted expired unit authority: %v", name, err)
			}
			if wallClockUnitFacts(t, ctx, r, org.ID) != before {
				t.Fatal("expired unit authority changed units, closure, receipts, structure version, audit or outbox")
			}
		})
	}
	t.Run("valid-new-write-and-exact-receipts", func(t *testing.T) {
		org, err := r.AdminCreateOrganization(ctx, owner, ownerActor.ID, "valid unit clocks", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		key := uuid.NewString()
		created, err := r.CreateUnit(ctx, owner, org.ID, nil, "valid child", key)
		if err != nil {
			t.Fatal(err)
		}
		before := wallClockUnitFacts(t, ctx, r, org.ID)
		replay, err := r.CreateUnit(ctx, owner, org.ID, nil, "valid child", key)
		if err != nil || !replay.Replayed || replay.Unit.ID != created.Unit.ID || wallClockUnitFacts(t, ctx, r, org.ID) != before {
			t.Fatal("exact create receipt contract changed", err)
		}
		other, err := r.CreateUnit(ctx, owner, org.ID, nil, "destination", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		moveKey := uuid.NewString()
		moved, err := r.MoveUnit(ctx, owner, org.ID, created.Unit.ID, &other.Unit.ID, other.StructureVersion, moveKey)
		if err != nil {
			t.Fatal(err)
		}
		before = wallClockUnitFacts(t, ctx, r, org.ID)
		replay, err = r.MoveUnit(ctx, owner, org.ID, created.Unit.ID, &other.Unit.ID, other.StructureVersion, moveKey)
		if err != nil || !replay.Replayed || replay.Unit.ID != moved.Unit.ID || wallClockUnitFacts(t, ctx, r, org.ID) != before {
			t.Fatal("exact move receipt contract changed", err)
		}
	})
}

func wallClockAcrossLock(t *testing.T, ctx context.Context, r *PostgresRepository, observer queryable, sql string, arg any, boundary time.Time, operation func() error) error {
	t.Helper()
	blocker, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, sql, arg); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- operation() }()
	waitDeadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND $2::integer=ANY(pg_blocking_pids(pid)))`, r.pool.Config().ConnConfig.Database, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("operation returned before required PostgreSQL lock wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("actual PostgreSQL barrier was not observed")
		}
	}
	t.Logf("observed actual lock wait behind owned blocker PID %d", pid)
	wallClockWait(t, ctx, observer, `SELECT clock_timestamp()>=$1::timestamptz`, boundary)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func wallClockRoleExpiry(t *testing.T, ctx context.Context, r *PostgresRepository, user uuid.UUID) time.Time {
	t.Helper()
	var until time.Time
	if err := r.pool.QueryRow(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()+interval '1200 milliseconds' WHERE user_id=$1 AND organization_id IS NULL RETURNING expires_at`, user).Scan(&until); err != nil {
		t.Fatal(err)
	}
	return until
}

func TestApprovalAuthorizationWallClockPostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	var originalFunction, originalMigrations string
	r := f.repositoryWithInitialStage(t, func(ctx context.Context, connection *pgx.Conn, installed []migrations.Migration) error {
		var prefix []migrations.Migration
		for _, migration := range installed {
			if migration.Version < 900010 {
				prefix = append(prefix, migration)
			}
		}
		if err := accountschema.Up(ctx, connection, prefix); err != nil {
			return err
		}
		if err := connection.QueryRow(ctx, `SELECT (to_jsonb(p)-'prosrc')::text FROM pg_proc p WHERE oid='public.lock_approval_writer_authorization(uuid,text)'::regprocedure`).Scan(&originalFunction); err != nil {
			return err
		}
		return connection.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY version),'[]')::text FROM schema_migrations m WHERE version<900010`).Scan(&originalMigrations)
	})
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
	for _, test := range []struct {
		name, sql string
		writer    bool
	}{
		{"writer-user-expiry", `SELECT id FROM users WHERE id=$1 FOR UPDATE`, true},
		{"writer-binding-expiry", `SELECT id FROM role_assignments WHERE user_id=$1 FOR UPDATE`, true},
		{"writer-version-expiry", `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' FOR UPDATE`, true},
		{"runtime-user-expiry", `SELECT id FROM users WHERE id=$1 FOR UPDATE`, false},
		{"runtime-version-expiry", `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' FOR UPDATE`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_BILLING_APPROVER", test.name)
			until := wallClockRoleExpiry(t, ctx, r, actor.ID)
			before := tokenRecentAuthFacts(t, ctx, r)
			err := wallClockAcrossLock(t, ctx, r, f.admin, test.sql, actor.ID, until, func() error {
				pool := r.pool
				if test.writer {
					pool = r.approvalWriterPool
				}
				tx, e := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
				if e != nil {
					return e
				}
				defer tx.Rollback(context.Background())
				if test.writer {
					_, _, _, e = strongApprovalWriterAuthorization(ctx, tx, actor.ID, "platform.billing.repairs.approve")
				} else {
					_, _, _, e = strongPlatformAuthorization(ctx, tx, actor.ID, "platform.billing.repairs.approve")
				}
				return e
			})
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("expired authority returned a binding after observed lock wait: %v", err)
			}
			if tokenRecentAuthFacts(t, ctx, r) != before {
				t.Fatal("authorization-only rejection changed persistent approval facts")
			}
		})
	}
	t.Run("valid-no-wait-and-unknown-permission", func(t *testing.T) {
		actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_BILLING_APPROVER", "valid-clock")
		for _, writer := range []bool{false, true} {
			pool := r.pool
			if writer {
				pool = r.approvalWriterPool
			}
			tx, e := pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			check := strongPlatformAuthorization
			if writer {
				check = strongApprovalWriterAuthorization
			}
			binding, version, _, e := check(ctx, tx, actor.ID, "platform.billing.repairs.approve")
			if e != nil || binding == uuid.Nil || version < 1 {
				t.Fatal("valid authority refused", e)
			}
			if _, _, _, e = check(ctx, tx, actor.ID, "platform.unknown.wall-clock"); !errors.Is(e, ErrForbidden) {
				t.Fatal("unknown permission did not fail closed", e)
			}
		}
	})
	for _, writer := range []bool{false, true} {
		t.Run(fmt.Sprintf("valid-alternative-after-expiry-writer-%t", writer), func(t *testing.T) {
			actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_BILLING_APPROVER", "alternative-clock")
			until := wallClockRoleExpiry(t, ctx, r, actor.ID)
			alternative := uuid.New()
			if _, err := r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_SUPER_ADMIN','ACCOUNT',clock_timestamp()-interval '1 day',clock_timestamp())`, alternative, actor.ID); err != nil {
				t.Fatal(err)
			}
			err := wallClockAcrossLock(t, ctx, r, f.admin, `SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM' FOR UPDATE`, actor.ID, until, func() error {
				pool := r.pool
				if writer {
					pool = r.approvalWriterPool
				}
				tx, e := pool.Begin(ctx)
				if e != nil {
					return e
				}
				defer tx.Rollback(context.Background())
				check := strongPlatformAuthorization
				if writer {
					check = strongApprovalWriterAuthorization
				}
				binding, _, _, e := check(ctx, tx, actor.ID, "platform.billing.repairs.approve")
				if e == nil && binding != alternative {
					return fmt.Errorf("expired candidate retained instead of the still-valid binding")
				}
				return e
			})
			if err != nil {
				t.Fatal("still-valid alternative authority lost", err)
			}
		})
	}
	t.Run("additive-migration-identity-acl-and-replay", func(t *testing.T) {
		config := r.pool.Config().ConnConfig.Copy()
		config.User = "account_migrator"
		connection, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(context.Background())
		if _, err := connection.Exec(ctx, "SET ROLE account_service"); err != nil {
			t.Fatal(err)
		}
		var function, history string
		if err := connection.QueryRow(ctx, `SELECT (to_jsonb(p)-'prosrc')::text FROM pg_proc p WHERE oid='public.lock_approval_writer_authorization(uuid,text)'::regprocedure`).Scan(&function); err != nil || function != originalFunction {
			t.Fatal("function identity/owner/ACL/SECURITY DEFINER/search_path changed", err)
		}
		if err := connection.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY version),'[]')::text FROM schema_migrations m WHERE version<900010`).Scan(&history); err != nil || history != originalMigrations {
			t.Fatal("old migration identities or application facts changed", err)
		}
		installed, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		if err := accountschema.Up(ctx, connection, installed); err != nil {
			t.Fatal("migration replay", err)
		}
		if _, err := migrations.VerifyApplied(ctx, connection, installed); err != nil {
			t.Fatal("actual full applied checksums", err)
		}
		var count int
		if err := connection.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=900010`).Scan(&count); err != nil || count != 1 {
			t.Fatal("additive migration missing", err)
		}
		var guarded bool
		if err := connection.QueryRow(ctx, `SELECT NOT has_schema_privilege('account_approval_authorization_owner','public','CREATE') AND NOT pg_has_role('account_service','account_approval_authorization_owner','MEMBER') AND NOT has_function_privilege('account_service','public.lock_approval_writer_authorization(uuid,text)','EXECUTE') AND has_function_privilege('account_approval_writer','public.lock_approval_writer_authorization(uuid,text)','EXECUTE')`).Scan(&guarded); err != nil || !guarded {
			t.Fatal("runtime function/schema boundary widened", err)
		}
		if _, _, _, err := strongApprovalWriterAuthorization(ctx, r.pool, uuid.New(), "platform.billing.repairs.approve"); err == nil {
			t.Fatal("service invoked dedicated writer function")
		} else {
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "42501" {
				t.Fatal("wrong runtime identity classification", err)
			}
		}
		err = accountschema.DownOne(ctx, connection, installed)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "55000" || !strings.Contains(err.Error(), "ACCOUNT_APPROVAL_AUTHORIZATION_WALL_CLOCK_FORWARD_ONLY") {
			t.Fatal("highest security fence rollback was not explicitly refused", err)
		}
		var after string
		if err := connection.QueryRow(ctx, `SELECT (to_jsonb(p)-'prosrc')::text FROM pg_proc p WHERE oid='public.lock_approval_writer_authorization(uuid,text)'::regprocedure`).Scan(&after); err != nil || after != function {
			t.Fatal("refused rollback modified function identity/privileges", err)
		}
	})
}
