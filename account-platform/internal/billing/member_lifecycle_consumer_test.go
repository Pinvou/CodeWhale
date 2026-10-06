package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func TestPostgresMemberQuotaLatestSnapshotMatchesSequentialProjection(t *testing.T) {
	for _, mode := range []string{"active", "expired", "reserved-review"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			type fixture struct {
				r            *PostgresRepository
				wallet       Wallet
				allocation   MemberQuotaAllocation
				consumer     *MemberLifecycleConsumer
				membershipID uuid.UUID
			}
			fixtures := make([]fixture, 2)
			clock := time.Now().UTC()
			for index := range fixtures {
				r, wallet, allocation, consumer, membershipID := memberConsumerFixture(t)
				r.now = func() time.Time { return clock }
				used, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, allocation.OrganizationID, allocation.MemberUserID, 1, 200, nil, clock.Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Commit(ctx, used.UsageRequestID, used.ID, 100, map[string]any{"input_tokens": 1}); err != nil {
					t.Fatal(err)
				}
				if mode == "reserved-review" {
					held, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, allocation.OrganizationID, allocation.MemberUserID, 1, 200, nil, clock.Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET status='REVIEW' WHERE id=$1`, held.ID); err != nil {
						t.Fatal(err)
					}
				}
				if mode != "active" {
					expires := clock.Add(time.Hour)
					for _, query := range []string{`UPDATE entitlement_packages SET valid_until=$2 WHERE id=$1`, `UPDATE entitlement_components SET expires_at=$2 WHERE package_id=$1`, `UPDATE member_quota_grants SET expires_at=$2 WHERE package_id=$1`} {
						if _, err := r.pool.Exec(ctx, query, allocation.PackageID, expires); err != nil {
							t.Fatal(err)
						}
					}
				}
				fixtures[index] = fixture{r, wallet, allocation, consumer, membershipID}
			}
			clock = clock.Add(2 * time.Hour)
			for index, f := range fixtures {
				if index == 0 {
					for version, status := range []string{"SUSPENDED", "REVOKED"} {
						if _, err := f.r.ApplyOrganizationMemberLifecycle(ctx, f.allocation.OrganizationID, f.allocation.MemberUserID, status, int64(version+2)); err != nil {
							t.Fatal(err)
						}
					}
				}
				// First delivered snapshot is version 4, including an existing member
				// whose initial events are outside the broker retention window.
				if err := f.consumer.Handle(ctx, membershipEvent(t, f.allocation, f.membershipID, 4, "ACTIVE")); err != nil {
					t.Fatal(err)
				}
			}
			read := func(f fixture) []byte {
				var result []byte
				err := f.r.pool.QueryRow(ctx, `SELECT jsonb_build_object('wallet',jsonb_build_array(w.posted_microcredits,w.reserved_microcredits),'lot',l.remaining_microcredits,'component',jsonb_build_array(c.original_microcredits,c.available_microcredits,c.reserved_microcredits,c.used_microcredits,c.active_grant_microcredits,c.suspended_grant_microcredits,c.expired_microcredits,c.revoked_microcredits,c.status),'grant',jsonb_build_array(g.original_microcredits,g.available_microcredits,g.reserved_microcredits,g.used_microcredits,g.expired_microcredits,g.revoked_microcredits,g.status,g.membership_version),'package',p.status) FROM wallets w JOIN entitlement_packages p ON p.wallet_id=w.id JOIN entitlement_components c ON c.package_id=p.id JOIN credit_lots l ON l.id=c.credit_lot_id JOIN member_quota_grants g ON g.component_id=c.id WHERE w.id=$1`, f.wallet.ID).Scan(&result)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			if !bytes.Equal(read(fixtures[0]), read(fixtures[1])) {
				t.Fatal("sequential and latest snapshots differ before financial expiry")
			}
			for _, f := range fixtures {
				_, reserveErr := f.r.ReserveMemberQuota(ctx, uuid.New(), f.wallet.ID, f.allocation.OrganizationID, f.allocation.MemberUserID, 4, 1, nil, clock.Add(time.Hour))
				if mode == "active" && reserveErr != nil {
					t.Fatal("active capacity is not consumable", reserveErr)
				}
				if mode != "active" && reserveErr == nil {
					t.Fatal("expired capacity became consumable during restoration")
				}
				if _, err := f.r.ExpirePackages(ctx, 100); err != nil {
					t.Fatal(err)
				}
				if mode == "reserved-review" {
					var status string
					if err := f.r.pool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE status='REVIEW'`).Scan(&status); err != nil || status != "REVIEW" {
						t.Fatal("unknown held usage was automatically released", err)
					}
				}
				assertBalanced(t, f.r)
			}
			if !bytes.Equal(read(fixtures[0]), read(fixtures[1])) {
				t.Fatal("sequential and latest snapshots differ after financial expiry")
			}
		})
	}
}

