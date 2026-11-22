package billing

import (
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestUsageWaiverMemberReservationPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	for _, state := range []string{"active", "member-removed", "expired-before-expiry-worker"} {
		for _, status := range []string{"ACTIVE", "REVIEW"} {
			for _, maximum := range []int64{0, 40, 100} {
				t.Run(fmt.Sprintf("%s-%s-%d", state, status, maximum), func(t *testing.T) {
					clock := time.Now().UTC()
					r.now = func() time.Time { return clock }
					organization, member := uuid.New(), uuid.New()
					if _, err := r.ApplyOrganizationLifecycle(ctx, organization, "ACTIVE", 1, false); err != nil {
						t.Fatal(err)
					}
					if _, err := r.ApplyOrganizationMemberLifecycle(ctx, organization, member, "ACTIVE", 1); err != nil {
						t.Fatal(err)
					}
					wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", organization)
					if err != nil {
						t.Fatal(err)
					}
					reference := "waiver-member:" + organization.String()
					if _, err := r.Credit(ctx, wallet.ID, reference, 100, true); err != nil {
						t.Fatal(err)
					}
					var packageID, componentID uuid.UUID
					if err := r.pool.QueryRow(ctx, `SELECT package.id,component.id FROM entitlement_packages package JOIN entitlement_components component ON component.package_id=package.id WHERE package.wallet_id=$1 AND package.source_reference=$2`, wallet.ID, reference).Scan(&packageID, &componentID); err != nil {
						t.Fatal(err)
					}
					grant, _, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: organization, MemberUserID: member, PackageID: packageID, ComponentID: componentID, ComponentType: "BASE", AmountMicrocredits: 100, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
					if err != nil {
						t.Fatal(err)
					}
					hold, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, organization, member, 1, maximum, nil, clock.Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET status=$2 WHERE id=$1`, hold.ID, status); err != nil {
						t.Fatal(err)
					}
					command, _, _ := waiverFixtureCommand(t, r, ctx, hold)
					if state == "member-removed" {
						if _, err := r.ApplyOrganizationMemberLifecycle(ctx, organization, member, "REVOKED", 2); err != nil {
							t.Fatal(err)
						}
					}
					if state == "expired-before-expiry-worker" {
						clock = grant.ExpiresAt.Add(time.Minute)
					}
					result, err := r.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: command.Reference, Digest: command.Digest})
					if err != nil || !result.Matches(command) || result.Outcome != usagewaiver.Waived {
						t.Fatal("member waiver failed", result, err)
					}
					var grantStatus string
					var reason *string
					var available, reserved, used, activeBucket, suspendedBucket, componentReserved int64
					if err := r.pool.QueryRow(ctx, `SELECT quota.status,quota.suspension_reason,quota.available_microcredits,quota.reserved_microcredits,quota.used_microcredits,component.active_grant_microcredits,component.suspended_grant_microcredits,component.reserved_microcredits FROM member_quota_grants quota JOIN entitlement_components component ON component.id=quota.component_id WHERE quota.id=$1`, grant.ID).Scan(&grantStatus, &reason, &available, &reserved, &used, &activeBucket, &suspendedBucket, &componentReserved); err != nil {
						t.Fatal(err)
					}
					expectedActive, expectedSuspended := int64(100), int64(0)
					if state == "member-removed" {
						expectedActive, expectedSuspended = 0, 100
						if grantStatus != "SUSPENDED" || reason == nil || *reason != "MEMBER_REMOVED" {
							t.Fatal("removed grant reactivated", grantStatus, reason)
						}
					} else if grantStatus != "ACTIVE" || reason != nil {
						t.Fatal("grant lifecycle changed", grantStatus, reason)
					}
					if available != 100 || reserved != 0 || used != 0 || activeBucket != expectedActive || suspendedBucket != expectedSuspended || componentReserved != 0 {
						t.Fatal("member quota not restored to original current bucket", available, reserved, used, activeBucket, suspendedBucket, componentReserved)
					}
					got, err := r.GetWallet(ctx, wallet.ID)
					if err != nil || got.PostedMicrocredits != 100 || got.ReservedMicrocredits != 0 {
						t.Fatal("organization wallet moved", got, err)
					}
					var lifecycle string
					var version int64
					if err := r.pool.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2`, organization, member).Scan(&lifecycle, &version); err != nil || state == "member-removed" && (lifecycle != "REVOKED" || version != 2) {
						t.Fatal("member identity restored", lifecycle, version, err)
					}
					if state == "expired-before-expiry-worker" {
						if _, err := r.ExpirePackages(ctx, 100); err != nil {
							t.Fatal("normal subsequent expiry", err)
						}
						var expired int64
						if err := r.pool.QueryRow(ctx, `SELECT status,available_microcredits,expired_microcredits FROM member_quota_grants WHERE id=$1`, grant.ID).Scan(&grantStatus, &available, &expired); err != nil || grantStatus != "EXPIRED" || available != 0 || expired != 100 {
							t.Fatal("expired grant became spendable", grantStatus, available, expired, err)
						}
					}
				})
			}
		}
	}
}
