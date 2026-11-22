package billing

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Uses the actual public migrations and the existing isolated Billing runtime.
// No financial rows or historical migration identities are patched by this fixture.
func TestMemberQuotaBatchPersistedFactsAndFinancialReplay(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, packageID := retainedOrganizationFinanceOrder(t, r, ctx, 1000000, true)
	org, actor, member := *order.BeneficiaryOrganizationID, uuid.New(), uuid.New()
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	var componentID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, packageID).Scan(&componentID); err != nil {
		t.Fatal(err)
	}
	first := memberQuotaBatchItem{MemberUserID: member, PackageID: packageID, ComponentID: componentID, ComponentType: "BASE", AmountMicrocredits: 40, MembershipVersion: 1}
	second := first
	second.AmountMicrocredits = 60
	items := []memberQuotaBatchItem{first, second}
	key := uuid.NewString()
	if err := r.FreezeMemberQuotaBatch(ctx, org, actor, key, items); err != nil {
		t.Fatal(err)
	}
	grantIDs := make([]uuid.UUID, 2)
	allocate := func(index int, item memberQuotaBatchItem) (MemberQuotaGrant, bool, error) {
		return r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: org, MemberUserID: item.MemberUserID, PackageID: item.PackageID, ComponentID: item.ComponentID, ComponentType: item.ComponentType, AmountMicrocredits: item.AmountMicrocredits, MembershipVersion: item.MembershipVersion, CreatedBy: actor, IdempotencyKey: deriveMemberQuotaChildKey(key, index, item)})
	}
	for index, item := range items {
		grant, created, err := allocate(index, item)
		if err != nil || !created {
			t.Fatal(grant, created, err)
		}
		grantIDs[index] = grant.ID
	}
	if grantIDs[0] == grantIDs[1] {
		t.Fatal("legitimate separate indexes collapsed")
	}
	if err := r.FreezeMemberQuotaBatch(ctx, org, actor, key, items); err != nil {
		t.Fatal(err)
	}
	for index, item := range items {
		grant, created, err := allocate(index, item)
		if err != nil || created || grant.ID != grantIDs[index] {
			t.Fatal(grant, created, err)
		}
	}
	for _, test := range []struct {
		name  string
		actor uuid.UUID
		items []memberQuotaBatchItem
	}{
		{"reorder", actor, []memberQuotaBatchItem{second, first}},
		{"subset", actor, items[1:]},
		{"append", actor, append(append([]memberQuotaBatchItem{}, items...), first)},
		{"different-actor", uuid.New(), items},
		{"different-amount", actor, []memberQuotaBatchItem{second, second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := r.FreezeMemberQuotaBatch(ctx, org, test.actor, key, test.items); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatal("changed parent accepted", err)
			}
		})
	}
	var count int
	var available, allocated int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grants WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if err := r.pool.QueryRow(ctx, `SELECT available_microcredits,active_grant_microcredits FROM entitlement_components WHERE id=$1`, componentID).Scan(&available, &allocated); err != nil || available != 999900 || allocated != 100 {
		t.Fatal(available, allocated, err)
	}
	for _, different := range []bool{false, true} {
		name := "concurrent-identical-facts"
		if different {
			name = "concurrent-conflicting-facts"
		}
		t.Run(name, func(t *testing.T) {
			concurrentKey := uuid.NewString()
			other := append([]memberQuotaBatchItem{}, items...)
			if different {
				other[0].AmountMicrocredits++
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			for _, facts := range [][]memberQuotaBatchItem{items, other} {
				go func(facts []memberQuotaBatchItem) {
					<-start
					results <- r.FreezeMemberQuotaBatch(ctx, org, actor, concurrentKey, facts)
				}(facts)
			}
			close(start)
			first, second := <-results, <-results
			if different {
				if !((first == nil && errors.Is(second, ErrIdempotencyConflict)) || (second == nil && errors.Is(first, ErrIdempotencyConflict))) {
					t.Fatal(first, second)
				}
			} else if first != nil || second != nil {
				t.Fatal(first, second)
			}
			var parents int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_batch_commands WHERE organization_id=$1 AND idempotency_key=$2`, org, concurrentKey).Scan(&parents); err != nil || parents != 1 {
				t.Fatal(parents, err)
			}
		})
	}
	t.Run("original-key-length-contract-counts-utf8-bytes", func(t *testing.T) {
		// Existing HTTP and child contracts use Go len(key), not rune count.
		// A valid 18-byte key must not become an unknown SQL CHECK failure.
		unicodeKey := strings.Repeat("批", 6)
		if err := r.FreezeMemberQuotaBatch(ctx, org, actor, unicodeKey, items); err != nil {
			t.Fatal(err)
		}
		if err := r.FreezeMemberQuotaBatch(ctx, org, actor, unicodeKey, items); err != nil {
			t.Fatal(err)
		}
	})
	for _, query := range []string{
		`UPDATE member_quota_batch_commands SET request_fingerprint=repeat('0',64)`,
		`DELETE FROM member_quota_batch_commands`,
		`TRUNCATE member_quota_batch_commands`,
	} {
		_, err := r.pool.Exec(ctx, query)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || (databaseError.Code != "55000" && databaseError.Code != "42501") {
			t.Fatal("runtime immutable denial was absent or unrelated", query, err)
		}
	}
	if err := r.FreezeMemberQuotaBatch(ctx, org, actor, key, items); err != nil {
		t.Fatal("original facts lost", err)
	}
}
