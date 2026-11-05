package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// The retained public Up/Verify fixture installs the complete real Account
// schema; runtime operations use the production account_service constructor.
// Only identity verification and the preexisting Token snapshot producer are
// controlled. No financial executor is invoked by a withdrawal or its receipt.
func TestApprovalWithdrawalOriginalReceiptPostgres(t *testing.T) {
	r, f, observer := approvalListPoolFixture(t)
	ctx := f.ctx
	owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "withdraw-owner")
	other := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "withdraw-other")
	p := entitlementApprovalPrincipal(owner)
	handler := NewService(r, nil).Handler(func(_ context.Context, token string) (identity.Principal, error) {
		if token == owner.Subject {
			return p, nil
		}
		if token == other.Subject {
			return entitlementApprovalPrincipal(other), nil
		}
		return identity.Principal{}, errors.New("controlled invalid token")
	})
	control := func() int64 {
		t.Helper()
		c, err := r.ApprovalActionControl(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c.Version
	}
	post := func(item ApprovalRequest, token string, expected, version int64, protocol bool, wire string) *httptest.ResponseRecorder {
		body := map[string]any{"expected_version": expected}
		if protocol {
			body["command_protocol"] = 1
		}
		raw, _ := json.Marshal(body)
		q := httptest.NewRequest("POST", "/admin/"+wire+"/approval-requests/"+item.ID.String()+"/actions/withdraw", strings.NewReader(string(raw)))
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("X-Pinvou-Approval-Action-Control-Version", strconv.FormatInt(version, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		return w
	}
	get := func(item ApprovalRequest, token string, expected, origin, current int64) *httptest.ResponseRecorder {
		q := httptest.NewRequest("GET", fmt.Sprintf("/admin/v2/approval-requests/%s/actions/withdraw-result?command_protocol=1&expected_version=%d&origin_action_control_version=%d", item.ID, expected, origin), nil)
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("X-Pinvou-Approval-Action-Control-Version", strconv.FormatInt(current, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		return w
	}
	facts := func(id uuid.UUID) string {
		t.Helper()
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('request',to_jsonb(a),'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM approval_state_events e WHERE e.approval_request_id=a.id),'decisions',(SELECT jsonb_agg(to_jsonb(d) ORDER BY d.id) FROM approval_decisions d WHERE d.approval_request_id=a.id),'executions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM approval_executions x WHERE x.approval_request_id=a.id),'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.event_id) FROM outbox_events o WHERE o.aggregate_id=a.id))::text FROM approval_requests a WHERE a.id=$1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	fixture := func(t *testing.T) ApprovalRequest {
		t.Helper()
		item, _ := wallClockSubmitted(t, r, ctx, p)
		return item
	}
	decode := func(w *httptest.ResponseRecorder) ApprovalWithdrawalReceipt {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("original withdrawal confirmation got %d want200: %s", w.Code, w.Body.String())
		}
		var v ApprovalWithdrawalReceipt
		if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Status != "WITHDRAWN" || v.ReceiptEventID == uuid.Nil {
			t.Fatal("invalid original receipt")
		}
		return v
	}
	t.Run("lost-ack-original-event-and-same-control-replay", func(t *testing.T) {
		item := fixture(t)
		v := control()
		w := post(item, owner.Subject, item.Version, v, true, "v1")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		before := facts(item.ID)
		first := decode(get(item, owner.Subject, item.Version, v, v))
		again := decode(get(item, owner.Subject, item.Version, v, v))
		if !reflect.DeepEqual(first, again) || first.ExpectedVersion != item.Version || first.ResultVersion != item.Version+1 {
			t.Fatal("receipt identity changed")
		}
		w = post(item, owner.Subject, item.Version, v, true, "v1")
		if w.Code != 200 {
			t.Fatal("same original POST not confirmed", w.Code, w.Body.String())
		}
		if before != facts(item.ID) {
			t.Fatal("confirmation replay wrote state/event/execution/outbox")
		}
		wallClockNoAuthorization(t, ctx, r, item.ID)
	})
	t.Run("other-actor-CAS-control-and-case-cannot-adopt-receipt", func(t *testing.T) {
		item := fixture(t)
		v := control()
		if w := post(item, owner.Subject, item.Version, v, true, "v1"); w.Code != 200 {
			t.Fatal(w.Code)
		}
		before := facts(item.ID)
		for _, q := range []struct {
			token            string
			expected, origin int64
		}{{other.Subject, item.Version, v}, {owner.Subject, item.Version + 1, v}, {owner.Subject, item.Version, v + 1}} {
			if w := get(item, q.token, q.expected, q.origin, v); w.Code == 200 {
				t.Fatal("wrong original invocation confirmed")
			}
		}
		another := fixture(t)
		if w := get(another, owner.Subject, item.Version, v, v); w.Code == 200 {
			t.Fatal("other case adopted original receipt")
		}
		if before != facts(item.ID) {
			t.Fatal("denied reads mutated original facts")
		}
	})
	t.Run("old-event-without-protocol-proof-is-not-fabricated", func(t *testing.T) {
		item := fixture(t)
		v := control()
		if w := post(item, owner.Subject, item.Version, v, false, "v1"); w.Code != 200 {
			t.Fatal("legacy safe-close changed", w.Code, w.Body.String())
		}
		before := facts(item.ID)
		if w := get(item, owner.Subject, item.Version, v, v); w.Code != 503 {
			t.Fatal("legacy {} event falsely treated as original command receipt", w.Code, w.Body.String())
		}
		if before != facts(item.ID) {
			t.Fatal("legacy history rewritten")
		}
	})
	t.Run("current-authority-retirement-blocks-confirmation", func(t *testing.T) {
		item := fixture(t)
		v := control()
		if w := post(item, owner.Subject, item.Version, v, true, "v1"); w.Code != 200 {
			t.Fatal(w.Code)
		}
		before := facts(item.ID)
		if _, err := observer.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, owner.ID); err != nil {
			t.Fatal(err)
		}
		if w := get(item, owner.Subject, item.Version, v, v); w.Code == 200 {
			t.Fatal("retired authority disclosed receipt")
		}
		if _, err := observer.Exec(ctx, `UPDATE role_assignments SET revoked_at=NULL WHERE user_id=$1`, owner.ID); err != nil {
			t.Fatal(err)
		}
		if before != facts(item.ID) {
			t.Fatal("denied read changed approval facts")
		}
	})
	t.Run("event-insert-failure-rolls-back-withdrawal", func(t *testing.T) {
		item := fixture(t)
		v := control()
		before := facts(item.ID)
		if _, err := observer.Exec(ctx, `CREATE FUNCTION withdraw_fixture_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.reason_code='WITHDRAWN_BY_REQUESTER' THEN RAISE EXCEPTION 'controlled withdrawal event failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER withdraw_fixture_reject BEFORE INSERT ON approval_state_events FOR EACH ROW EXECUTE FUNCTION withdraw_fixture_reject()`); err != nil {
			t.Fatal(err)
		}
		w := post(item, owner.Subject, item.Version, v, true, "v1")
		if w.Code != 500 || before != facts(item.ID) {
			t.Fatal("failed receipt left partial state/event", w.Code)
		}
		if _, err := observer.Exec(ctx, `DROP TRIGGER withdraw_fixture_reject ON approval_state_events;DROP FUNCTION withdraw_fixture_reject()`); err != nil {
			t.Fatal(err)
		}
		if w = post(item, owner.Subject, item.Version, v, true, "v1"); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		decode(get(item, owner.Subject, item.Version, v, v))
	})
	t.Run("concurrent-CAS-has-one-event-and-both-confirm-original", func(t *testing.T) {
		item := fixture(t)
		v := control()
		done := make(chan *httptest.ResponseRecorder, 2)
		start := make(chan struct{})
		for range 2 {
			go func() { <-start; done <- post(item, owner.Subject, item.Version, v, true, "v1") }()
		}
		close(start)
		succeeded := 0
		for range 2 {
			w := <-done
			if w.Code == 200 {
				succeeded++
			} else if w.Code != 500 {
				t.Fatal("unexpected concurrent admission", w.Code, w.Body.String())
			}
		}
		if succeeded < 1 {
			t.Fatal("no CAS winner")
		}
		before := facts(item.ID)
		decode(get(item, owner.Subject, item.Version, v, v))
		decode(get(item, owner.Subject, item.Version, v, v))
		if before != facts(item.ID) {
			t.Fatal("concurrent confirmation rewrote facts")
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='WITHDRAWN_BY_REQUESTER'`, item.ID).Scan(&n); err != nil || n != 1 {
			t.Fatal("not exactly one immutable withdrawal", n, err)
		}
	})
	t.Run("control-change-is-read-only-and-SAFE-close-remains-available", func(t *testing.T) {
		item := fixture(t)
		safe := fixture(t)
		v := control()
		if w := post(item, owner.Subject, item.Version, v, true, "v2"); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		before := facts(item.ID)
		update := func(sql string) {
			t.Helper()
			tx, err := observer.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err == nil {
				_, err = tx.Exec(ctx, sql)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		update(`UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`)
		if w := post(item, owner.Subject, item.Version, v, true, "v2"); w.Code != 409 {
			t.Fatal("stale write control was bypassed", w.Code, w.Body.String())
		}
		receipt := decode(get(item, owner.Subject, item.Version, v, control()))
		if receipt.OriginActionControlVersion != v || receipt.ActionControlVersion == v || before != facts(item.ID) {
			t.Fatal("read confirmation upgraded original write or changed state")
		}
		update(`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`)
		update(`UPDATE account_approval_control.admin_approval_control SET mode='PROOF_REQUIRED',approval_wire_version=2,proof_required=true,approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`)
		update(`UPDATE account_approval_control.admin_approval_control SET mode='SAFE_ROLLBACK',approval_wire_version=1,proof_required=true,approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`)
		current := control()
		if w := post(safe, owner.Subject, safe.Version, current, true, "v2"); w.Code != 200 {
			t.Fatal("SAFE withdrawal was closed", w.Code, w.Body.String())
		}
		decode(get(safe, owner.Subject, safe.Version, current, current))
	})
}
