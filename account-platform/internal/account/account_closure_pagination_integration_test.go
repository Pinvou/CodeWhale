package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type closurePaginationRepository struct {
	*PostgresRepository
	reads int
}

func (r *closurePaginationRepository) ListAccountClosureRepairs(ctx context.Context, filter AccountClosureRepairFilter) (AccountClosureRepairPage, error) {
	r.reads++
	return r.PostgresRepository.ListAccountClosureRepairs(ctx, filter)
}

// Real public migrations and account_service reads. Queue records are controlled
// fixture inputs; this does not execute identity deletion or financial settlement.
func TestAccountClosurePaginationPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	base := time.Date(2026, 10, 31, 7, 8, 9, 123456000, time.UTC)
	idAt := func(group, index int) uuid.UUID {
		return uuid.MustParse(fmt.Sprintf("019000%02x-0000-7000-8000-%012x", group, index))
	}
	seed := func(group, count int, status, stage string) {
		t.Helper()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('pinvou.registration_bonus_capable','1',true)`); err != nil {
			t.Fatal(err)
		}
		// Reverse insertion and a 110-item tie force the SQL ordering and the
		// UUID part of the cursor to decide the first page boundary.
		for i := count - 1; i >= 0; i-- {
			user := uuid.New()
			at := base.Add(-time.Duration(i/110) * time.Second)
			if _, err := tx.Exec(ctx, `INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) VALUES($1,'ACTIVE',generate_unique_user_code(),$2,$3,$4,$4)`, user, "page_"+user.String(), "+page:"+user.String(), base); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO account_closure_requests(id,user_id,idempotency_key_digest,request_fingerprint,status,stage,reauthenticated_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,repeat('a',64),repeat('b',64),$3,$4,$5,$5::timestamptz+interval '2 days',$5,$6)`, idAt(group, i), user, status, stage, base.Add(-time.Hour), at); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	seed(1, 203, "IN_PROGRESS", "CLOSURE_REQUESTED")
	seed(2, 100, "BLOCKED", "FINANCIAL_CLEARANCE")
	seed(3, 1, "REPAIR_REQUIRED", "QUIESCING")
	filter := AccountClosureRepairFilter{Status: " in_progress ", Stage: " closure_requested "}
	read := func(t *testing.T, selected AccountClosureRepairFilter) AccountClosureRepairPage {
		t.Helper()
		page, err := r.ListAccountClosureRepairs(ctx, selected)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	advance := func(t *testing.T, selected AccountClosureRepairFilter, page AccountClosureRepairPage) AccountClosureRepairFilter {
		t.Helper()
		if page.NextCursor == nil {
			t.Fatal("expected another page")
		}
		at, id, err := decodeAccountClosureCursor(*page.NextCursor)
		if err != nil || at == nil || id == nil || !at.Equal(page.Items[len(page.Items)-1].UpdatedAt) || *id != page.Items[len(page.Items)-1].ID || at.Nanosecond() != 123456000 {
			t.Fatalf("cursor lost the database boundary: at=%v id=%v err=%v", at, id, err)
		}
		selected.CursorUpdatedAt, selected.CursorID = at, id
		return selected
	}
	snapshot := func() string {
		t.Helper()
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(q) ORDER BY id)::text FROM account_closure_requests q`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("ordered-pages", func(t *testing.T) {
		before := snapshot()
		selected := filter
		index := 0
		for _, count := range []int{100, 100, 3} {
			page := read(t, selected)
			if len(page.Items) != count {
				t.Fatalf("page size=%d want=%d", len(page.Items), count)
			}
			for _, item := range page.Items {
				if item.ID != idAt(1, index) {
					t.Fatalf("missing, repeated or misordered item at %d: %s", index, item.ID)
				}
				index++
			}
			if index < 203 {
				selected = advance(t, selected, page)
			} else if page.NextCursor != nil {
				t.Fatal("last page has a cursor")
			}
		}
		if snapshot() != before {
			t.Fatal("pagination changed queue facts")
		}
	})
	t.Run("exact-and-empty-boundaries", func(t *testing.T) {
		page := read(t, AccountClosureRepairFilter{Status: "BLOCKED", Stage: "FINANCIAL_CLEARANCE"})
		if len(page.Items) != 100 || page.NextCursor != nil {
			t.Fatalf("exact boundary: size=%d cursor=%v", len(page.Items), page.NextCursor)
		}
		for i, item := range page.Items {
			if item.ID != idAt(2, i) {
				t.Fatal("filter leaked or reordered a row")
			}
		}
		page = read(t, AccountClosureRepairFilter{Status: "RETRY"})
		if page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
			t.Fatal("empty page contract changed")
		}
	})
	t.Run("http-authority-and-cursors", func(t *testing.T) {
		actor := func(role string) entitlementFixtureActor {
			actor := entitlementFixtureActor{ID: uuid.New(), Subject: "pagination-" + uuid.NewString()}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,now(),now() FROM capability`, actor.ID, actor.Subject, "+page:"+actor.ID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://entitlement-owned.invalid',$1,$2,now(),now())`, actor.Subject, actor.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,$3,'ACCOUNT',now()-interval '1 day',now())`, uuid.New(), actor.ID, role); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			return actor
		}
		a := actor("PLATFORM_IDENTITY_ADMIN")
		b := actor("PLATFORM_IDENTITY_ADMIN")
		observed := &closurePaginationRepository{PostgresRepository: r}
		h := NewService(observed, nil).Handler(func(_ context.Context, token string) (identity.Principal, error) {
			if token == "second" {
				return entitlementApprovalPrincipal(b), nil
			}
			return entitlementApprovalPrincipal(a), nil
		})
		request := func(token, cursor string) *httptest.ResponseRecorder {
			query := url.Values{"status": {"IN_PROGRESS"}, "stage": {"CLOSURE_REQUESTED"}}
			if cursor != "" {
				query.Set("cursor", cursor)
			}
			req := httptest.NewRequest(http.MethodGet, "/admin/v1/account-closure-requests?"+query.Encode(), nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			return out
		}
		valid := read(t, filter)
		for _, bad := range []string{"not-a-cursor", "e30", *valid.NextCursor + strings.Repeat(" ", 513-len(*valid.NextCursor)), strings.Repeat(" ", 513)} {
			before := observed.reads
			out := request("first", bad)
			var body struct {
				Code string `json:"code"`
			}
			if out.Code != 400 || json.Unmarshal(out.Body.Bytes(), &body) != nil || body.Code != "INVALID_CLOSURE_FILTER" || observed.reads != before {
				t.Fatalf("invalid cursor reached SQL or wrong error: status=%d reads=%d", out.Code, observed.reads-before)
			}
		}
		before := snapshot()
		out := request("first", *valid.NextCursor)
		var page AccountClosureRepairPage
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &page) != nil || len(page.Items) != 100 || page.Items[0].ID != idAt(1, 100) {
			t.Fatalf("real HTTP continuation failed: status=%d", out.Code)
		}
		if snapshot() != before {
			t.Fatal("HTTP pagination changed requests")
		}
		for _, token := range []string{"", "first"} {
			want := 401
			if token != "" {
				if _, err := r.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1 AND role_code='PLATFORM_IDENTITY_ADMIN'`, a.ID); err != nil {
					t.Fatal(err)
				}
				want = 403
			}
			reads := observed.reads
			if out := request(token, *valid.NextCursor); out.Code != want || observed.reads != reads {
				t.Fatalf("cursor bypassed current authority: status=%d want=%d", out.Code, want)
			}
		}
		if out := request("second", *valid.NextCursor); out.Code != 200 {
			t.Fatalf("another authorized subject cannot read its own current page: %d", out.Code)
		}
	})
	t.Run("live-queue-updates", func(t *testing.T) {
		first := read(t, filter)
		selected := advance(t, filter, first)
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET updated_at=$2 WHERE id=ANY($1)`, []uuid.UUID{idAt(1, 0), idAt(1, 150)}, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET status='RETRY',updated_at=$2 WHERE id=$1`, idAt(1, 151), base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		index := 100
		for pages := 0; pages < 3; pages++ {
			page := read(t, selected)
			for _, item := range page.Items {
				for index == 150 || index == 151 {
					index++
				}
				if item.ID != idAt(1, index) {
					t.Fatalf("unchanged range repeated or omitted an item: index=%d id=%s", index, item.ID)
				}
				index++
			}
			if page.NextCursor == nil {
				break
			}
			selected = advance(t, selected, page)
		}
		if index != 203 {
			t.Fatalf("unchanged continuation stopped at %d", index)
		}
		refresh := read(t, filter)
		if len(refresh.Items) != 100 || refresh.Items[0].ID != idAt(1, 0) || refresh.Items[1].ID != idAt(1, 150) {
			t.Fatal("first-page refresh did not discover activity that moved above the cursor")
		}
		if snapshot() != before {
			t.Fatal("live-page reads changed requests")
		}
	})
}
