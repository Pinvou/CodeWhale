package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresMemberQuotaAuthorityRequiresCompleteTuple(t *testing.T) {
	r, wallet, request := quotaFixture(t, false)
	ctx := context.Background()
	hold, err := r.Reserve(ctx, uuid.New(), wallet.ID, 10, nil, r.now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.pool.Exec(ctx, `UPDATE usage_holds SET quota_organization_id=$2,quota_member_user_id=$3,quota_membership_version=NULL WHERE id=$1`, hold.ID, request.OrganizationID, request.MemberUserID)
	var violation *pgconn.PgError
	if !errors.As(err, &violation) || violation.Code != "23514" || violation.ConstraintName != "usage_holds_member_authority" {
		t.Fatal("incomplete member authority must be rejected by its database constraint")
	}
	if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET quota_organization_id=$2,quota_member_user_id=$3,quota_membership_version=1 WHERE id=$1`, hold.ID, request.OrganizationID, request.MemberUserID); err != nil {
		t.Fatal("complete member authority must remain valid", err)
	}
}

func TestPostgresMemberQuotaOrganizationReadPreservesNoExpiryPool(t *testing.T) {
	r, wallet, request := quotaFixture(t, false)
	items, summary, err := r.ListOrganizationPackages(context.Background(), request.OrganizationID, wallet.ID, 100)
	if err != nil || len(items) != 1 || items[0].ValidUntil != nil || len(items[0].Components) != 1 || items[0].Components[0].ExpiresAt != nil || summary.AvailableMicrocredits != "1000" || summary.NextExpiryAt != nil {
		t.Fatal("organization pool must preserve and summarize non-expiring inherited credit", err)
	}
	if _, _, err := r.ListOrganizationPackages(context.Background(), uuid.New(), wallet.ID, 100); !errors.Is(err, ErrConflict) {
		t.Fatal("another organization cannot read a wallet's packages")
	}
}

func TestPostgresMemberQuotaOrganizationReadKeepsA08PayerBoundPackages(t *testing.T) {
	r, wallet, request := quotaFixture(t, false)
	ctx := context.Background()
	// A08 issuePackageWithinTransaction stored the paying requester's user_id
	// even for an ORGANIZATION wallet. Preserve that legal historical projection.
	if _, err := r.pool.Exec(ctx, `UPDATE entitlement_packages SET user_id=$2 WHERE id=$1`, request.PackageID, request.MemberUserID); err != nil {
		t.Fatal(err)
	}
	items, summary, err := r.ListOrganizationPackages(ctx, request.OrganizationID, wallet.ID, 100)
	if err != nil || len(items) != 1 || items[0].ValidUntil != nil || summary.AvailableMicrocredits != "1000" {
		t.Fatal("old organization credit was hidden", err)
	}
	if _, _, err := r.AllocateMemberQuota(ctx, request); err != nil {
		t.Fatal("old organization credit cannot be allocated", err)
	}
	if _, _, err := r.ListOrganizationPackages(ctx, uuid.New(), wallet.ID, 100); !errors.Is(err, ErrConflict) {
		t.Fatal("wallet scope was weakened")
	}
	assertBalanced(t, r)
}

func TestPostgresMemberQuotaExpiredRestoreConservesFinancialExpiry(t *testing.T) {
	r, wallet, request := quotaFixture(t, true)
	ctx := context.Background()
	if _, _, err := r.AllocateMemberQuota(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, request.OrganizationID, request.MemberUserID, "REVOKED", 2); err != nil {
		t.Fatal(err)
	}
	expiry := r.now().Add(time.Hour)
	for _, query := range []string{`UPDATE entitlement_packages SET valid_until=$2 WHERE id=$1`, `UPDATE entitlement_components SET expires_at=$2 WHERE package_id=$1`, `UPDATE member_quota_grants SET expires_at=$2 WHERE package_id=$1`} {
		if _, err := r.pool.Exec(ctx, query, request.PackageID, expiry); err != nil {
			t.Fatal(err)
		}
	}
	r.now = func() time.Time { return expiry.Add(time.Minute) }
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, request.OrganizationID, request.MemberUserID, "ACTIVE", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExpirePackages(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stored, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || stored.PostedMicrocredits != 0 || stored.ReservedMicrocredits != 0 {
		t.Fatal("membership restoration must not skip authoritative wallet/lot expiry", err)
	}
	assertBalanced(t, r)
}

func quotaFixture(t *testing.T, finite bool) (*PostgresRepository, Wallet, MemberQuotaAllocation) {
	t.Helper()
	ctx := context.Background()
	r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	org, member := uuid.New(), uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Credit(ctx, wallet.ID, "owned-quota-fixture", 1000, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, org, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	request := MemberQuotaAllocation{OrganizationID: org, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 800, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()}
	if err := r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1`, wallet.ID).Scan(&request.ComponentID, &request.PackageID); err != nil {
		t.Fatal(err)
	}
	if finite {
		if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET expires_at=$2 WHERE id=$1`, request.ComponentID, r.now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	return r, wallet, request
}

func TestPostgresMemberQuotaPreservesNoExpiryAndExactWire(t *testing.T) {
	r, _, request := quotaFixture(t, false)
	grant, _, err := r.AllocateMemberQuota(context.Background(), request)
	if err != nil {
		t.Fatal("non-expiring inherited credit must remain allocatable", err)
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["expires_at"] != nil {
		t.Fatal("inherited credit must not acquire an invented expiry")
	}
	for _, field := range []string{"original_microcredits", "available_microcredits", "reserved_microcredits", "used_microcredits", "expired_microcredits", "revoked_microcredits"} {
		if _, ok := wire[field].(string); !ok {
			t.Fatalf("%s must use the exact decimal string read contract", field)
		}
	}
}

func TestPostgresMemberQuotaActiveVersionRefresh(t *testing.T) {
	r, wallet, request := quotaFixture(t, true)
	ctx := context.Background()
	if _, _, err := r.AllocateMemberQuota(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, request.OrganizationID, request.MemberUserID, "ACTIVE", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, request.OrganizationID, request.MemberUserID, 2, 100, nil, r.now().Add(time.Minute)); err != nil {
		t.Fatal("active membership version refresh stranded valid existing grant", err)
	}
}

func TestPostgresMemberQuotaReservationReplayBindsMember(t *testing.T) {
	r, wallet, request := quotaFixture(t, true)
	ctx := context.Background()
	if _, _, err := r.AllocateMemberQuota(ctx, request); err != nil {
		t.Fatal(err)
	}
	usageID := uuid.New()
	if _, err := r.ReserveMemberQuota(ctx, usageID, wallet.ID, request.OrganizationID, request.MemberUserID, 1, 100, nil, r.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReserveMemberQuota(ctx, usageID, wallet.ID, request.OrganizationID, uuid.New(), 1, 100, nil, r.now().Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatal("same usage cannot be reassigned to another member")
	}
}

func TestMemberQuotaBatchChangedFactsKeepOriginalCommandIdentity(t *testing.T) {
	item := memberQuotaBatchItem{MemberUserID: uuid.New(), PackageID: uuid.New(), ComponentID: uuid.New()}
	key := deriveMemberQuotaChildKey("owned-fixed-batch-command", 0, item)
	item.MemberUserID = uuid.New()
	if key != deriveMemberQuotaChildKey("owned-fixed-batch-command", 0, item) {
		t.Fatal("changed target must conflict with original child facts, not silently create a new command")
	}
}

func TestPostgresMemberQuotaWholeGrantCommitRetainsRemainingCapacity(t *testing.T) {
	r, wallet, request := quotaFixture(t, false)
	ctx := context.Background()
	request.AmountMicrocredits = 1000
	grant, _, err := r.AllocateMemberQuota(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, request.OrganizationID, request.MemberUserID, 1, 400, nil, r.now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, hold.UsageRequestID, hold.ID, 100, map[string]any{"input_tokens": 1}); err != nil {
		t.Fatal(err)
	}
	items, err := r.ListMemberQuotaGrants(ctx, request.OrganizationID, &request.MemberUserID, 100)
	if err != nil || len(items) != 1 || items[0].ID != grant.ID || items[0].AvailableMicrocredits != 900 || items[0].UsedMicrocredits != 100 || items[0].ReservedMicrocredits != 0 {
		t.Fatal("settlement failed to conserve member capacity", err)
	}
	stored, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || stored.PostedMicrocredits != 900 || stored.ReservedMicrocredits != 0 {
		t.Fatal("member wallet settlement mismatch", err)
	}
	next, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, request.OrganizationID, request.MemberUserID, 1, 900, nil, r.now().Add(time.Minute))
	if err != nil {
		t.Fatal("remaining grant must keep its package consumable", err)
	}
	if _, err := r.Release(ctx, next.UsageRequestID, next.ID, "upstream_not_started"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeMemberQuota(ctx, request.OrganizationID, grant.ID, request.CreatedBy, "return unconsumed capacity", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var available, active, suspended, used int64
	if err := r.pool.QueryRow(ctx, `SELECT available_microcredits,active_grant_microcredits,suspended_grant_microcredits,used_microcredits FROM entitlement_components WHERE id=$1`, request.ComponentID).Scan(&available, &active, &suspended, &used); err != nil || available != 900 || active != 0 || suspended != 0 || used != 100 {
		t.Fatal("revocation must preserve original component capacity", err)
	}
	assertBalanced(t, r)
}

func TestPostgresMemberQuotaConcurrentSameUsageHasOneReservation(t *testing.T) {
	r, wallet, request := quotaFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, _, err := r.AllocateMemberQuota(ctx, request); err != nil {
		t.Fatal(err)
	}
	usageID := uuid.New()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan Hold, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			hold, err := r.ReserveMemberQuota(ctx, usageID, wallet.ID, request.OrganizationID, request.MemberUserID, 1, 100, nil, r.now().Add(time.Minute))
			results <- hold
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal("concurrent replay must retain one financial winner", err)
		}
	}
	var winner uuid.UUID
	for hold := range results {
		if winner == uuid.Nil {
			winner = hold.ID
		}
		if hold.ID != winner {
			t.Fatal("same usage acquired multiple holds")
		}
	}
	stored, err := r.GetWallet(ctx, wallet.ID)
	if err != nil || stored.ReservedMicrocredits != 100 {
		t.Fatal("replay reserved the wallet twice", err)
	}
}

func TestPostgresMemberQuotaLifecycleAndFrozenWalletBoundaries(t *testing.T) {
	r, wallet, request := quotaFixture(t, false)
	ctx := context.Background()
	grant, _, err := r.AllocateMemberQuota(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, request.OrganizationID, request.MemberUserID, 1, 300, nil, r.now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, request.OrganizationID, request.MemberUserID, "REVOKED", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Release(ctx, hold.UsageRequestID, hold.ID, "upstream_not_started"); err != nil {
		t.Fatal(err)
	}
	items, err := r.ListMemberQuotaGrants(ctx, request.OrganizationID, nil, 100)
	if err != nil || len(items) != 1 || items[0].Status != "SUSPENDED" || items[0].AvailableMicrocredits != 800 || items[0].ReservedMicrocredits != 0 {
		t.Fatal("late release must retain revoked member suspension", err)
	}
	if _, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, request.OrganizationID, request.MemberUserID, 2, 1, nil, r.now().Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatal("revoked member cannot consume suspended capacity")
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, request.OrganizationID, request.MemberUserID, "ACTIVE", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeMemberQuota(ctx, request.OrganizationID, grant.ID, request.CreatedBy, "return capacity", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, request.OrganizationID, request.MemberUserID, "ACTIVE", 4); err != nil {
		t.Fatal(err)
	}
	items, err = r.ListMemberQuotaGrants(ctx, request.OrganizationID, nil, 100)
	if err != nil || items[0].Status != "REVOKED" {
		t.Fatal("membership restore must not revive an explicitly revoked grant", err)
	}
	if _, err := r.ApplyOrganizationLifecycle(ctx, request.OrganizationID, "DISABLED", 1, false); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = uuid.NewString()
	if _, _, err := r.AllocateMemberQuota(ctx, request); !errors.Is(err, ErrWalletUnavailable) {
		t.Fatal("new quota allocation cannot bypass frozen wallet")
	}
	assertBalanced(t, r)
}

func TestPostgresMemberQuotaRevokeReasonCharacterBoundary(t *testing.T) {
	r, _, request := quotaFixture(t, false)
	ctx := context.Background()
	grant, _, err := r.AllocateMemberQuota(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.RevokeMemberQuota(ctx, request.OrganizationID, grant.ID, request.CreatedBy, strings.Repeat("\u539f", 501), uuid.NewString()); !errors.Is(err, ErrConflict) {
		t.Fatal("overlong reason reached mutation", err)
	}
	stored, err := r.ListMemberQuotaGrants(ctx, request.OrganizationID, nil, 100)
	if err != nil || len(stored) != 1 || stored[0].Status != "ACTIVE" {
		t.Fatal("invalid revocation changed grant", err)
	}
	if _, err = r.RevokeMemberQuota(ctx, request.OrganizationID, grant.ID, request.CreatedBy, strings.Repeat("\u539f", 500), uuid.NewString()); err != nil {
		t.Fatal("500 characters rejected", err)
	}
	assertBalanced(t, r)
}
