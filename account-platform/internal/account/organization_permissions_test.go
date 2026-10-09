package account

import (
	"slices"
	"testing"
)

func TestOrganizationDeliveredFinancePermissions(t *testing.T) {
	delivered := []string{"org.wallet.read", "org.entitlements.purchase", "org.quota.read.self", "org.quota.read.all", "org.quota.revoke", "org.roles.read"}
	for _, permission := range delivered {
		t.Run(permission, func(t *testing.T) {
			if !f01PermissionAllowed(permission) {
				t.Fatal("delivered organization route permission unavailable")
			}
			if !slices.Contains(filterF01Permissions([]string{permission}), permission) {
				t.Fatal("current context loses delivered authority")
			}
		})
	}
	for _, permission := range []string{"org.audit.export", "org.grants.reclaim_closed", "personal.account.close.request", "personal.account.close.read", "platform.account-closures.repair"} {
		if f01PermissionAllowed(permission) {
			t.Fatal("undelivered authority advertised")
		}
	}
}
