package account

// Only permissions whose complete runtime feature is installed can be
// advertised or authorized. Later immutable storage seeds confer no authority.
func f01PermissionAllowed(permission string) bool {
	switch permission {
	case PermissionPlatformAuditExport, PermissionPlatformAuditPIIExport, "personal.account.close.read", "personal.account.close.request", "platform.account-closures.read", "platform.account-closures.repair", "org.grants.reclaim_closed":
		return true
	case "platform.tokens.read", "platform.tokens.manage", "platform.tokens.credentials.validate", "platform.tokens.owner-candidates.read", "platform.tokens.credentials.revoke.request", "platform.tokens.credentials.revoke.approve", "platform.models.read", "platform.models.sync", "platform.models.manage", "platform.models.lifecycle.request", "platform.models.lifecycle.approve", "platform.models.catalog.rollback.request", "platform.models.catalog.rollback.approve", "platform.pricing.read", "platform.pricing.manage", "platform.audit.read", "platform.system.status.read":
		return true
	case PermissionPersonalSessionLogin, PermissionPersonalProfileRead, PermissionPersonalProfileUpdate, PermissionPersonalSecurityManage, PermissionPersonalBillingRead,
		PermissionPlatformUsersRead, PermissionPlatformUsersStatusManage, PermissionPlatformRegistrationsRepair, PermissionPlatformProfileChangesRepair, PermissionPlatformSMSConfigRead, PermissionPlatformSMSConfigManage:
		return true
	case PermissionPlatformOrganizationsRead, PermissionPlatformOrganizationsCreate, PermissionPlatformOrganizationsProfileManage, PermissionPlatformOrganizationsStatusManage, PermissionPlatformOrganizationsMembersManage, PermissionPlatformOrganizationsUnitsManage, PermissionPlatformOrganizationsDelete, PermissionPlatformOrganizationsOwnerTransfer, PermissionPersonalContextSwitch, PermissionPersonalInvitationsRead, PermissionPersonalInvitationsAccept, PermissionOrganizationProfileRead, PermissionOrganizationUnitsRead, PermissionOrganizationUnitsManage, PermissionOrganizationMembersRead, PermissionOrganizationMembersManage, PermissionOrganizationRolesAssign, PermissionOrganizationOwnerTransfer, PermissionOrganizationQuotaAllocate:
		return true
	case "org.model.invoke", "org.models.read", "org.models.manage", "org.usage.read.self", "org.usage.read.all", "org.wallet.read", "org.entitlements.purchase", "org.quota.read.self", "org.quota.read.all", "org.quota.revoke", "org.roles.read":
		return true
	case "platform.usage.read", "platform.usage.reconcile.retry", "platform.billing.reconciliation.read", "platform.billing.reconciliation.run", "platform.billing.reconciliation.manage":
		return true
	case "personal.models.read", "personal.model.invoke", "personal.usage.read", "personal.usage.cancel":
		return true
	case "personal.recharge.create", "personal.recharge.manage":
		return true
	case "platform.billing.adjustments.approve", "platform.billing.ledger.read", "platform.billing.orders.sync", "platform.billing.payment-controls.resume.approve", "platform.billing.payment-controls.resume.request", "platform.billing.repairs.approve", "platform.billing.repairs.request", "platform.billing.wallets.read", "platform.products.create", "platform.products.delete", "platform.products.publish", "platform.products.read", "platform.products.unpublish":
		return true
	}
	return false
}

func filterF01Permissions(permissions []string) []string {
	result := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		if f01PermissionAllowed(permission) {
			result = append(result, permission)
		}
	}
	return result
}

func filterMePermissions(me *Me) {
	me.ActiveContext.Permissions = filterF01Permissions(me.ActiveContext.Permissions)
	me.PersonalPermissions = filterF01Permissions(me.PersonalPermissions)
	me.PlatformPermissions = filterF01Permissions(me.PlatformPermissions)
}