func TestPostgresMemberQuotaConsumerBindsStableAggregateAndFacts(t *testing.T) {
	r, _, allocation, consumer, membershipID := memberConsumerFixture(t)
	ctx := context.Background()
	if err := consumer.Handle(ctx, membershipEvent(t, allocation, membershipID, 7, "ACTIVE")); err != nil {
		t.Fatal("first complete snapshot may have version greater than one", err)
	}
	if err := consumer.Handle(ctx, membershipEvent(t, allocation, membershipID, 7, "REVOKED")); !errors.Is(err, ErrConflict) {
		t.Fatal("same version with different state must conflict")
	}
	if err := consumer.Handle(ctx, membershipEvent(t, allocation, uuid.New(), 8, "ACTIVE")); !errors.Is(err, events.ErrInvalidEvent) {
		t.Fatal("member aggregate cannot be replaced")
	}
	foreign := allocation
	foreign.MemberUserID = uuid.New()
	if err := consumer.Handle(ctx, membershipEvent(t, foreign, membershipID, 8, "ACTIVE")); !errors.Is(err, events.ErrInvalidEvent) {
		t.Fatal("stable aggregate cannot move to another member")
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rejected facts acquired Inbox receipts", err)
	}
}

func memberConsumerFixture(t *testing.T) (*PostgresRepository, Wallet, MemberQuotaAllocation, *MemberLifecycleConsumer, uuid.UUID) {
	t.Helper()
	r, wallet, allocation := quotaFixture(t, false)
	if _, _, err := r.AllocateMemberQuota(context.Background(), allocation); err != nil {
		t.Fatal(err)
	}
	return r, wallet, allocation, NewMemberLifecycleConsumer(r), uuid.New()
}

func membershipEvent(t *testing.T, allocation MemberQuotaAllocation, membershipID uuid.UUID, version int64, status string) events.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"organization_id": allocation.OrganizationID, "user_id": allocation.MemberUserID, "membership_status": status, "membership_version": version})
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{ID: uuid.New(), Type: "account.membership.changed.v1", AggregateID: membershipID, AggregateVersion: version, Payload: payload, TraceID: "owned-member-projection", OccurredAt: time.Now().UTC()}
}

