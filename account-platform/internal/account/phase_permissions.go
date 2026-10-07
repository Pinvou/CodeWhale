package account

// Only permissions whose complete runtime feature is delivered by F01 can be
// advertised or authorized. Later immutable storage seeds confer no authority.
func f01PermissionAllowed(permission string) bool {
	switch permission {
	case PermissionPersonalSessionLogin, PermissionPersonalProfileRead, PermissionPersonalProfileUpdate, PermissionPersonalSecurityManage, PermissionPersonalBillingRead,
		PermissionPlatformUsersRead, PermissionPlatformUsersStatusManage, PermissionPlatformRegistrationsRepair, PermissionPlatformProfileChangesRepair, PermissionPlatformSMSConfigRead, PermissionPlatformSMSConfigManage:
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
