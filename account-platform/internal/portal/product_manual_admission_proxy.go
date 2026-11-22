package portal

import (
	"net/http"
	"strings"
)

func manualProductAdmissionPathAllowed(method, resource string) bool {
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 5 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "recharge-products" || !pathUUID(parts[3]) || parts[4] != "manual-admission-submissions" {
		return false
	}
	if len(parts) == 5 {
		return method == http.MethodPost
	}
	if !pathUUID(parts[5]) {
		return false
	}
	return len(parts) == 6 && (method == http.MethodGet || method == http.MethodHead) || len(parts) == 7 && parts[6] == "decisions" && method == http.MethodPost
}
