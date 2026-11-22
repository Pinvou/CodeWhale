package portal

import (
	"net/http"
	"strings"
)

func platformRoleAdminPathAllowed(method, resource string) bool {
	if resource == "/admin/v1/roles" {
		return method == http.MethodGet
	}
	if resource == "/admin/v1/role-bindings" {
		return method == http.MethodGet || method == http.MethodPost
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	return len(parts) == 4 && parts[0] == "admin" && parts[1] == "v1" && parts[2] == "role-bindings" && pathUUID(parts[3]) && method == http.MethodDelete
}
