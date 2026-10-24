package account

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/billinggovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Activation, proof and transport observations are controlled contract seams.
// The schema, policies, original approval/authorization/execution facts, actor
// authentication and query lease functions are the actual production paths.
// This test never invokes a Billing or Token financial/domain effect.
type queryLeaseBillingActivation struct{}

func (queryLeaseBillingActivation) Snapshot(context.Context) (billinggovernance.Snapshot, error) {
	snapshot := billinggovernance.Snapshot{SchemaVersion: 1}
	for _, descriptor := range approvalconsumer.BillingCommands() {
		policy, err := approvalpolicy.Lookup(descriptor.CommandType)
		if err != nil {
			return snapshot, err
		}
		if descriptor.CommandType == "ENTITLEMENT_COMPONENT_ADJUSTMENT" {
			snapshot.Activations = append(snapshot.Activations, billinggovernance.Activation{CommandType: descriptor.CommandType, PolicyVersion: 1, SnapshotSchemaVersion: 1, State: "INACTIVE", Version: 1, UpdatedAt: time.Now().UTC()})
		}
		snapshot.Activations = append(snapshot.Activations, billinggovernance.Activation{CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion, SnapshotSchemaVersion: policy.SnapshotSchemaVersion, State: "ACTIVE", Version: 1, UpdatedAt: time.Now().UTC()})
	}
	sort.Slice(snapshot.Activations, func(i, j int) bool {
		a, b := snapshot.Activations[i], snapshot.Activations[j]
		if a.CommandType == b.CommandType {
			return a.PolicyVersion < b.PolicyVersion
		}
		return a.CommandType < b.CommandType
	})
	return snapshot, billinggovernance.ValidateSnapshot(snapshot)
}

