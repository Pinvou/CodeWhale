package account

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestF09BillingRuntimeActivationDirectLockDenied(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned Billing fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, pool, _ := newEntitlementCrossDomainBillingFixture(t, ctx, raw)
	var actor string
	var update, owner bool
	if err := pool.QueryRow(ctx, `SELECT current_user,has_table_privilege(current_user,'public.billing_approval_command_activations','UPDATE'),pg_has_role(current_user,'billing_schema_owner','MEMBER')`).Scan(&actor, &update, &owner); err != nil || actor != "billing_service" || update || owner {
		t.Fatal("runtime activation least privilege differs")
	}
	err := pool.QueryRow(ctx, `SELECT state='ACTIVE' FROM public.billing_approval_command_activations WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2 FOR SHARE`).Scan(&update)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "42501" {
		t.Fatal("runtime direct activation row lock must be denied with 42501")
	}
	t.Log("actual billing_service direct row lock rejected SQLSTATE42501; runtime has no activation UPDATE or owner membership")
}
