package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
	"time"
)

func jsonContractAccountFixture(t *testing.T, ctx context.Context, staged bool) (*PostgresRepository, *pgx.Conn, func() (ApprovalRequest, ApprovalAuthorizationBinding, ApprovalDeliveryState), string, string) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	inputBytes, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	source, dir := "", ""
	if staged {
		source, dir = stageFinancialJSONPackage(t)
	}
	repo, operator, relay, _ := newEntitlementApprovalFixture(t, ctx, raw)
	tokenRecentAuthActors(t, ctx, repo)
	requester := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "json-contract-requester")
	approver := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "json-contract-approver")
	repo.WithBillingGovernanceActivation(queryLeaseBillingActivation{}).WithTokenGovernanceActivation(&tokenRecentAuthActivation{true})
	repo.WithApprovalProofSource(&tokenRecentAuthProof{})
	var inputs map[string]approvalencoding.LegacyProofInput
	if err = json.Unmarshal(inputBytes, &inputs); err != nil {
		t.Fatal(err)
	}
	input := inputs["WALLET_ADJUSTMENT"]
	var descriptor approvalconsumer.Descriptor
	for _, d := range approvalconsumer.BillingCommands() {
		if d.CommandType == input.CommandType {
			descriptor = d
		}
	}
	createGrant := func() (ApprovalRequest, ApprovalAuthorizationBinding, ApprovalDeliveryState) {
		draft := ApprovalDraft{CommandType: input.CommandType, IdempotencyKey: uuid.NewString(), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand, CriticalResourceVersions: input.CriticalResourceVersions, FinancialSnapshot: input.FinancialSnapshot, Reason: "controlled grant before begin diagnostic", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
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
		submitted, err := repo.SubmitApprovalRequest(ctx, entitlementApprovalPrincipal(requester), created.ID, created.Version)
		if err != nil {
			t.Fatal("submit", err)
		}
		approved, err := repo.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), created.ID, submitted.Version, "APPROVED", "independent prebegin diagnostic", input.CriticalResourceVersions)
		if err != nil || approved.Status != "APPROVED" {
			t.Fatal("approve", err)
		}
		worker := "prebegin-diagnostic-" + uuid.NewString()
		claims, err := relay.ClaimFrozen(ctx, worker, 1, 30*time.Second)
		if err != nil || len(claims) != 1 {
			t.Fatal("claim", err)
		}
		eventID := claims[0].EventID
		disposition, err := relay.ProcessFrozen(ctx, worker, claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
			if message.EventID != eventID || len(message.Body) == 0 {
				t.Fatal("wrong frozen event")
			}
			return events.FrozenPublishResult{Disposition: events.FrozenPublished}
		})
		if err != nil || disposition != events.FrozenPublished {
			t.Fatal("publish", err)
		}
		binding, err := repo.ResolveApprovalAuthorizationBinding(ctx, eventID, descriptor.Durable, descriptor.Workload)
		if err != nil {
			t.Fatal("binding", err)
		}
		observed, err := repo.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: 1})
		if err != nil {
			t.Fatal("observe", err)
		}
		granted, err := repo.GrantApprovalAuthorizationDomainAttempt(ctx, eventID, binding.Durable, binding.Workload, observed.Version)
		if err != nil || granted.State != "DOMAIN_INFLIGHT" || granted.DomainAttemptCount != 1 {
			t.Fatal("grant", granted, err)
		}
		return approved, binding, granted
	}

	return repo, operator, createGrant, source, dir
}
func jsonContractAccountInsert(ctx context.Context, r *PostgresRepository, request ApprovalRequest, b ApprovalAuthorizationBinding, critical, financial string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO approval_executions(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at,
authorization_event_id,authorization_durable,authorization_approved_request_version,authorization_event_digest,authorization_digest_kind,authorization_serialization_version,authorization_provenance,authorization_approval_epoch)
VALUES($1,$2,$3,decode($4,'hex'),$5,$6,$7,'EXECUTING','{}',$8::jsonb,$9::jsonb,clock_timestamp(),$10,$11,$12,$13,$14,$15,$16,$17)`, uuid.New(), request.ID, request.CommandType, request.CommandFingerprint, fmt.Sprintf("approval:%s:%s:%s", request.ID, request.CommandType, request.CommandFingerprint), b.Workload, b.EventID.String(), critical, financial, b.EventID, b.Durable, b.ApprovedRequestVersion, b.EventDigest, b.DigestKind, b.SerializationVersion, b.Provenance, b.ApprovalEpoch)
	return err
}
func jsonContractAccountFacts(t *testing.T, r *PostgresRepository) string {
	t.Helper()
	var s string
	err := r.pool.QueryRow(context.Background(), `SELECT jsonb_build_object('requests',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM approval_requests x),'executions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM approval_executions x),'decisions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM approval_decisions x),'outbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x),'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x))::text`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestAccountFinancialJSONObjectsPostgres(t *testing.T) {
	if strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")) == "" {
		t.Skip("owned PostgreSQL required")
	}
	for _, scenario := range []string{"fresh-current-producer", "upgrade-current-producer", "dirty-critical-history", "dirty-financial-history"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			r, operator, create, source, dir := jsonContractAccountFixture(t, ctx, scenario != "fresh-current-producer")
			request, binding, _ := create()
			dirty := strings.HasPrefix(scenario, "dirty-")
			if dirty {
				critical, financial := string(request.CriticalResourceVersions), string(request.FinancialSnapshot)
				if scenario == "dirty-critical-history" {
					critical = "[]"
				} else {
					financial = "[]"
				}
				if err := jsonContractAccountInsert(ctx, r, request, binding, critical, financial); err != nil {
					t.Fatal("published predecessor did not admit controlled historical shape", err)
				}
			} else if _, err := r.BeginApprovalExecutionWithAuthorization(ctx, request.ID, binding.Workload, binding.EventID.String(), request.CriticalResourceVersions, request.FinancialSnapshot, binding); err != nil {
				t.Fatal("actual producer", err)
			}
			before := jsonContractAccountFacts(t, r)
			if source != "" {
				addFinancialJSONGuard(t, source, dir)
			} else {
				dir = "../../migrations/account"
			}
			loaded, err := migrations.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if dirty {
				var ledgerBefore, ledgerAfter string
				if err = operator.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`).Scan(&ledgerBefore); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					err = accountschema.Up(ctx, operator, loaded)
					var pg *pgconn.PgError
					if !errors.As(err, &pg) || pg.Code != "23514" {
						t.Fatal("invalid history did not block public upgrade", err)
					}
					if err = operator.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(x) ORDER BY version)::text FROM schema_migrations x`).Scan(&ledgerAfter); err != nil || ledgerBefore != ledgerAfter || jsonContractAccountFacts(t, r) != before {
						t.Fatal("failed upgrade rewrote history", err)
					}
					var n int
					if err = operator.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='public.approval_executions'::regclass AND conname IN ('approval_executions_critical_resource_versions_object_check','approval_executions_financial_snapshot_object_check')`).Scan(&n); err != nil || n != 0 {
						t.Fatal("partial guard persisted", n, err)
					}
				}
				return
			}
			if err = accountschema.Up(ctx, operator, loaded); err != nil {
				t.Fatal(err)
			}
			if _, err = accountschema.VerifyApplied(ctx, operator, loaded); err != nil {
				t.Fatal(err)
			}
			if jsonContractAccountFacts(t, r) != before {
				t.Fatal("valid history changed")
			}
			if _, err = r.BeginApprovalExecutionWithAuthorization(ctx, request.ID, binding.Workload, binding.EventID.String(), request.CriticalResourceVersions, request.FinancialSnapshot, binding); err != nil {
				t.Fatal("original execution replay changed", err)
			}
			for _, column := range []string{"critical_resource_versions", "financial_snapshot"} {
				badRequest, badBinding, _ := create()
				for _, bad := range []string{`[]`, `null`, `42`, `true`, `"text"`} {
					critical, financial := string(badRequest.CriticalResourceVersions), string(badRequest.FinancialSnapshot)
					if column == "critical_resource_versions" {
						critical = bad
					} else {
						financial = bad
					}
					err = jsonContractAccountInsert(ctx, r, badRequest, badBinding, critical, financial)
					var pg *pgconn.PgError
					if !errors.As(err, &pg) || pg.Code != "23514" || pg.ConstraintName != "approval_executions_"+column+"_object_check" {
						t.Fatal("wrong JSON shape not rejected by exact contract", column, err)
					}
				}
				if _, err = r.BeginApprovalExecutionWithAuthorization(ctx, badRequest.ID, badBinding.Workload, badBinding.EventID.String(), badRequest.CriticalResourceVersions, badRequest.FinancialSnapshot, badBinding); err != nil {
					t.Fatal("valid original producer after rejected writes", err)
				}
			}
			before = jsonContractAccountFacts(t, r)
			// Later public migrations may be reversible; verify this release's
			// exact historical Down without rolling back the current installation.
			assertHistoricalRollbackRefusalContext(t, ctx, r, loaded, 900024, "55000", "financial JSON object contracts are forward-only")
			if jsonContractAccountFacts(t, r) != before {
				t.Fatal("historical rollback changed financial facts")
			}
		})
	}
}
