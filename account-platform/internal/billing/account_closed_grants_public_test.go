package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClosedGrantPublishedOwnerEffects(t *testing.T) {
	raw := os.Getenv("F08_BILLING_URL")
	if raw == "" {
		t.Skip("guarded public Billing fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, e := NewPostgresRepository(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	for _, name := range []string{"unicode16", "unicode128", "foreign", "expired", "frozen", "anomaly", "reconciliation", "reserved"} {
		t.Run(name, func(t *testing.T) {
			coordinator, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, e := r.GetWallet(ctx, order.WalletID)
			if e != nil {
				t.Fatal(e)
			}
			member, actor := uuid.New(), uuid.New()
			if _, e = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); e != nil {
				t.Fatal(e)
			}
			req := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: actor, IdempotencyKey: uuid.NewString()}
			if e = r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&req.ComponentID, &req.PackageID); e != nil {
				t.Fatal(e)
			}
			grant, _, e := r.AllocateMemberQuota(ctx, req)
			if e != nil {
				t.Fatal(e)
			}
			if name == "reserved" {
				if _, e = r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 1, 100, nil, r.now().Add(time.Hour)); e != nil {
					t.Fatal(e)
				}
			}
			_, e = r.IsolateAccountClosedGrants(ctx, uuid.New(), member, 2)
			if name == "reserved" {
				if e == nil {
					t.Fatal("reserved grant isolation accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			candidate, life, e := r.AccountClosedGrantCandidate(ctx, wallet.OwnerID, grant.ID)
			if e != nil || life != 2 || candidate.Status != "SUSPENDED" {
				t.Fatal("real candidate alias/closure tuple", candidate, life, e)
			}
			jobctx, e := serviceauth.ContextForJob(ctx, "token-gateway", "closed-grant-reclaim", uuid.NewString())
			if e != nil {
				t.Fatal(e)
			}
			jobctx = humandelegation.WithClaims(jobctx, humandelegation.Claims{DelegationID: uuid.New(), HumanUserID: actor, Permission: "org.grants.reclaim_closed", AuthorizationVersion: 1})
			snapshot := func() string {
				var b []byte
				e := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('grant',(SELECT to_jsonb(g) FROM member_quota_grants g WHERE id=$1),'component',(SELECT to_jsonb(c) FROM entitlement_components c WHERE id=$2),'audit',(SELECT jsonb_agg(a ORDER BY a.id) FROM member_quota_grant_audits a WHERE grant_id=$1),'commands',(SELECT jsonb_agg(c ORDER BY c.idempotency_key) FROM account_closed_grant_reclaim_commands c WHERE grant_id=$1),'outbox',(SELECT jsonb_agg(o ORDER BY o.event_id) FROM outbox_events o))`, grant.ID, req.ComponentID).Scan(&b)
				if e != nil {
					t.Fatal(e)
				}
				return string(b)
			}
			before := snapshot()
			key := strings.Repeat("键", 16)
			reason := strings.Repeat("因", 167)
			org := wallet.OwnerID
			if name == "unicode128" {
				key = strings.Repeat("键", 128)
				reason = strings.Repeat("因", 500)
			}
			if name == "foreign" {
				org = uuid.New()
			}
			if name == "expired" {
				old := r.now
				r.now = func() time.Time { return grant.ExpiresAt.Add(time.Second) }
				defer func() { r.now = old }()
			}
			if name == "frozen" {
				if _, e = r.RecordAndFreezeForcedRefund(ctx, transaction, "closed-grant-frozen-"+uuid.NewString(), 5, json.RawMessage(`{"confirmed":true}`)); e != nil {
					t.Fatal(e)
				}
				before = snapshot()
			}
			if name == "anomaly" {
				tx, e := r.pool.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				if _, e = createPaymentAnomalyWithinTransaction(ctx, tx, "FORCED_REFUND", "P1", "reclaim-anomaly-"+uuid.NewString(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentTransactionID: &transaction}, map[string]any{"controlled": true}, r.now()); e != nil {
					t.Fatal(e)
				}
				if e = tx.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				before = snapshot()
			}
			if name == "reconciliation" {
				if _, e = coordinator.persistReconciliation(ctx, order.Channel, normalizedReconciliationBusinessDate(r.now()), nil, "MANUAL_API", &actor); e != nil {
					t.Fatal(e)
				}
				var n int
				if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases WHERE payment_order_id=$1 AND status='OPEN'`, order.ID).Scan(&n); e != nil || n == 0 {
					t.Fatal("real reconciliation producer did not create this order's blocker", n, e)
				}
				before = snapshot()
			}
			result, e := r.ReclaimAccountClosedGrant(jobctx, org, grant.ID, actor, life, reason, key)
			if name == "foreign" || name == "expired" || name == "frozen" || name == "anomaly" || name == "reconciliation" {
				if e == nil || snapshot() != before {
					t.Fatal("rejected owner mutated facts", e)
				}
				return
			}
			if e != nil || result.AmountMicrocredits != 500000 || result.Grant.Status != "RECLAIMED_ACCOUNT_CLOSED" {
				t.Fatal(result, e)
			}
			committed := snapshot()
			replay, e := r.ReclaimAccountClosedGrant(jobctx, org, grant.ID, actor, life, reason, key)
			if e != nil || !replay.Replayed || snapshot() != committed {
				t.Fatal("receipt replay duplicated effect", e)
			}
			if _, e = r.ReclaimAccountClosedGrant(jobctx, org, grant.ID, actor, life, "改"+strings.TrimPrefix(reason, "因"), key); !errors.Is(e, ErrIdempotencyConflict) || snapshot() != committed {
				t.Fatal("changed facts same key", e)
			}
			if _, e = r.ReclaimAccountClosedGrant(jobctx, org, grant.ID, actor, life, strings.Repeat("因", 501), uuid.NewString()); e == nil || snapshot() != committed {
				t.Fatal("overlong reason", e)
			}
		})
	}
}
