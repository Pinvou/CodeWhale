package billing

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

// Prepare the exact production SQL without executing refund mutations. This
// checks PostgreSQL grammar and real column types independently of the pending
// ordinary-refund policy decision.
func TestPostgresMemberGrantQueriesPrepare(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Open(t, "billing")
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	literals := regexp.MustCompile("(?s)`([^`]+)`")
	alias := regexp.MustCompile(`\bmember_quota_grants (grant|member_grant)\b`)
	matched := 0
	for _, name := range []string{"admin.go", "member_quota_holds.go", "refunds.go"} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, literal := range literals.FindAllStringSubmatch(string(source), -1) {
			if !alias.MatchString(literal[1]) {
				continue
			}
			matched++
			t.Run(fmt.Sprintf("%s-%d", name, matched), func(t *testing.T) {
				if _, err := connection.Conn().Prepare(ctx, fmt.Sprintf("controlled_member_query_%d", matched), literal[1]); err != nil {
					t.Fatal("production query cannot prepare", err)
				}
			})
		}
	}
	if matched == 0 {
		t.Fatal("production SQL was not exercised")
	}
}

func TestPostgresMemberHoldDetailAndExtensionExecute(t *testing.T) {
	r, wallet, transaction := forcedRefundPaidSource(t)
	ctx, member := context.Background(), uuid.New()
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, wallet.OwnerID, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	request := MemberQuotaAllocation{OrganizationID: wallet.OwnerID, MemberUserID: member, ComponentType: "BASE", AmountMicrocredits: 500000, MembershipVersion: 1, CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()}
	if err := r.pool.QueryRow(ctx, `SELECT component.id,package.id FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.source_payment_transaction_id=$1 AND component.component_type='BASE'`, transaction).Scan(&request.ComponentID, &request.PackageID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AllocateMemberQuota(ctx, request); err != nil {
		t.Fatal(err)
	}
	hold, err := r.ReserveMemberQuota(ctx, uuid.New(), wallet.ID, wallet.OwnerID, member, 1, 100, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("hold-detail", func(t *testing.T) {
		detail, err := r.AdminUsageHold(ctx, hold.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Hold.ID != hold.ID {
			t.Fatal("wrong hold detail")
		}
	})
	t.Run("member-extension", func(t *testing.T) {
		extended, err := r.ExtendHold(ctx, hold.UsageRequestID, hold.ID, time.Now().Add(2*time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		if extended.MaxMicrocredits != 200 {
			t.Fatal("extension omitted its quota amount")
		}
	})
	// No refund command is executed by the grammar checks above.
	var refunds int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM refunds`).Scan(&refunds); err != nil || refunds != 0 {
		t.Fatal("unexpected refund mutation", err)
	}
}
