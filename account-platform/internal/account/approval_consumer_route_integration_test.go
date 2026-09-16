package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
)

func TestApprovalConsumerRouteClosedMatrixIntegration(t *testing.T) {
	db, repository, relay, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	if _, err := connection.Exec(ctx, `RESET ROLE; SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	defer connection.Exec(context.Background(), `RESET ROLE`)

	type routeCase struct {
		command, executor, legacy, approval string
		policy                              int64
	}
	for _, item := range []routeCase{
		{"WALLET_ADJUSTMENT", approvalconsumer.BillingWorkload, "billing-command-executor-v1", "billing-wallet-adjustment-executor-p1-v1", 1},
		{"PAYMENT_PROVIDER_RESUME", approvalconsumer.BillingWorkload, "billing-command-executor-v1", "billing-payment-provider-resume-executor-p2-v1", 2},
		{"PAYMENT_PRODUCT_RESUME", approvalconsumer.BillingWorkload, "billing-command-executor-v1", "billing-payment-product-resume-executor-p1-v1", 1},
		{"TOKEN_MODEL_LIFECYCLE_CHANGE", approvalconsumer.TokenWorkload, "token-model-lifecycle-executor-v1", "token-model-lifecycle-executor-v1", 2},
		{"TOKEN_MODEL_CATALOG_ROLLBACK", approvalconsumer.TokenWorkload, "token-catalog-rollback-executor-v1", "token-catalog-rollback-executor-v1", 2},
		{"TOKEN_CREDENTIAL_REVOKE", approvalconsumer.TokenWorkload, "token-credential-revoke-executor-v1", "token-credential-revoke-executor-v1", 1},
	} {
		for wire, want := range map[string]string{"LEGACY_TYPED_V1": item.legacy, "APPROVAL_RFC8785_DECIMAL_V1": item.approval} {
			var got string
			if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable($1,$2,$3,$4)`,
				item.command, item.policy, item.executor, wire).Scan(&got); err != nil || got != want {
				t.Fatalf("route %s/%s got=%s want=%s err=%v", item.command, wire, got, want, err)
			}
		}
	}
	for _, invalid := range [][4]any{
		{"UNKNOWN", int64(1), approvalconsumer.TokenWorkload, "LEGACY_TYPED_V1"},
		{"TOKEN_CREDENTIAL_REVOKE", int64(2), approvalconsumer.TokenWorkload, "LEGACY_TYPED_V1"},
		{"WALLET_ADJUSTMENT", int64(1), approvalconsumer.TokenWorkload, "LEGACY_TYPED_V1"},
		{"WALLET_ADJUSTMENT", int64(1), approvalconsumer.BillingWorkload, "UNKNOWN"},
	} {
		var value *string
		if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable($1,$2,$3,$4)`,
			invalid[0], invalid[1], invalid[2], invalid[3]).Scan(&value); err != nil || value != nil {
			t.Fatalf("invalid route=%v value=%v err=%v", invalid, value, err)
		}
	}

	if _, err := connection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	binding := publishedApprovalDeliveryBinding(t, ctx, db, repository, relay, approvalconsumer.LegacyBilling().Durable)
	for _, caller := range []string{approvalconsumer.BillingWorkload, approvalconsumer.TokenWorkload} {
		route, err := repository.ResolveApprovalAuthorizationRoute(ctx, binding.EventID, caller)
		if err != nil || route.CanonicalWorkload != binding.Workload || route.CanonicalDurable != binding.Durable ||
			route.EventDigest != binding.EventDigest || route.ApprovalRequestID != binding.ApprovalRequestID {
			t.Fatalf("caller=%s route=%+v err=%v", caller, route, err)
		}
	}
	if _, err := repository.ResolveApprovalAuthorizationBinding(ctx, binding.EventID, "token-credential-revoke-executor-v1", approvalconsumer.TokenWorkload); !errors.Is(err, ErrApprovalConsumerBindingInvalid) {
		t.Fatalf("read-only route granted target binding: %v", err)
	}
}

func TestApprovalConsumerRoutePrivilegesIntegration(t *testing.T) {
	db, _, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var internalRuntime, publicRuntime, publicWriter bool
	if err := db.adminPool.QueryRow(ctx, `SELECT
pg_catalog.has_function_privilege('account_service','account_approval_authorization.resolve_authorization_route(uuid)','EXECUTE'),
pg_catalog.has_function_privilege('account_service','public.resolve_approval_authorization_route(uuid,text)','EXECUTE'),
pg_catalog.has_function_privilege('account_approval_writer','public.resolve_approval_authorization_route(uuid,text)','EXECUTE')`).Scan(
		&internalRuntime, &publicRuntime, &publicWriter); err != nil {
		t.Fatal(err)
	}
	if internalRuntime || !publicRuntime || publicWriter {
		t.Fatalf("route privileges internal_runtime=%t public_runtime=%t public_writer=%t", internalRuntime, publicRuntime, publicWriter)
	}
}
