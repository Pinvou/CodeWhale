package billing

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

// Provider/payment facts are controlled through the original retained fixture.
// Real PostgreSQL checks quota bucket movement, inbox replay and financial facts;
// the companion Account schema test supplies an actual cleanup-produced event.
func TestF08MembershipClosureReasonQuotaReplayPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, _ := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil {
		t.Fatal(err)
	}
	member := uuid.New()
	if _, err = r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	request := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()}
	if err = r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&request.ComponentID, &request.PackageID); err != nil {
		t.Fatal(err)
	}
	grant, _, err := r.AllocateMemberQuota(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	financialFacts := func() string {
		var result string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM payment_transactions p))::text`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	stableEffects := func() string {
		var result string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM member_quota_grants g),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM entitlement_packages p),(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM member_quota_grant_audits a),(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o))::text`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	payload, err := json.Marshal(map[string]any{"organization_id": wallet.OwnerID, "user_id": member, "membership_status": "REVOKED", "membership_version": 2, "reason": "ACCOUNT_CLOSURE"})
	if err != nil {
		t.Fatal(err)
	}
	event := events.Event{ID: uuid.New(), Type: "account.membership.changed.v1", AggregateID: uuid.New(), AggregateVersion: 2, TraceID: "controlled-membership-closure", Payload: payload}
	consumer := NewMemberLifecycleConsumer(r)
	beforeMoney := financialFacts()
	if err = consumer.Handle(ctx, event); err != nil {
		t.Fatal("consume explicit closure provenance", err)
	}
	var status, reason string
	var version, allocation, active, suspended int64
	if err = r.pool.QueryRow(ctx, `SELECT status,suspension_reason,membership_version,allocation_version FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&status, &reason, &version, &allocation); err != nil {
		t.Fatal(err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE id=$1`, request.ComponentID).Scan(&active, &suspended); err != nil {
		t.Fatal(err)
	}
	if status != "SUSPENDED" || reason != "MEMBER_REMOVED" || version != 2 || allocation != grant.AllocationVersion+1 || active != 0 || suspended != 500000 || financialFacts() != beforeMoney {
		t.Fatal("closure event changed the existing quota/money contract")
	}
	effects := stableEffects()
	for _, delivery := range []string{"same-event", "same-tuple-new-event", "legacy-four-fields"} {
		t.Run(delivery, func(t *testing.T) {
			if delivery != "same-event" {
				event.ID = uuid.New()
			}
			if delivery == "legacy-four-fields" {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(event.Payload, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "reason")
				event.Payload, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := consumer.Handle(ctx, event); err != nil {
				t.Fatal(err)
			}
			if stableEffects() != effects || financialFacts() != beforeMoney {
				t.Fatal("replay duplicated quota, audit, outbox or financial effects")
			}
		})
	}
	var processed int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE aggregate_id=$1 AND processed_at IS NOT NULL`, event.AggregateID).Scan(&processed); err != nil || processed != 3 {
		t.Fatalf("processed deliveries=%d err=%v", processed, err)
	}
	for _, invalid := range []string{`null`, `"OTHER"`, `""`, `{}`} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &fields); err != nil {
			t.Fatal(err)
		}
		fields["reason"] = json.RawMessage(invalid)
		event.Payload, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		event.ID = uuid.New()
		if err := consumer.Handle(ctx, event); !errors.Is(err, events.ErrInvalidEvent) {
			t.Fatalf("invalid reason %s: %v", invalid, err)
		}
		if stableEffects() != effects || financialFacts() != beforeMoney {
			t.Fatal("rejected reason changed financial or quota facts")
		}
		var exists bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM inbox_messages WHERE event_id=$1)`, event.ID).Scan(&exists); err != nil || exists {
			t.Fatal("invalid event persisted an inbox row", err)
		}
	}
}