func TestPostgresMemberQuotaConsumerContinuityAndAtomicity(t *testing.T) {
	r, _, allocation, consumer, membershipID := memberConsumerFixture(t)
	ctx := context.Background()
	first := membershipEvent(t, allocation, membershipID, 1, "ACTIVE")
	second := membershipEvent(t, allocation, membershipID, 2, "ACTIVE")
	t.Run("initial-and-active-version", func(t *testing.T) {
		for _, event := range []events.Event{first, second} {
			if err := consumer.Handle(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		items, err := r.ListMemberQuotaGrants(ctx, allocation.OrganizationID, nil, 100)
		if err != nil || len(items) != 1 || items[0].MembershipVersion != 2 || items[0].AvailableMicrocredits != 800 {
			t.Fatal("role-only version change must retain usable quota capacity", err)
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		var before, after int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := consumer.Handle(ctx, second); err != nil {
			t.Fatal(err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits`).Scan(&after); err != nil || before != after {
			t.Fatal("duplicate event applied quota twice", err)
		}
	})
	fourth := membershipEvent(t, allocation, membershipID, 4, "ACTIVE")
	t.Run("complete-snapshot-recovers-gap", func(t *testing.T) {
		if err := consumer.Handle(ctx, fourth); err != nil {
			t.Fatal("complete member snapshot must recover a missing prior event", err)
		}
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE event_id=$1 AND processed_at IS NOT NULL`, fourth.ID).Scan(&count); err != nil || count != 1 {
			t.Fatal("accepted complete snapshot must persist its receipt", err)
		}
		if err := consumer.Handle(ctx, membershipEvent(t, allocation, membershipID, 3, "SUSPENDED")); err != nil {
			t.Fatal("late skipped snapshot must be harmless", err)
		}
	})
	t.Run("suspend-and-restore", func(t *testing.T) {
		third := membershipEvent(t, allocation, membershipID, 5, "SUSPENDED")
		if err := consumer.Handle(ctx, third); err != nil {
			t.Fatal(err)
		}
		items, err := r.ListMemberQuotaGrants(ctx, allocation.OrganizationID, nil, 100)
		if err != nil || items[0].Status != "SUSPENDED" || items[0].AvailableMicrocredits != 800 {
			t.Fatal("suspended membership must preserve and restrict capacity", err)
		}
		if err := consumer.Handle(ctx, membershipEvent(t, allocation, membershipID, 6, "ACTIVE")); err != nil {
			t.Fatal(err)
		}
		items, err = r.ListMemberQuotaGrants(ctx, allocation.OrganizationID, nil, 100)
		if err != nil || items[0].Status != "ACTIVE" || items[0].MembershipVersion != 6 || items[0].AvailableMicrocredits != 800 {
			t.Fatal("later active membership must restore existing capacity", err)
		}
	})
	t.Run("effect-and-inbox-rollback-together", func(t *testing.T) {
		if _, err := r.pool.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT fixture_no_membership_update CHECK(event_type<>'billing.member-quota.membership-updated.v1') NOT VALID`); err != nil {
			t.Fatal(err)
		}
		fifth := membershipEvent(t, allocation, membershipID, 7, "ACTIVE")
		if err := consumer.Handle(ctx, fifth); err == nil {
			t.Fatal("fixture did not reject the quota outbox write")
		}
		var version int64
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2`, allocation.OrganizationID, allocation.MemberUserID).Scan(&version); err != nil || version != 6 {
			t.Fatal("failed effect left lifecycle projection committed", err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE event_id=$1`, fifth.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed effect left an Inbox receipt committed", err)
		}
		if _, err := r.pool.Exec(ctx, `ALTER TABLE outbox_events DROP CONSTRAINT fixture_no_membership_update`); err != nil {
			t.Fatal(err)
		}
		if err := consumer.Handle(ctx, fifth); err != nil {
			t.Fatal("rolled-back event must remain retryable", err)
		}
	})
	assertBalanced(t, r)
}

func TestPostgresMemberQuotaConsumerRPCProjectionAhead(t *testing.T) {
	r, _, allocation, consumer, membershipID := memberConsumerFixture(t)
	ctx := context.Background()
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, allocation.OrganizationID, allocation.MemberUserID, "REVOKED", 4); err != nil {
		t.Fatal(err)
	}
	for index, status := range []string{"ACTIVE", "ACTIVE", "SUSPENDED", "REVOKED"} {
		if err := consumer.Handle(ctx, membershipEvent(t, allocation, membershipID, int64(index+1), status)); err != nil {
			t.Fatal("legitimate delayed events must not dead-letter behind an RPC projection", err)
		}
		var storedStatus string
		var version int64
		if err := r.pool.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2`, allocation.OrganizationID, allocation.MemberUserID).Scan(&storedStatus, &version); err != nil || storedStatus != "REVOKED" || version != 4 {
			t.Fatal("delayed event reversed authoritative member state", err)
		}
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, allocation.OrganizationID, allocation.MemberUserID, "ACTIVE", 4); !errors.Is(err, ErrConflict) {
		t.Fatal("same-version opposite status must not revive a member")
	}
}

func TestPostgresMemberQuotaConsumerRejectsUnboundPayload(t *testing.T) {
	r, _, allocation, consumer, membershipID := memberConsumerFixture(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*events.Event){
		"version": func(event *events.Event) { event.AggregateVersion = 2 },
		"unknown": func(event *events.Event) {
			event.Payload = append(event.Payload[:len(event.Payload)-1], []byte(`,"unknown":true}`)...)
		},
		"trailing": func(event *events.Event) { event.Payload = append(event.Payload, []byte(` {}`)...) },
		"status": func(event *events.Event) {
			event.Payload = json.RawMessage(`{"organization_id":"` + allocation.OrganizationID.String() + `","user_id":"` + allocation.MemberUserID.String() + `","membership_status":"INVALID","membership_version":1}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			event := membershipEvent(t, allocation, membershipID, 1, "ACTIVE")
			mutate(&event)
			if !errors.Is(consumer.Handle(ctx, event), events.ErrInvalidEvent) {
				t.Fatal("invalid lifecycle payload must be rejected before Inbox effects")
			}
		})
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatal("malformed event acquired an Inbox receipt", err)
	}
}
