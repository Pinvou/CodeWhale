package billing

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

func TestAccountClosureIsolationScrubsContactWithoutFinancialMutation(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	var payer uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT payer_user_id FROM payment_orders WHERE id=$1`, order.ID).Scan(&payer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET mobile_e164='controlled-closure-contact' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('order',(SELECT to_jsonb(p)-'mobile_e164' FROM payment_orders p WHERE id=$1),'transactions',(SELECT jsonb_agg(to_jsonb(t) ORDER BY t.id) FROM payment_transactions t WHERE payment_order_id=$1),'packages',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM entitlement_packages p WHERE payment_order_id=$1),'wallet',(SELECT to_jsonb(w) FROM wallets w WHERE id=$2),'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM journal_entries j))`, order.ID, order.WalletID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	before := snapshot()
	closureID := uuid.New()
	first, err := r.IsolateAccountClosedGrants(ctx, closureID, payer, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed := func() {
		var absent bool
		if err := r.pool.QueryRow(ctx, `SELECT mobile_e164 IS NULL FROM payment_orders WHERE id=$1`, order.ID).Scan(&absent); err != nil || !absent || snapshot() != before {
			t.Fatal("contact cleanup changed financial facts or left contact", absent, err)
		}
	}
	assertScrubbed()
	// The immutable receipt remains authoritative; a resumed metadata phase
	// still scrubs rather than skipping work because isolation already committed.
	if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET mobile_e164='controlled-replayed-contact' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := r.IsolateAccountClosedGrants(ctx, closureID, payer, 2)
	if err != nil || replay.ClosureRequestID != first.ClosureRequestID || replay.IsolatedGrantCount != first.IsolatedGrantCount {
		t.Fatal("isolation receipt replay", replay, err)
	}
	assertScrubbed()
}

func TestAccountClosureIsolationPreservesPriorAuditFacts(t *testing.T) {
	for _, name := range []string{"active", "member-removed", "chargeback", "forced-refund", "reserved"} {
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
			wallet, err := r.GetWallet(ctx, order.WalletID)
			if err != nil {
				t.Fatal(err)
			}
			member, actor := uuid.New(), uuid.New()
			if _, err = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
				t.Fatal(err)
			}
			allocation := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: actor, IdempotencyKey: uuid.NewString()}
			if err = r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&allocation.ComponentID, &allocation.PackageID); err != nil {
				t.Fatal(err)
			}
			grant, _, err := r.AllocateMemberQuota(ctx, allocation)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "member-removed":
				_, err = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "SUSPENDED", 2)
			case "chargeback":
				_, err = r.RecordConfirmedChargebackForReview(ctx, transaction, "closure-audit-"+uuid.NewString(), json.RawMessage(`{"confirmed":true}`))
			case "forced-refund":
				_, err = r.RecordAndFreezeForcedRefund(ctx, transaction, "closure-audit-"+uuid.NewString(), 5, json.RawMessage(`{"confirmed":true}`))
			case "reserved":
				_, err = r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 1, 100, nil, r.now().Add(time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			readGrant := func() MemberQuotaGrant {
				items, err := r.ListMemberQuotaGrants(ctx, wallet.OwnerID, &member, 10)
				if err != nil || len(items) != 1 || items[0].ID != grant.ID {
					t.Fatal("current original grant", items, err)
				}
				return items[0]
			}
			prior := readGrant()
			wantReason := map[string]string{"member-removed": "MEMBER_REMOVED", "chargeback": "CHARGEBACK_REVIEW", "forced-refund": "FORCED_REFUND_REVIEW"}[name]
			if wantReason == "" {
				if prior.Status != "ACTIVE" || prior.SuspensionReason != nil {
					t.Fatal("fixture did not produce active grant", prior)
				}
			} else if prior.Status != "SUSPENDED" || prior.SuspensionReason == nil || *prior.SuspensionReason != wantReason {
				t.Fatal("real producer did not create expected suspension", prior)
			}
			// Monetary facts and existing financial evidence must not be changed
			// by retaining the old reason. The established bucket transfer and
			// grant lifecycle fields are checked separately. The component trigger
			// also projects package.updated_at; its exact rule is checked below.
			stableMoney := func() string {
				var raw []byte
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'order',(SELECT to_jsonb(p) FROM payment_orders p WHERE id=$1),
'transaction',(SELECT to_jsonb(p) FROM payment_transactions p WHERE id=$2),
'package',(SELECT to_jsonb(p)-'updated_at' FROM entitlement_packages p WHERE id=$3),
'wallet',(SELECT to_jsonb(w) FROM wallets w WHERE id=$4),
'grant',(SELECT to_jsonb(g)-'status'-'suspension_reason'-'allocation_version'-'updated_at' FROM member_quota_grants g WHERE id=$5),
'component',(SELECT to_jsonb(c)-'active_grant_microcredits'-'suspended_grant_microcredits'-'updated_at' FROM entitlement_components c WHERE id=$6),
'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),
'chargebacks',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chargeback_cases c),
'forced_refunds',(SELECT jsonb_agg(to_jsonb(f) ORDER BY id) FROM forced_refund_cases f),
'anomalies',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM payment_anomaly_cases a))`, order.ID, transaction, allocation.PackageID, wallet.ID, grant.ID, allocation.ComponentID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			wholeState := func() string {
				var raw []byte
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'grant',(SELECT to_jsonb(g) FROM member_quota_grants g WHERE id=$1),
'component',(SELECT to_jsonb(c) FROM entitlement_components c WHERE id=$2),
'package',(SELECT to_jsonb(p) FROM entitlement_packages p WHERE id=$3),
'audits',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM member_quota_grant_audits a WHERE grant_id=$1),
'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o),
'isolations',(SELECT jsonb_agg(to_jsonb(i) ORDER BY closure_request_id) FROM account_closure_grant_isolations i))`, grant.ID, allocation.ComponentID, allocation.PackageID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return stableMoney() + string(raw)
			}
			readBuckets := func() (int64, int64) {
				var active, suspended int64
				if err := r.pool.QueryRow(ctx, `SELECT active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE id=$1`, allocation.ComponentID).Scan(&active, &suspended); err != nil {
					t.Fatal(err)
				}
				return active, suspended
			}
			var packageUpdatedBefore time.Time
			if err := r.pool.QueryRow(ctx, `SELECT updated_at FROM entitlement_packages WHERE id=$1`, allocation.PackageID).Scan(&packageUpdatedBefore); err != nil {
				t.Fatal(err)
			}
			moneyBefore, stateBefore := stableMoney(), wholeState()
			activeBefore, suspendedBefore := readBuckets()
			closureID := uuid.New()
			first, err := r.IsolateAccountClosedGrants(ctx, closureID, member, 2)
			if name == "reserved" {
				if !errors.Is(err, ErrConflict) || wholeState() != stateBefore {
					t.Fatal("reserved rejection changed original facts", err)
				}
				return
			}
			if err != nil || first.Replayed || first.IsolatedGrantCount != 1 || first.ClosureRequestID != closureID || first.MemberUserID != member || first.SubjectLifecycleVersion != 2 {
				t.Fatal("isolation receipt", first, err)
			}
			after := readGrant()
			if after.Status != "SUSPENDED" || after.SuspensionReason == nil || *after.SuspensionReason != "ACCOUNT_CLOSED" || after.AllocationVersion != prior.AllocationVersion+1 || stableMoney() != moneyBefore {
				t.Fatal("isolation state or financial contract changed")
			}
			var packageClockProjected bool
			if err := r.pool.QueryRow(ctx, `SELECT package.updated_at=GREATEST($2::timestamptz,(SELECT max(updated_at) FROM entitlement_components WHERE package_id=$1)) FROM entitlement_packages package WHERE package.id=$1`, allocation.PackageID, packageUpdatedBefore).Scan(&packageClockProjected); err != nil || !packageClockProjected {
				t.Fatal("existing component-to-package clock projection changed", packageClockProjected, err)
			}
			activeAfter, suspendedAfter := readBuckets()
			moved := int64(0)
			if prior.Status == "ACTIVE" {
				moved = prior.AvailableMicrocredits
			}
			if activeAfter != activeBefore-moved || suspendedAfter != suspendedBefore+moved {
				t.Fatal("original active/suspended bucket transfer changed")
			}
			var auditRaw []byte
			if err := r.pool.QueryRow(ctx, `SELECT snapshot FROM member_quota_grant_audits WHERE grant_id=$1 AND action='ACCOUNT_CLOSED_ISOLATED'`, grant.ID).Scan(&auditRaw); err != nil {
				t.Fatal(err)
			}
			var audit, expected map[string]any
			expectedRaw, err := json.Marshal(after)
			if err != nil || json.Unmarshal(expectedRaw, &expected) != nil || json.Unmarshal(auditRaw, &audit) != nil {
				t.Fatal("valid audit JSON", err)
			}
			priorStatus, hasStatus := audit["prior_status"]
			priorReason, hasReason := audit["prior_suspension_reason"]
			if !hasStatus || !hasReason || priorStatus != prior.Status || (prior.SuspensionReason == nil && priorReason != nil) || (prior.SuspensionReason != nil && priorReason != *prior.SuspensionReason) {
				t.Fatal("isolation audit lost prior status/reason", audit)
			}
			delete(audit, "prior_status")
			delete(audit, "prior_suspension_reason")
			// PostgreSQL rounds timestamps to microseconds; the existing producer
			// records its original clock value. All other after fields stay exact.
			delete(audit, "updated_at")
			delete(expected, "updated_at")
			if !reflect.DeepEqual(audit, expected) {
				t.Fatal("existing after-snapshot fields changed", audit, expected)
			}
			var auditCount int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='ACCOUNT_CLOSED_ISOLATED'`, grant.ID).Scan(&auditCount); err != nil || auditCount != 1 {
				t.Fatal("one original isolation audit", auditCount, err)
			}
			committed := wholeState()
			replay, err := r.IsolateAccountClosedGrants(ctx, closureID, member, 2)
			if err != nil || !replay.Replayed || replay.ClosureRequestID != first.ClosureRequestID || replay.IsolatedGrantCount != 1 || wholeState() != committed {
				t.Fatal("receipt replay rewrote first audit or repeated effect", replay, err)
			}
			if _, err = r.IsolateAccountClosedGrants(ctx, closureID, member, 3); !errors.Is(err, ErrIdempotencyConflict) || wholeState() != committed {
				t.Fatal("changed lifecycle reused original receipt", err)
			}
			candidate, lifecycle, err := r.AccountClosedGrantCandidate(ctx, wallet.OwnerID, grant.ID)
			if err != nil || lifecycle != 2 || candidate.Status != "SUSPENDED" || candidate.SuspensionReason == nil || *candidate.SuspensionReason != "ACCOUNT_CLOSED" {
				t.Fatal("existing closed-grant recognition broken", candidate, lifecycle, err)
			}
			jobContext, err := serviceauth.ContextForJob(ctx, "token-gateway", "closed-grant-reclaim", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			jobContext = humandelegation.WithClaims(jobContext, humandelegation.Claims{DelegationID: uuid.New(), HumanUserID: actor, Permission: "org.grants.reclaim_closed", AuthorizationVersion: 1})
			result, err := r.ReclaimAccountClosedGrant(jobContext, wallet.OwnerID, grant.ID, actor, lifecycle, "controlled closed-grant recovery", uuid.NewString())
			if name == "chargeback" || name == "forced-refund" {
				if !errors.Is(err, ErrConflict) || wholeState() != committed {
					t.Fatal("financial review gate changed", result, err)
				}
				return
			}
			if err != nil || result.Grant.Status != "RECLAIMED_ACCOUNT_CLOSED" || result.AmountMicrocredits != prior.AvailableMicrocredits {
				t.Fatal("existing closed-grant reclaim changed", result, err)
			}
			// Even after the grant reaches a later terminal state, isolation replay
			// uses its first receipt and never reconstructs the old reason from now.
			reclaimed := wholeState()
			if _, err = r.IsolateAccountClosedGrants(ctx, closureID, member, 2); err != nil || wholeState() != reclaimed {
				t.Fatal("late replay replaced original isolation evidence", err)
			}
		})
	}
}