func TestApprovalRecoveryQueryAllRegisteredCommandsPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if raw == "" {
		t.Skip("isolated PostgreSQL required")
	}
	userCodeLookupTestGuard(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	repo, operator, relay, _ := newEntitlementApprovalFixture(t, ctx, raw)
	tokenRecentAuthActors(t, ctx, repo)
	requester := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "query-lease-requester")
	approver := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "query-lease-approver")
	repo.WithBillingGovernanceActivation(queryLeaseBillingActivation{}).WithTokenGovernanceActivation(&tokenRecentAuthActivation{true})
	repo.WithApprovalProofSource(&tokenRecentAuthProof{})
	setup, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setup.Close(context.Background()) })
	// Only this test's absent-actor fixture cluster is touched; runtime authority
	// remains the actual restricted LOGIN, never a SET ROLE superuser session.
	if _, err = setup.Exec(ctx, "ALTER ROLE account_approval_recovery LOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := setup.Exec(context.Background(), "ALTER ROLE account_approval_recovery NOLOGIN"); e != nil {
			t.Error(e)
		}
	})
	config := repo.pool.Config().ConnConfig.Copy()
	config.User, config.Password = "account_approval_recovery", ""
	recovery, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovery.Close(context.Background()) })
	if err = VerifyApprovalRecoveryActor(ctx, recovery); err != nil {
		t.Fatal(err)
	}
	leaser, err := NewRecoveryQueryLeaseStore(recovery)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	var inputs map[string]approvalencoding.LegacyProofInput
	if err = json.Unmarshal(data, &inputs); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 14 || len(approvalconsumer.All()) != 14 {
		t.Fatal("closed matrix changed without explicit review")
	}
	candidates := make(map[string]ApprovalRecoveryCandidate)
	for index, descriptor := range approvalconsumer.All() {
		t.Run(descriptor.CommandType, func(t *testing.T) {
			input, ok := inputs[descriptor.CommandType]
			if !ok || input.PolicyVersion != descriptor.PolicyVersion {
				t.Fatal("missing exact frozen command input")
			}
			if _, err = approvalencoding.ConvertLegacyProofInput(input); err != nil {
				t.Fatal(err)
			}
			draft := ApprovalDraft{CommandType: input.CommandType, IdempotencyKey: uuid.NewString(), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand, CriticalResourceVersions: input.CriticalResourceVersions, FinancialSnapshot: input.FinancialSnapshot, Reason: "controlled original execution query lease", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
			repo.WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft})
			var created ApprovalRequest
			err = retryWhileWriterUnavailable(t, ctx, repo, func() error {
				var e error
				created, e = repo.CreateApprovalDraft(ctx, entitlementApprovalPrincipal(requester), draft)
				return e
			})
			if err != nil {
				t.Fatal("create", err)
			}
			submitted, e := repo.SubmitApprovalRequest(ctx, entitlementApprovalPrincipal(requester), created.ID, created.Version)
			if e != nil {
				t.Fatal("submit", e)
			}
			approved, e := repo.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), created.ID, submitted.Version, "APPROVED", "independent query lease fixture", input.CriticalResourceVersions)
			if e != nil || approved.Status != "APPROVED" {
				t.Fatal("approve", approved.Status, e)
			}
			var eventID uuid.UUID
			if e = repo.pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, created.ID).Scan(&eventID); e != nil {
				t.Fatal(e)
			}
			worker := "query-lease-" + uuid.NewString()
			claims, e := relay.ClaimFrozen(ctx, worker, 1, 30*time.Second)
			if e != nil || len(claims) != 1 || claims[0].EventID != eventID {
				t.Fatal("real relay claim", claims, e)
			}
			disposition, e := relay.ProcessFrozen(ctx, worker, claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
				if message.EventID != eventID || len(message.Body) == 0 {
					t.Fatal("wrong frozen event")
				}
				return events.FrozenPublishResult{Disposition: events.FrozenPublished}
			})
			if e != nil || disposition != events.FrozenPublished {
				t.Fatal("publish", disposition, e)
			}
			binding, e := repo.ResolveApprovalAuthorizationBinding(ctx, eventID, descriptor.Durable, descriptor.Workload)
			if e != nil {
				t.Fatal("binding", e)
			}
			state, e := repo.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: approvalconsumer.Sequence(index + 1)})
			if e != nil {
				t.Fatal("observe", e)
			}
			if descriptor.Workload == approvalconsumer.BillingWorkload {
				if _, e = repo.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version); e != nil {
					t.Fatal("grant", e)
				}
			}
			job := eventID.String()
			if _, e = repo.BeginApprovalExecutionWithAuthorization(ctx, created.ID, descriptor.Workload, job, input.CriticalResourceVersions, input.FinancialSnapshot, binding); e != nil {
				t.Fatal("begin original", e)
			}
			// Alternate valid EXECUTING and EXECUTION_FAILED originals. Completing a
			// definite no-effect failure grants no second attempt and writes no funds.
			if index%2 == 0 {
				if e = repo.CompleteApprovalExecution(ctx, created.ID, descriptor.Workload, job, "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_RESOURCE_NOT_FOUND"); e != nil {
					t.Fatal("complete failure", e)
				}
			}
			plan, e := repo.PlanProofApprovalRecovery(ctx, 200)
			if e != nil {
				t.Fatal("actual recovery plan", e)
			}
			var candidate ApprovalRecoveryCandidate
			for _, item := range plan.Candidates {
				if item.RequestID == created.ID {
					candidate = item
				}
			}
			if candidate.ExecutionID == uuid.Nil || candidate.CommandType != descriptor.CommandType || candidate.Durable != descriptor.Durable {
				t.Fatal("original missing in real recovery planner", candidate)
			}
			candidates[descriptor.CommandType] = candidate
			before := tokenRecentAuthFacts(t, ctx, repo)
			lease, e := leaser.Claim(ctx, candidate)
			if e != nil {
				t.Fatal("query original execution", e)
			}
			if lease.QueryCount != 1 || lease.Version != 1 || lease.Token == uuid.Nil {
				t.Fatal("invalid original lease", lease)
			}
			if _, e = leaser.Claim(ctx, candidate); !errors.Is(e, ErrApprovalRecoveryLeaseUnavailable) {
				t.Fatal("concurrent live lease accepted", e)
			}
			if _, e = leaser.Release(ctx, lease); e != nil {
				t.Fatal("release", e)
			}
			if _, e = leaser.Release(ctx, lease); !errors.Is(e, ErrApprovalRecoveryLeaseUnavailable) {
				t.Fatal("stale release accepted", e)
			}
			if _, e = leaser.Claim(ctx, candidate); !errors.Is(e, ErrApprovalRecoveryLeaseUnavailable) {
				t.Fatal("backoff bypass", e)
			}
			if tokenRecentAuthFacts(t, ctx, repo) != before {
				t.Fatal("query lease altered approval/execution/outbox business facts")
			}
		})
	}
	t.Run("exact-original-binding-and-actor-fences", func(t *testing.T) {
		original, ok := candidates["USAGE_SETTLEMENT_REPAIR"]
		if !ok {
			t.Fatal("missing appended command original")
		}
		// Remove an unrelated rejection reason before testing binding negatives:
		// this original's live lease is released and its real backoff has elapsed.
		var nextQuery time.Time
		if e := repo.pool.QueryRow(ctx, `SELECT next_query_at FROM approval_recovery_query_leases WHERE approval_request_id=$1`, original.RequestID).Scan(&nextQuery); e != nil {
			t.Fatal(e)
		}
		if delay := time.Until(nextQuery); delay > 0 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(delay + 20*time.Millisecond):
			}
		}
		before := tokenRecentAuthFacts(t, ctx, repo)
		for _, mutate := range []func(*ApprovalRecoveryCandidate){
			func(c *ApprovalRecoveryCandidate) { c.RequestVersion++ }, func(c *ApprovalRecoveryCandidate) { c.ExecutionID = uuid.New() },
			func(c *ApprovalRecoveryCandidate) { c.OriginalEventID = uuid.New() }, func(c *ApprovalRecoveryCandidate) { c.ApprovalEpoch++ },
			func(c *ApprovalRecoveryCandidate) { c.Durable = approvalconsumer.LegacyBilling().Durable }, func(c *ApprovalRecoveryCandidate) { c.Durable = "future-unregistered-executor-v1" },
		} {
			changed := original
			mutate(&changed)
			if _, e := leaser.Claim(ctx, changed); !errors.Is(e, ErrApprovalRecoveryLeaseUnavailable) {
				t.Fatal("altered original was admitted", changed, e)
			}
		}
		if lease, e := leaser.Claim(ctx, original); e != nil || lease.QueryCount != 2 || lease.Version != 3 {
			t.Fatal("valid original rejected after binding negative controls", lease, e)
		} else if _, e = leaser.Release(ctx, lease); e != nil {
			t.Fatal(e)
		}
		runtimeLeaser, _ := NewRecoveryQueryLeaseStore(repo.pool)
		if _, e := runtimeLeaser.Claim(ctx, original); e == nil {
			t.Fatal("account_service gained recovery function authority")
		}
		if _, e := operator.Exec(ctx, `SELECT * FROM public.claim_approval_recovery_query($1,$2,$3,$4,$5,$6)`, original.RequestID, original.ExecutionID, original.OriginalEventID, original.Durable, original.ApprovalEpoch, original.RequestVersion); e == nil {
			t.Fatal("migrator SET ROLE bypassed session actor fence")
		}
		if _, e := recovery.Exec(ctx, `SELECT * FROM public.approval_requests`); e == nil {
			t.Fatal("query worker gained business table reads")
		}
		if e := VerifyApprovalRecoveryActor(ctx, recovery); e != nil {
			t.Fatal("recovery least privilege drift", e)
		}
		if tokenRecentAuthFacts(t, ctx, repo) != before {
			t.Fatal("binding/actor checks changed business facts")
		}
	})
}

