package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnstartedApprovalGrantInventoryPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if raw == "" {
		t.Skip("isolated PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	repo, operator, relay, operatorDSN := newEntitlementApprovalFixture(t, ctx, raw)
	tokenRecentAuthActors(t, ctx, repo)
	requester := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "prebegin-diagnostic-requester")
	approver := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "prebegin-diagnostic-approver")
	repo.WithBillingGovernanceActivation(queryLeaseBillingActivation{}).WithTokenGovernanceActivation(&tokenRecentAuthActivation{true})
	repo.WithApprovalProofSource(&tokenRecentAuthProof{})
	data, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	var inputs map[string]approvalencoding.LegacyProofInput
	if err = json.Unmarshal(data, &inputs); err != nil {
		t.Fatal(err)
	}
	input, ok := inputs["WALLET_ADJUSTMENT"]
	if !ok {
		t.Fatal("frozen input missing")
	}
	var descriptor approvalconsumer.Descriptor
	for _, d := range approvalconsumer.BillingCommands() {
		if d.CommandType == input.CommandType {
			descriptor = d
		}
	}
	if descriptor.CommandType == "" {
		t.Fatal("registered descriptor missing")
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
	created, binding, granted := createGrant()
	eventID := binding.EventID
	inventory, err := repo.InspectUnstartedApprovalGrants(ctx, 1, nil)
	if err != nil || len(inventory.Items) != 1 || inventory.Next != nil {
		t.Fatal("inventory missing unstarted grant", inventory, err)
	}
	item := inventory.Items[0]
	if item.OriginalEventID != eventID || item.RequestID != created.ID || item.EffectConclusion != "UNKNOWN" || item.NextAction != "QUERY_ONLY_NO_REEXECUTION" || item.DeliveryVersion != granted.Version {
		t.Fatal("unsafe or unbound inventory", item)
	}
	empty, err := repo.InspectUnstartedApprovalGrants(ctx, 1, &UnstartedGrantCursor{EventID: item.OriginalEventID, Durable: item.Durable})
	if err != nil || len(empty.Items) != 0 {
		t.Fatal("cursor did not advance", empty, err)
	}

	second, _, _ := createGrant()
	page, err := repo.InspectUnstartedApprovalGrants(ctx, 1, nil)
	if err != nil || len(page.Items) != 1 || page.Next == nil {
		t.Fatal("missing continuation", page, err)
	}
	nextPage, err := repo.InspectUnstartedApprovalGrants(ctx, 1, page.Next)
	if err != nil || len(nextPage.Items) != 1 || nextPage.Next != nil || nextPage.Items[0].RequestID == page.Items[0].RequestID {
		t.Fatal("second page missing or duplicate", nextPage, err)
	}
	combined := map[uuid.UUID]bool{page.Items[0].RequestID: true, nextPage.Items[0].RequestID: true}
	if !combined[created.ID] || !combined[second.ID] {
		t.Fatal("cursor lost original requests")
	}
	for _, limit := range []int{0, 201} {
		if _, err = repo.InspectUnstartedApprovalGrants(ctx, limit, nil); !errors.Is(err, ErrApprovalRecoveryPlanInvalid) {
			t.Fatal("invalid limit accepted", err)
		}
	}
	// Execute the actual CLI factory and JSON writer against this owned DB.
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binary := filepath.Join(t.TempDir(), "approval-grant-inventory"+suffix)
	build := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"+suffix), "build", "-o", binary, "../../cmd/approval-grant-inventory")
	if err = build.Run(); err != nil {
		t.Fatal("build actual inventory CLI", err)
	}
	command := exec.CommandContext(ctx, binary, "--limit", "200")
	// ConnConfig.ConnString retains the original coordinator DSN even after
	// fixture fields are changed. Use the fixture's explicit owned DB URL.
	runtimeURL, err := url.Parse(operatorDSN)
	if err != nil {
		t.Fatal("owned fixture URL invalid")
	}
	runtimeURL.User = url.User("account_service")
	command.Env = append(os.Environ(), "APPROVAL_GRANT_INVENTORY_DATABASE_URL="+runtimeURL.String())
	output, err := command.Output()
	if err != nil {
		t.Fatal("actual inventory CLI failed", err)
	}
	var cliInventory UnstartedGrantInventory
	if err = json.Unmarshal(output, &cliInventory); err != nil || cliInventory.SchemaVersion != 1 || len(cliInventory.Items) != 2 || cliInventory.Next != nil {
		t.Fatal("actual CLI JSON inventory invalid", err)
	}
	for _, i := range cliInventory.Items {
		if !combined[i.RequestID] || i.EffectConclusion != "UNKNOWN" || i.NextAction != "QUERY_ONLY_NO_REEXECUTION" {
			t.Fatal("actual CLI lost unknown binding")
		}
	}
	afterGrant, err := repo.ApprovalAuthorizationDeliveryState(ctx, eventID, binding.Durable, binding.Workload)
	if err != nil || afterGrant.Version != granted.Version || afterGrant.State != granted.State {
		t.Fatal("inventory changed delivery", err)
	}
	// The controlled crash boundary: no Begin or domain Execute has been called.
	var executions int
	if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM approval_executions WHERE approval_request_id=$1`, created.ID).Scan(&executions); err != nil || executions != 0 {
		t.Fatal("baseline execution facts", err)
	}
	plan, err := repo.PlanProofApprovalRecovery(ctx, 200)
	if err != nil {
		t.Fatal("ordinary plan", err)
	}
	for _, candidate := range plan.Candidates {
		if candidate.RequestID == created.ID {
			t.Fatal("baseline ordinary planner unexpectedly sees unstarted grant")
		}
	}
	if plan.Control.Mode != AdminApprovalModeProofRequired {
		t.Fatal("not normal proof mode")
	}
	if _, err = repo.PlanApprovalReconciliation(ctx, 200); !errors.Is(err, ErrApprovalReconciliationBlocked) {
		t.Fatal("baseline cutover planner not restricted", err)
	}
	t.Logf("BASELINE_WINDOW: state=%s attempts=%d executions=%d ordinary_candidates=%d cutover_blocked_in_mode=%s", granted.State, granted.DomainAttemptCount, executions, len(plan.Candidates), plan.Control.Mode)
	// A late legitimate Begin remains possible. Mere absence is not a fence or
	// financial zero-effect proof; no second grant or domain call is attempted.
	if _, err = repo.BeginApprovalExecutionWithAuthorization(ctx, created.ID, binding.Workload, eventID.String(), input.CriticalResourceVersions, input.FinancialSnapshot, binding); err != nil {
		t.Fatal("late legitimate Begin", err)
	}
	after, err := repo.PlanProofApprovalRecovery(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range after.Candidates {
		if candidate.RequestID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("begun original absent from normal planner")
	}
	postBegin, err := repo.InspectUnstartedApprovalGrants(ctx, 200, nil)
	if err != nil || len(postBegin.Items) != 1 || postBegin.Items[0].RequestID != second.ID {
		t.Fatal("begun grant remained unstarted", postBegin, err)
	}
	oldNow := repo.now
	repo.now = func() time.Time { return second.ExecutionDeadline.Add(time.Second) }
	expired, err := repo.expireApprovalRequest(ctx, second.ID)
	repo.now = oldNow
	if err != nil || !expired {
		t.Fatal("expiry fixture", err)
	}
	invalidated, err := repo.InspectUnstartedApprovalGrants(ctx, 200, nil)
	if err != nil || len(invalidated.Items) != 1 || invalidated.Items[0].RequestStatus != "INVALIDATED" || invalidated.Items[0].EffectConclusion != "UNKNOWN" {
		t.Fatal("invalidation hid unknown grant", invalidated, err)
	}
	// Real migration caller cannot impersonate a runtime reader via SET ROLE.
	if _, err = operator.Exec(ctx, "SET ROLE account_service"); err != nil {
		t.Fatal(err)
	}
	_, err = operator.Exec(ctx, "SELECT * FROM public.list_approval_unstarted_grants(1,NULL,NULL)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatal("migration caller bypassed session identity", err)
	}
	var privileged bool
	if err = repo.pool.QueryRow(ctx, `SELECT has_schema_privilege('account_approval_authorization_owner','public','CREATE')
 OR has_function_privilege('account_approval_writer','public.list_approval_unstarted_grants(integer,uuid,text)','EXECUTE')
 OR has_function_privilege('account_approval_recovery','public.list_approval_unstarted_grants(integer,uuid,text)','EXECUTE')`).Scan(&privileged); err != nil || privileged {
		t.Fatal("inventory ACL widened", err)
	}
	identitySQL := `SELECT (SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s),pg_get_functiondef(p.oid)||p.proacl::text FROM pg_proc p WHERE p.oid='public.list_approval_unstarted_grants(integer,uuid,text)'::regprocedure`
	var ledgerBefore, fnBefore, ledgerAfter, fnAfter string
	if err = operator.QueryRow(ctx, identitySQL).Scan(&ledgerBefore, &fnBefore); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	err = accountschema.DownOne(ctx, operator, loaded)
	if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
		t.Fatal("inventory down did not refuse", err)
	}
	if err = operator.QueryRow(ctx, identitySQL).Scan(&ledgerAfter, &fnAfter); err != nil || ledgerBefore != ledgerAfter || fnBefore != fnAfter {
		t.Fatal("refused down changed ledger/function", err)
	}
	afterDown, err := repo.InspectUnstartedApprovalGrants(ctx, 200, nil)
	beforeItems, _ := json.Marshal(invalidated.Items)
	afterItems, _ := json.Marshal(afterDown.Items)
	if err != nil || string(beforeItems) != string(afterItems) {
		t.Fatal("refused down changed unknown facts", err)
	}
	t.Log("BASELINE_LATE_BEGIN: real original now visible to normal recovery; no domain execution performed")
}
