package portal

import (
	"net/http"
	"strings"
)

// Only delivered organization operations cross the session/CSRF proxy. Model
// invocation is owned by the preceding phase; gifts and personal closure do not
// become reachable by sharing an organization path prefix.
func organizationAccountPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet && (resource == "/v1/me/organizations" || resource == "/v1/me/invitations" || resource == "/v1/me/role-bindings") {
		return true
	}
	if method == http.MethodPost && (resource == "/v1/sessions/current/active-context" || resource == "/v1/invitations/accept") {
		return true
	}
	p := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(p) < 4 || p[0] != "v1" || p[1] != "organizations" || !pathUUID(p[2]) {
		return false
	}
	if len(p) == 4 {
		if method == http.MethodGet {
			return p[3] == "units" || p[3] == "members" || p[3] == "quota-members" || p[3] == "role-bindings"
		}
		return method == http.MethodPost && (p[3] == "units" || p[3] == "invitations" || p[3] == "role-bindings" || p[3] == "ownership")
	}
	if len(p) == 5 && pathUUID(p[4]) {
		return method == http.MethodDelete && (p[3] == "invitations" || p[3] == "role-bindings" || p[3] == "members")
	}
	return len(p) == 6 && pathUUID(p[4]) && (method == http.MethodPut && p[3] == "members" && (p[5] == "roles" || p[5] == "units") || method == http.MethodPatch && p[3] == "units" && p[5] == "parent")
}

func organizationTokenPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	p := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(p) < 4 || p[0] != "v1" || p[1] != "organizations" || !pathUUID(p[2]) {
		return false
	}
	if len(p) == 4 {
		return method == http.MethodGet && (p[3] == "model-policy" || p[3] == "balance" || p[3] == "entitlement-packages" || p[3] == "quota-grants" || p[3] == "payment-capabilities") || method == http.MethodPost && p[3] == "quota-grants:batch" || method == http.MethodPut && p[3] == "model-policy"
	}
	if len(p) == 5 {
		return p[3] == "recharge" && (method == http.MethodPost && p[4] == "orders" || method == http.MethodGet && p[4] == "products")
	}
	return len(p) == 6 && (method == http.MethodGet && p[3] == "recharge" && p[4] == "orders" && pathUUID(p[5]) || method == http.MethodPost && p[3] == "quota-grants" && pathUUID(p[4]) && p[5] == "revoke")
}

func organizationAdminPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if resource == "/admin/v1/organizations" {
		return method == http.MethodGet || method == http.MethodPost
	}
	p := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(p) == 5 && p[0] == "admin" && p[1] == "v1" && p[2] == "users" && pathUUID(p[3]) && p[4] == "memberships" {
		return method == http.MethodGet
	}
	if len(p) < 4 || p[0] != "admin" || p[1] != "v1" || p[2] != "organizations" || !pathUUID(p[3]) {
		return false
	}
	if len(p) == 4 {
		return method == http.MethodPatch || method == http.MethodDelete
	}
	if len(p) == 5 {
		if method == http.MethodGet {
			return p[4] == "deletion-readiness" || p[4] == "units" || p[4] == "members"
		}
		return method == http.MethodPatch && p[4] == "status" || method == http.MethodPost && (p[4] == "units" || p[4] == "members" || p[4] == "ownership")
	}
	if len(p) == 6 && pathUUID(p[5]) {
		return method == http.MethodDelete && (p[4] == "members" || p[4] == "units") || method == http.MethodPatch && p[4] == "units"
	}
	return len(p) == 7 && pathUUID(p[5]) && (method == http.MethodPut && p[4] == "members" && (p[6] == "roles" || p[6] == "units") || method == http.MethodPatch && p[4] == "units" && p[6] == "parent")
}