func TestApprovalRecoveryQueryForwardMigrationPostgres(t *testing.T) {
	if strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")) == "" {
		t.Skip("isolated PostgreSQL required")
	}
	userCodeLookupTestGuard(t)
	t.Run("public-upgrade-preserves-original-identities-and-acl", func(t *testing.T) {
		fixture := retainedWalletFenceActors(t)
		var oldLedger, oldACL string
		var allMigrations []migrations.Migration
		repo := fixture.repositoryWithInitialStage(t, func(ctx context.Context, conn *pgx.Conn, installed []migrations.Migration) error {
			allMigrations = installed
			retained := make([]migrations.Migration, 0, len(installed))
			for _, m := range installed {
				if m.Version != 900009 {
					retained = append(retained, m)
				}
			}
			if e := accountschema.Up(ctx, conn, retained); e != nil {
				return e
			}
			// A genuine runtime session must not install the forward migration,
			// even though it owns the public function and all prior SQL is applied.
			configuration := conn.Config().Copy()
			configuration.User = "account_service"
			runtime, e := pgx.ConnectConfig(ctx, configuration)
			if e != nil {
				return e
			}
			defer runtime.Close(context.Background())
			var forwardSQL string
			for _, migration := range installed {
				if migration.Version == 900009 {
					forwardSQL = migration.UpSQL
				}
			}
			if forwardSQL == "" {
				return errors.New("candidate forward migration not installed")
			}
			// The public migrator connection retains its session advisory lock.
			// Execute only the guarded candidate SQL on this runtime negative;
			// the actual positive uses public Up, with the genuine migrator actor.
			_, e = runtime.Exec(ctx, forwardSQL)
			var forbidden *pgconn.PgError
			if !errors.As(e, &forbidden) || forbidden.Code != "42501" {
				return errors.New("runtime did not hit exact migration session fence")
			}
			return conn.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s), pg_get_userbyid(p.proowner)||'|'||p.prosecdef::text||'|'||p.proconfig::text||'|'||p.proacl::text FROM pg_proc p WHERE p.oid='public.claim_approval_recovery_query(uuid,uuid,uuid,text,bigint,bigint)'::regprocedure`).Scan(&oldLedger, &oldACL)
		})
		ctx := context.Background()
		configuration := repo.pool.Config().ConnConfig.Copy()
		configuration.User = "account_migrator"
		migrator, e := pgx.ConnectConfig(ctx, configuration)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { migrator.Close(context.Background()) })
		if _, e = migrator.Exec(ctx, "SET ROLE account_service"); e != nil {
			t.Fatal(e)
		}
		var newLedger, newACL string
		var installed int
		if e := repo.pool.QueryRow(ctx, `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s WHERE version<>900009),pg_get_userbyid(p.proowner)||'|'||p.prosecdef::text||'|'||p.proconfig::text||'|'||p.proacl::text,(SELECT count(*) FROM schema_migrations WHERE version=900009) FROM pg_proc p WHERE p.oid='public.claim_approval_recovery_query(uuid,uuid,uuid,text,bigint,bigint)'::regprocedure`).Scan(&newLedger, &newACL, &installed); e != nil {
			t.Fatal(e)
		}
		if installed != 1 || oldLedger != newLedger || oldACL != newACL || !strings.HasPrefix(newACL, "account_service|true|") {
			t.Fatal("forward upgrade changed frozen ledger/function ownership or ACL", installed, oldACL, newACL)
		}
		if e := accountschema.Up(ctx, migrator, allMigrations); e != nil {
			t.Fatal("exact migration replay", e)
		}
		// Verify this migration's exact refusal even after later additive
		// migrations exist. DownOne intentionally targets the latest version;
		// its current highest-version contract is covered by that migration.
		var downSQL string
		for _, migration := range allMigrations {
			if migration.Version == 900009 {
				downSQL = migration.DownSQL
			}
		}
		if downSQL == "" {
			t.Fatal("original recovery query Down SQL missing")
		}
		down, e := migrator.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		_, e = down.Exec(ctx, downSQL)
		if rollbackErr := down.Rollback(ctx); rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		if e == nil || !strings.Contains(e.Error(), "ACCOUNT_APPROVAL_RECOVERY_QUERY_MATRIX_FORWARD_ONLY") {
			t.Fatal("unsafe recovery rollback accepted", e)
		}
		if _, e := migrations.VerifyApplied(ctx, migrator, allMigrations); e != nil {
			t.Fatal("rollback refusal altered migration identities", e)
		}
	})
}
