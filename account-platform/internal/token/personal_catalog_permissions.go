package token

func hasPermission(resolved AuthorizationContext, permission string) bool {
	for _, candidate := range resolved.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}
