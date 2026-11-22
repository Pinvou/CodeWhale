package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type decisionReceiptProof struct{ calls atomic.Int32 }

func (p *decisionReceiptProof) Proof(ctx context.Context, id uuid.UUID, artifacts approvalencoding.ProofArtifacts) (ApprovalProofResult, error) {
	p.calls.Add(1)
	return (&wallClockProof{}).Proof(ctx, id, artifacts)
}

// Real public migrations and runtime/writer/relay roles. Identity, domain
// snapshot/proof and broker PubAck are controlled seams, not live IdP, Billing
// financial effects or NATS acceptance. All Account state transitions are real.
func TestApprovalDecisionOriginalReceiptPostgres(t *testing.T) {
	r, f, observer := approvalListPoolFixture(t)
	ctx := f.ctx
	r.approvalPresenterReady = approvalPolicyPresenterReady
	u, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + r.pool.Config().ConnConfig.Database
	u.User = url.User("account_migrator")
	version := int64(1)
	for _, step := range []FreshApprovalStep{FreshApprovalQuiesce, FreshApprovalFence, FreshApprovalResources, FreshApprovalActivate, FreshApprovalResume} {
		control, e := RunFreshApprovalStep(ctx, u.String(), version, step, func(context.Context, AdminApprovalControl) (json.RawMessage, error) {
			return json.RawMessage(`{"controlled_broker_check":true}`), nil
		})
		if e != nil {
			t.Fatal("official fresh control setup", step, e)
		}
		version = control.Version
	}
	r.WithBillingGovernanceActivation(queryLeaseBillingActivation{})
	proof := &decisionReceiptProof{}
	r.WithApprovalProofSource(proof)
	owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "decision-owner")
	approver := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "decision-approver")
	other := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "decision-other")
	principal := entitlementApprovalPrincipal(owner)
	handler := NewService(r, nil).Handler(func(_ context.Context, token string) (identity.Principal, error) {
		for _, actor := range []entitlementFixtureActor{owner, approver, other} {
			if token == actor.Subject {
				return entitlementApprovalPrincipal(actor), nil
			}
			if token == actor.Subject+"-stale" {
				p := entitlementApprovalPrincipal(actor)
				p.AuthenticatedAt = time.Now().Add(-20 * time.Minute)
				return p, nil
			}
		}
		return identity.Principal{}, errors.New("controlled invalid token")
	})
	data, e := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
	if e != nil {
		t.Fatal(e)
	}
	var inputs map[string]approvalencoding.LegacyProofInput
	if json.Unmarshal(data, &inputs) != nil {
		t.Fatal("missing existing Billing proof inputs")
	}
	input := inputs["WALLET_ADJUSTMENT"]
	fixture := func(t *testing.T) (ApprovalRequest, ApprovalDraft) {
		t.Helper()
		draft := ApprovalDraft{CommandType: input.CommandType, IdempotencyKey: uuid.NewString(), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand, CriticalResourceVersions: input.CriticalResourceVersions, FinancialSnapshot: input.FinancialSnapshot, Reason: "controlled original Billing decision", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
		return wallClockSubmitDraft(t, r, ctx, principal, draft), draft
	}
	control := func() int64 {
		t.Helper()
		c, e := r.ApprovalActionControl(ctx)
		if e != nil {
			t.Fatal(e)
		}
		return c.Version
	}
	const reason = " Controlled decision reason "
	hash := sha256.Sum256([]byte(reason))
	reasonDigest := hex.EncodeToString(hash[:])
	post := func(item ApprovalRequest, draft ApprovalDraft, token, decision string, v int64, protocol bool, wire int) *httptest.ResponseRecorder {
		body := map[string]any{"expected_version": item.Version, "decision": decision, "reason": reason}
		if protocol {
			body["command_protocol"] = 1
		}
		if wire == 2 {
			body["action_control_version"] = v
		} else {
			body["observed_versions"] = json.RawMessage(draft.CriticalResourceVersions)
		}
		raw, _ := json.Marshal(body)
		q := httptest.NewRequest("POST", fmt.Sprintf("/admin/v%d/approval-requests/%s/actions/decide", wire, item.ID), strings.NewReader(string(raw)))
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("X-Pinvou-Approval-Action-Control-Version", strconv.FormatInt(v, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		return w
	}
	get := func(item ApprovalRequest, token, decision, digest string, expected, origin, current int64) *httptest.ResponseRecorder {
		q := httptest.NewRequest("GET", fmt.Sprintf("/admin/v1/approval-requests/%s/actions/decision-result?command_protocol=1&expected_version=%d&origin_action_control_version=%d&decision=%s&reason_digest=%s", item.ID, expected, origin, decision, digest), nil)
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("X-Pinvou-Approval-Action-Control-Version", strconv.FormatInt(current, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		return w
	}
	facts := func(id uuid.UUID) string {
		t.Helper()
		var raw string
		e := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('request',to_jsonb(a),'snapshot',(SELECT to_jsonb(s) FROM approval_request_snapshots s WHERE s.approval_request_id=a.id),'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM approval_state_events e WHERE e.approval_request_id=a.id),'decisions',(SELECT jsonb_agg(to_jsonb(d) ORDER BY d.id) FROM approval_decisions d WHERE d.approval_request_id=a.id),'executions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM approval_executions x WHERE x.approval_request_id=a.id),'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.event_id) FROM outbox_events o WHERE o.aggregate_id=a.id))::text FROM approval_requests a WHERE a.id=$1`, id).Scan(&raw)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	decode := func(t *testing.T, w *httptest.ResponseRecorder) ApprovalDecisionReceipt {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("original decision confirmation got %d want200: %s", w.Code, w.Body.String())
		}
		var receipt ApprovalDecisionReceipt
		if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ReceiptEventID == uuid.Nil || receipt.DecisionID == uuid.Nil {
			t.Fatal("invalid original decision receipt")
		}
		return receipt
	}
	counts := func(t *testing.T, id uuid.UUID, decision string) {
		t.Helper()
		var n, auth int
		if e := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM approval_decisions WHERE approval_request_id=$1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1')`, id).Scan(&n, &auth); e != nil {
			t.Fatal(e)
		}
		want := 0
		if decision == "APPROVED" {
			want = 1
		}
		if n != 1 || auth != want {
			t.Fatal("decision/authorization count", n, auth, want)
		}
	}
	t.Run("normal-Billing-APPROVE-and-REJECT-v1-v2-exact-replay", func(t *testing.T) {
		for _, wire := range []int{1, 2} {
			for _, decision := range []string{"APPROVED", "REJECTED"} {
				item, draft := fixture(t)
				v := control()
				w := post(item, draft, approver.Subject, decision, v, true, wire)
				if w.Code != 200 {
					t.Fatal("normal canonical Billing decision", wire, decision, w.Code, w.Body.String())
				}
				counts(t, item.ID, decision)
				before := facts(item.ID)
				calls := proof.calls.Load()
				a := decode(t, get(item, approver.Subject, decision, reasonDigest, item.Version, v, v))
				b := decode(t, get(item, approver.Subject, decision, reasonDigest, item.Version, v, v))
				if !reflect.DeepEqual(a, b) || a.Decision != decision || a.ExpectedVersion != item.Version || a.ResultVersion != item.Version+1 {
					t.Fatal("original receipt changed")
				}
				if w = post(item, draft, approver.Subject, decision, v, true, wire); w.Code != 200 {
					t.Fatal("original POST replay not recognized", w.Code, w.Body.String())
				}
				if facts(item.ID) != before || proof.calls.Load() != calls {
					t.Fatal("replay changed facts or called domain proof again")
				}
			}
		}
	})
	t.Run("approved-original-survives-executing-and-failed-execution-version-advancement", func(t *testing.T) {
		item, draft := fixture(t)
		v := control()
		if w := post(item, draft, approver.Subject, "APPROVED", v, true, 2); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		first := decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, v))
		binding := decisionReceiptPublishOriginal(t, ctx, r, item.ID)
		job := uuid.NewString()
		if _, e := r.BeginApprovalExecutionWithAuthorization(ctx, item.ID, "billing-command-executor", job, draft.CriticalResourceVersions, draft.FinancialSnapshot, binding); e != nil {
			t.Fatal("real bound Begin", e)
		}
		decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, v))
		if e := r.CompleteApprovalExecution(ctx, item.ID, "billing-command-executor", job, "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_RESOURCE_NOT_FOUND"); e != nil {
			t.Fatal("real Account executor terminal", e)
		}
		var status string
		var actualVersion int64
		if e := r.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, item.ID).Scan(&status, &actualVersion); e != nil || status != "EXECUTION_FAILED" || actualVersion <= item.Version+1 {
			t.Fatal("real producer did not advance", status, actualVersion, e)
		}
		before := facts(item.ID)
		calls := proof.calls.Load()
		after := decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, v))
		if !reflect.DeepEqual(first, after) {
			t.Fatal("execution progression erased original decision proof")
		}
		if w := post(item, draft, approver.Subject, "APPROVED", v, true, 2); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if facts(item.ID) != before || calls != proof.calls.Load() {
			t.Fatal("recovery reran authorization/execution/proof")
		}
		counts(t, item.ID, "APPROVED")
	})
	t.Run("old-reason-only-event-is-never-upgraded", func(t *testing.T) {
		item, draft := fixture(t)
		v := control()
		if w := post(item, draft, approver.Subject, "REJECTED", v, false, 1); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		before := facts(item.ID)
		if w := get(item, approver.Subject, "REJECTED", reasonDigest, item.Version, v, v); w.Code != 503 {
			t.Fatal("legacy reason-only history falsely confirmed", w.Code, w.Body.String())
		}
		if facts(item.ID) != before {
			t.Fatal("old history changed")
		}
	})
	t.Run("wrong-actor-CAS-decision-reason-origin-cannot-adopt-proof", func(t *testing.T) {
		item, draft := fixture(t)
		v := control()
		if w := post(item, draft, approver.Subject, "REJECTED", v, true, 2); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		before := facts(item.ID)
		for _, q := range []struct {
			token, decision, digest string
			expected, origin        int64
		}{{other.Subject, "REJECTED", reasonDigest, item.Version, v}, {approver.Subject, "APPROVED", reasonDigest, item.Version, v}, {approver.Subject, "REJECTED", strings.Repeat("0", 64), item.Version, v}, {approver.Subject, "REJECTED", reasonDigest, item.Version + 1, v}, {approver.Subject, "REJECTED", reasonDigest, item.Version, v + 1}} {
			if w := get(item, q.token, q.decision, q.digest, q.expected, q.origin, v); w.Code == 200 {
				t.Fatal("wrong original operation adopted proof")
			}
		}
		if facts(item.ID) != before {
			t.Fatal("denied reads changed facts")
		}
	})
	t.Run("self-decision-current-role-and-Token-recent-auth-protections-remain", func(t *testing.T) {
		item, draft := fixture(t)
		v := control()
		if w := post(item, draft, owner.Subject, "APPROVED", v, true, 2); w.Code == 200 {
			t.Fatal("self approval")
		}
		wallClockNoAuthorization(t, ctx, r, item.ID)
		if w := post(item, draft, approver.Subject, "REJECTED", v, true, 2); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		before := facts(item.ID)
		if _, e := observer.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, approver.ID); e != nil {
			t.Fatal(e)
		}
		if w := get(item, approver.Subject, "REJECTED", reasonDigest, item.Version, v, v); w.Code == 200 {
			t.Fatal("retired approver disclosed result")
		}
		if _, e := observer.Exec(ctx, `UPDATE role_assignments SET revoked_at=NULL WHERE user_id=$1`, approver.ID); e != nil {
			t.Fatal(e)
		}
		if facts(item.ID) != before {
			t.Fatal("retirement read wrote facts")
		}
		for _, decision := range []string{"APPROVED", "REJECTED"} {
			tokenItem, tokenDraft := wallClockSubmitted(t, r, ctx, principal)
			before := facts(tokenItem.ID)
			w := post(tokenItem, tokenDraft, approver.Subject+"-stale", decision, v, true, 2)
			if w.Code != 401 || !strings.Contains(w.Body.String(), "RECENT_AUTHENTICATION_REQUIRED") || facts(tokenItem.ID) != before {
				t.Fatal("Token recent-auth was bypassed", decision, w.Code, w.Body.String())
			}
		}
	})
	t.Run("proof-event-failure-rolls-back-state-decision-and-authorization", func(t *testing.T) {
		item, draft := fixture(t)
		v := control()
		before := facts(item.ID)
		if _, e := observer.Exec(ctx, `CREATE FUNCTION decision_fixture_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reason_code='INDEPENDENT_DECISION' THEN RAISE EXCEPTION 'controlled decision event failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER decision_fixture_reject BEFORE INSERT ON approval_state_events FOR EACH ROW EXECUTE FUNCTION decision_fixture_reject()`); e != nil {
			t.Fatal(e)
		}
		w := post(item, draft, approver.Subject, "APPROVED", v, true, 2)
		if w.Code < 500 || facts(item.ID) != before {
			t.Fatal("failed proof event left partial effects", w.Code, w.Body.String())
		}
		if _, e := observer.Exec(ctx, `DROP TRIGGER decision_fixture_reject ON approval_state_events;DROP FUNCTION decision_fixture_reject()`); e != nil {
			t.Fatal(e)
		}
		if w = post(item, draft, approver.Subject, "APPROVED", v, true, 2); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, v))
	})
	t.Run("concurrent-original-command-has-one-decision-and-authorization", func(t *testing.T) {
		item, draft := fixture(t)
		v := control()
		done := make(chan *httptest.ResponseRecorder, 2)
		start := make(chan struct{})
		for range 2 {
			go func() { <-start; done <- post(item, draft, approver.Subject, "APPROVED", v, true, 2) }()
		}
		close(start)
		n := 0
		for range 2 {
			w := <-done
			if w.Code == 200 {
				n++
			} else if w.Code != 409 && w.Code < 500 {
				t.Fatal("unexpected competing write status", w.Code, w.Body.String())
			}
		}
		if n < 1 {
			t.Fatal("no CAS winner")
		}
		counts(t, item.ID, "APPROVED")
		before := facts(item.ID)
		decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, v))
		decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, v))
		if facts(item.ID) != before {
			t.Fatal("confirmation changed concurrent facts")
		}
	})
	t.Run("current-SAFE-control-can-only-read-old-approval-and-still-reject", func(t *testing.T) {
		item, draft := fixture(t)
		safe, safeDraft := fixture(t)
		v := control()
		if w := post(item, draft, approver.Subject, "APPROVED", v, true, 2); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		before := facts(item.ID)
		tx, e := observer.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); e == nil {
			_, e = tx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET mode='SAFE_ROLLBACK',approval_wire_version=1,proof_required=true,approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`)
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		current := control()
		calls := proof.calls.Load()
		if w := post(item, draft, approver.Subject, "APPROVED", v, true, 2); w.Code != 409 {
			t.Fatal("stale original control accepted", w.Code, w.Body.String())
		}
		decode(t, get(item, approver.Subject, "APPROVED", reasonDigest, item.Version, v, current))
		if facts(item.ID) != before || proof.calls.Load() != calls {
			t.Fatal("cross-control read advanced approval")
		}
		safeBefore := facts(safe.ID)
		if w := post(safe, safeDraft, approver.Subject, "APPROVED", current, true, 2); w.Code == 200 || facts(safe.ID) != safeBefore {
			t.Fatal("SAFE advanced a new approval", w.Code, w.Body.String())
		}
		if w := post(safe, safeDraft, approver.Subject, "REJECTED", current, true, 1); w.Code != 200 {
			t.Fatal("SAFE rejection was blocked", w.Code, w.Body.String())
		}
		decode(t, get(safe, approver.Subject, "REJECTED", reasonDigest, safe.Version, current, current))
		counts(t, safe.ID, "REJECTED")
	})
}

func decisionReceiptPublishOriginal(t *testing.T, ctx context.Context, r *PostgresRepository, id uuid.UUID) ApprovalAuthorizationBinding {
	t.Helper()
	u, e := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.Path = "/" + r.pool.Config().ConnConfig.Database
	u.User = url.User("account_approval_relay")
	relay, e := NewApprovalRelayStore(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer relay.Close()
	var event uuid.UUID
	if e = r.pool.QueryRow(ctx, `SELECT event_id FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, id).Scan(&event); e != nil {
		t.Fatal(e)
	}
	worker := "decision-receipt-" + uuid.NewString()
	claims, e := relay.ClaimFrozen(ctx, worker, 100, 30*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, claim := range claims {
		if claim.EventID == event {
			found = true
		}
		disposition, e := relay.ProcessFrozen(ctx, worker, claim, 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
			return events.FrozenPublishResult{Disposition: events.FrozenPublished}
		})
		if e != nil || disposition != events.FrozenPublished {
			t.Fatal("controlled original PubAck", e)
		}
	}
	if !found {
		t.Fatal("original authorization was not claimed")
	}
	var durable string
	for _, descriptor := range approvalconsumer.BillingCommands() {
		if descriptor.CommandType == "WALLET_ADJUSTMENT" {
			durable = descriptor.Durable
		}
	}
	if durable == "" {
		t.Fatal("installed Billing descriptor absent")
	}
	binding, e := r.ResolveApprovalAuthorizationBinding(ctx, event, durable, "billing-command-executor")
	if e != nil {
		t.Fatal(e)
	}
	state, e := r.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: approvalconsumer.Sequence(1)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.GrantApprovalAuthorizationDomainAttempt(ctx, binding.EventID, binding.Durable, binding.Workload, state.Version); e != nil {
		t.Fatal("real consumer domain admission", e)
	}
	return binding
}
