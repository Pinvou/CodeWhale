package portal

import (
	"net/http"
	"strings"
)

func grantRecoveryPathAllowed(method, resource string) bool {
	const prefix = "/admin/v1/approval-grant-recovery-cases"
	if resource == prefix {
		return method == http.MethodGet || method == http.MethodHead
	}
	if !strings.HasPrefix(resource, prefix+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(resource, prefix+"/"), "/")
	if len(parts) < 1 || !pathUUID(parts[0]) {
		return false
	}
	if len(parts) == 1 {
		return method == http.MethodGet || method == http.MethodHead
	}
	if len(parts) == 2 && method == http.MethodPost {
		switch parts[1] {
		case "observations", "proposals", "reviews":
			return true
		}
	}
	return false
}
