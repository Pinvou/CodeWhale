package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func platformRoleHTTP(h http.Handler, token, method, path string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	q := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, q)
	return out
}
func TestPlatformRoleManagementRuntimePostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	ctx := context.Background()
	t.Run("actual-http-invalid-json-is-explicit-no-write", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "invalid-json-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "invalid-json-target")
		h := userCodeHandler(r, map[string]identity.Principal{"admin": admin, "target": entitlementApprovalPrincipal(target)})
		marker := "controlled-private-input-" + uuid.NewString()
		key := uuid.NewString()
		valid, err := json.Marshal(map[string]any{"user_id": target.ID, "role_code": "PLATFORM_MODEL_ADMIN", "idempotency_key": key, "reason": marker})
		if err != nil {
			t.Fatal(err)
		}
		request := func(token, raw string) *httptest.ResponseRecorder {
			q := httptest.NewRequest(http.MethodPost, "/admin/v1/role-bindings", strings.NewReader(raw))
			if token != "" {
				q.Header.Set("Authorization", "Bearer "+token)
			}
			q.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, q)
			return out
		}
		snapshot := func() string {
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),'versions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY user_id,scope,scope_id) FROM authorization_versions x),'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x),'events',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x))::text`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := snapshot()
		for _, test := range []struct{ name, raw string }{
			{"malformed", strings.TrimSuffix(string(valid), "}")},
			{"unknown-field", strings.TrimSuffix(string(valid), "}") + `,"unexpected":"` + marker + `"}`},
			{"multiple-values", string(valid) + ` {}`},
			{"wrong-type", strings.Replace(string(valid), `"role_code":"PLATFORM_MODEL_ADMIN"`, `"role_code":17`, 1)},
		} {
			t.Run(test.name, func(t *testing.T) {
				out := request("admin", test.raw)
				var body struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				if out.Code != http.StatusBadRequest || json.Unmarshal(out.Body.Bytes(), &body) != nil || body.Code != "INVALID_JSON" || body.Message != "request body is invalid" {
					t.Fatal("invalid JSON did not return explicit safe400", out.Code, out.Body.String())
				}
				if out.Header().Get("Content-Type") != "application/json" || out.Header().Get("Cache-Control") != "no-store" || strings.Contains(out.Body.String(), marker) {
					t.Fatal("invalid JSON response leaked input or changed API headers", out.Header(), out.Body.String())
				}
				if snapshot() != before {
					t.Fatal("invalid JSON request changed authorization or audit/outbox facts")
				}
			})
		}
		for _, boundary := range []struct {
			token string
			code  int
		}{{"", http.StatusUnauthorized}, {"target", http.StatusForbidden}} {
			out := request(boundary.token, "{")
			if out.Code != boundary.code {
				t.Fatal("authentication or permission boundary changed", out.Code)
			}
		}
		var grants, deniedAudits int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE idempotency_key=$1`, key).Scan(&grants); err != nil || grants != 0 {
			t.Fatal("authentication/permission refusal wrote grant", grants, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='authorization.permission.denied' AND resource_id='/admin/v1/role-bindings'`).Scan(&deniedAudits); err != nil || deniedAudits != 1 {
			t.Fatal("existing permission-denial audit changed", deniedAudits, err)
		}

		out := request("admin", string(valid))
		var binding RoleBinding
		if out.Code != http.StatusCreated || json.Unmarshal(out.Body.Bytes(), &binding) != nil || binding.UserID != target.ID || binding.RoleCode != "PLATFORM_MODEL_ADMIN" || binding.GrantSource != "ADMIN" {
			t.Fatal("valid original intent no longer grants exact target/role", out.Code, out.Body.String())
		}
	})

	t.Run("actual-http-catalog-grant-original-key-revoke-live-authority", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "role-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "role-target")
		other, _, _ := userCodeActor(t, r, "other-admin")
		tp := entitlementApprovalPrincipal(target)
		h := userCodeHandler(r, map[string]identity.Principal{"admin": admin, "target": tp, "other": other})
		catalog := platformRoleHTTP(h, "admin", "GET", "/admin/v1/roles", nil)
		if catalog.Code != 200 || catalog.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(catalog.Code, catalog.Body.String())
		}
		var definitions struct {
			Items []AdminRoleDefinition `json:"items"`
		}
		if err := json.Unmarshal(catalog.Body.Bytes(), &definitions); err != nil || len(definitions.Items) == 0 {
			t.Fatal(err)
		}
		for _, item := range definitions.Items {
			if item.Scope != "PLATFORM" || len(item.Permissions) == 0 {
				t.Fatal("invalid catalog", item.Code)
			}
		}
		if out := platformRoleHTTP(h, "target", "GET", "/admin/v1/roles", nil); out.Code != 403 {
			t.Fatal("personal read allowed", out.Code)
		}
		starts := time.Now().UTC().Truncate(time.Microsecond)
		expires := starts.Add(time.Hour)
		key := uuid.NewString()
		body := map[string]any{"user_id": target.ID, "role_code": "PLATFORM_IDENTITY_ADMIN", "starts_at": starts, "expires_at": expires, "idempotency_key": key, "reason": "controlled lifecycle grant"}
		out := platformRoleHTTP(h, "admin", "POST", "/admin/v1/role-bindings", body)
		if out.Code != 201 || out.Header().Get("X-Command-Type") != "ACCOUNT_ROLE_BINDING_GRANT" || out.Header().Get("X-Command-Policy-Version") != "1" {
			t.Fatal(out.Code, out.Body.String(), out.Header())
		}
		var first RoleBinding
		if err := json.Unmarshal(out.Body.Bytes(), &first); err != nil || first.GrantSource != "ADMIN" || first.UserID != target.ID {
			t.Fatal(first, err)
		}
		replay := platformRoleHTTP(h, "admin", "POST", "/admin/v1/role-bindings", body)
		var same RoleBinding
		if replay.Code != 201 || json.Unmarshal(replay.Body.Bytes(), &same) != nil || same.ID != first.ID {
			t.Fatal("lost ACK changed original grant", replay.Code, replay.Body.String())
		}
		for _, field := range []string{"role_code", "reason", "starts_at", "expires_at"} {
			copy := map[string]any{}
			for k, v := range body {
				copy[k] = v
			}
			switch field {
			case "role_code":
				copy[field] = "PLATFORM_MODEL_ADMIN"
			case "reason":
				copy[field] = "different controlled reason"
			case "starts_at":
				copy[field] = starts.Add(time.Second)
			case "expires_at":
				copy[field] = expires.Add(time.Second)
			}
			if changed := platformRoleHTTP(h, "admin", "POST", "/admin/v1/role-bindings", copy); changed.Code != 409 {
				t.Fatal("same key changed body", field, changed.Code)
			}
		}
		if changed := platformRoleHTTP(h, "other", "POST", "/admin/v1/role-bindings", body); changed.Code != 409 {
			t.Fatal("other actor took old key", changed.Code)
		}
		live := platformRoleHTTP(h, "target", "GET", "/admin/v1/roles", nil)
		if live.Code != 200 {
			t.Fatal("grant did not establish live authority", live.Code)
		}
		list := platformRoleHTTP(h, "admin", "GET", "/admin/v1/role-bindings?user_id="+target.ID.String(), nil)
		if list.Code != 200 || !strings.Contains(list.Body.String(), first.ID.String()) {
			t.Fatal("association missing", list.Code)
		}
		revoke := platformRoleHTTP(h, "admin", "DELETE", "/admin/v1/role-bindings/"+first.ID.String(), nil)
		if revoke.Code != 200 || revoke.Header().Get("X-Command-Type") != "ACCOUNT_ROLE_BINDING_REVOKE" || !strings.Contains(revoke.Body.String(), `"status":"REVOKED"`) {
			t.Fatal(revoke.Code, revoke.Body.String())
		}
		if live = platformRoleHTTP(h, "target", "GET", "/admin/v1/roles", nil); live.Code != 403 {
			t.Fatal("old credential retained role authority", live.Code)
		}
		replay = platformRoleHTTP(h, "admin", "POST", "/admin/v1/role-bindings", body)
		if replay.Code != 201 || !strings.Contains(replay.Body.String(), `"status":"REVOKED"`) {
			t.Fatal("old key reactivated revoked role", replay.Code, replay.Body.String())
		}
		var grants, audits, events int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE user_id=$1 AND idempotency_key=$2`, target.ID, key).Scan(&grants); err != nil || grants != 1 {
			t.Fatal(grants, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action='admin.role.binding.grant'`, first.ID.String()).Scan(&audits); err != nil || audits != 1 {
			t.Fatal(audits, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.role-binding.changed.v1'`, first.ID).Scan(&events); err != nil || events != 2 {
			t.Fatal(events, err)
		}
	})
	t.Run("scope-high-authority-source-and-pii-bounds", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "bounds-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "bounds-target")
		starts := time.Now().UTC().Truncate(time.Microsecond)
		expires := starts.Add(time.Hour)
		for _, role := range []string{"PLATFORM_SUPER_ADMIN", "OWNER", "PERSONAL_USER", "ORG_ADMIN"} {
			_, err := r.AdminGrantPlatformRoleBinding(ctx, admin, RoleBindingGrant{UserID: target.ID, RoleCode: role, StartsAt: &starts, ExpiresAt: &expires, IdempotencyKey: uuid.NewString()})
			if !errors.Is(err, ErrConflict) {
				t.Fatal("protected or foreign scope accepted", role, err)
			}
		}
		req := RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_AUDIT_PII_EXPORT", StartsAt: &starts, ExpiresAt: &expires, IdempotencyKey: uuid.NewString(), Reason: "controlled temporary audit access"}
		for _, mode := range []string{"no-expiry", "long-expiry", "short-reason", "stale-auth", "organization", "unit"} {
			copy := req
			p := admin
			copy.IdempotencyKey = uuid.NewString()
			id := uuid.New()
			switch mode {
			case "no-expiry":
				copy.ExpiresAt = nil
			case "long-expiry":
				end := starts.Add(25 * time.Hour)
				copy.ExpiresAt = &end
			case "short-reason":
				copy.Reason = "short"
			case "stale-auth":
				p.AuthenticatedAt = starts.Add(-time.Hour)
			case "organization":
				copy.OrganizationID = &id
			case "unit":
				copy.UnitID = &id
			}
			if _, err := r.AdminGrantPlatformRoleBinding(ctx, p, copy); err == nil {
				t.Fatal("invalid PII grant accepted", mode)
			}
		}
		binding, err := r.AdminGrantPlatformRoleBinding(ctx, admin, req)
		if err != nil {
			t.Fatal(err)
		}
		if binding.GrantSource != "ADMIN" || binding.ExpiresAt == nil || !binding.ExpiresAt.Equal(expires) {
			t.Fatal("source/expiry changed", binding)
		}
		decision, err := r.Authorize(ctx, entitlementApprovalPrincipal(target), PermissionPlatformAuditPIIExport, nil, nil)
		if err != nil || !decision.Allowed {
			t.Fatal("temporary PII permission absent", err)
		}
		if _, err = r.AdminRevokePlatformRoleBinding(ctx, admin, binding.ID); err != nil {
			t.Fatal(err)
		}
		decision, err = r.Authorize(ctx, entitlementApprovalPrincipal(target), PermissionPlatformAuditPIIExport, nil, nil)
		if err != nil || decision.Allowed {
			t.Fatal("PII revocation not immediate", err)
		}
	})
	t.Run("current-actor-required-by-repository-no-stale-credential-bypass", func(t *testing.T) {
		r := f.repository(t)
		reader := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_IDENTITY_ADMIN", "read-only")
		p := entitlementApprovalPrincipal(reader)
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "denied-target")
		if _, err := r.AdminGrantPlatformRoleBinding(ctx, p, RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_MODEL_ADMIN", IdempotencyKey: uuid.NewString()}); !errors.Is(err, ErrForbidden) {
			t.Fatal("read permission became assign", err)
		}
		if _, err := r.AdminGrantPlatformRoleBinding(ctx, identity.Principal{Issuer: p.Issuer, Subject: "missing"}, RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_MODEL_ADMIN", IdempotencyKey: uuid.NewString()}); !errors.Is(err, ErrForbidden) {
			t.Fatal("unmapped actor granted", err)
		}
	})
	t.Run("authority-expiry-is-rechecked-after-user-row-lock-wait", func(t *testing.T) {
		r := f.repository(t)
		admin, actor, _ := userCodeActor(t, r, "expiring-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "wait-target")
		// Another super administrator remains valid; this is a legitimate expiring
		// authorization fixture, not bypass of last-administrator protection.
		userCodeActor(t, r, "remaining-super-admin")
		if _, err := r.pool.Exec(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()+interval '2 seconds' WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN'`, actor); err != nil {
			t.Fatal(err)
		}
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = blocker.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, target.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, e := r.AdminGrantPlatformRoleBinding(ctx, admin, RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_MODEL_ADMIN", IdempotencyKey: uuid.NewString()})
			result <- e
		}()
		deadline := time.Now().Add(time.Second)
		blocked := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND usename=current_user AND wait_event_type='Lock' AND query LIKE '%SELECT id,status FROM users WHERE id=ANY%')`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("writer did not reach production user-lock barrier")
		}
		time.Sleep(2100 * time.Millisecond)
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-result:
			if !errors.Is(err, ErrForbidden) {
				t.Fatal("expired lock-wait authority wrote", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer never completed")
		}
		var n int
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_MODEL_ADMIN'`, target.ID).Scan(&n); err != nil || n != 0 {
			t.Fatal("expired authority left grant facts", n, err)
		}
	})
	t.Run("disabled-target-cannot-be-granted-but-existing-role-can-be-revoked", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "disabled-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "disabled-target")
		item, err := r.AdminGrantPlatformRoleBinding(ctx, admin, RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_MODEL_ADMIN", IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		if err = r.AdminSetUserStatus(ctx, admin, target.ID, "DISABLED"); err != nil {
			t.Fatal(err)
		}
		if _, err = r.AdminGrantPlatformRoleBinding(ctx, admin, RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_IDENTITY_ADMIN", IdempotencyKey: uuid.NewString()}); !errors.Is(err, ErrConflict) {
			t.Fatal("disabled target granted", err)
		}
		item, err = r.AdminRevokePlatformRoleBinding(ctx, admin, item.ID)
		if err != nil || item.Status != "REVOKED" {
			t.Fatal("disabled target could not be safely revoked", item.Status, err)
		}
	})

	t.Run("original-admin-repository-full-intent-platform-and-organization", func(t *testing.T) {
		r := f.repository(t)
		admin, adminID, _ := userCodeActor(t, r, "original-admin")
		other, _, _ := userCodeActor(t, r, "original-other")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "original-target")
		another := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "original-another")
		org, err := r.AdminCreateOrganization(ctx, admin, adminID, "Controlled original admin intent", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		for _, user := range []uuid.UUID{target.ID, another.ID} {
			if _, err = r.AdminAddMember(ctx, admin, org.ID, user, []string{"MEMBER"}, nil, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
		}
		unit, err := r.AdminCreateUnit(ctx, admin, org.ID, nil, "Controlled unit", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		secondUnit, err := r.AdminCreateUnit(ctx, admin, org.ID, nil, "Other controlled unit", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		snapshot := func() string {
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),'memberships',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),'versions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY user_id,scope,scope_id) FROM authorization_versions x),'audit',(SELECT count(*) FROM audit_logs),'events',(SELECT count(*) FROM outbox_events))::text`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		for _, scope := range []string{"PLATFORM", "UNIT"} {
			request := RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_MODEL_ADMIN", IdempotencyKey: uuid.NewString(), Reason: "controlled original intent"}
			if scope == "UNIT" {
				request.RoleCode = "UNIT_ADMIN"
				request.OrganizationID = &org.ID
				request.UnitID = &unit.Unit.ID
			}
			first, err := r.AdminGrantRoleBinding(ctx, admin, request)
			if err != nil {
				t.Fatal(scope, err)
			}
			before := snapshot()
			replay, err := r.AdminGrantRoleBinding(ctx, admin, request)
			if err != nil || replay.ID != first.ID || snapshot() != before {
				t.Fatal("omitted start did not retain original frozen grant", scope, err)
			}
			explicit := request
			micro := first.StartsAt.Add(999 * time.Nanosecond)
			explicit.StartsAt = &micro
			replay, err = r.AdminGrantRoleBinding(ctx, admin, explicit)
			if err != nil || replay.ID != first.ID || snapshot() != before {
				t.Fatal("database microsecond precision mismatch", scope, err)
			}
			for _, change := range []string{"actor", "subject", "role", "scope", "unit", "starts", "expires", "reason"} {
				changed := request
				p := admin
				later := first.StartsAt.Add(time.Second)
				switch change {
				case "actor":
					p = other
				case "subject":
					changed.UserID = another.ID
				case "role":
					if scope == "PLATFORM" {
						changed.RoleCode = "PLATFORM_IDENTITY_ADMIN"
					} else {
						changed.RoleCode = "ORG_MODEL_ADMIN"
						changed.UnitID = nil
					}
				case "scope":
					if scope == "PLATFORM" {
						changed.RoleCode = "UNIT_ADMIN"
						changed.OrganizationID = &org.ID
						changed.UnitID = &unit.Unit.ID
					} else {
						changed.RoleCode = "PLATFORM_IDENTITY_ADMIN"
						changed.OrganizationID = nil
						changed.UnitID = nil
					}
				case "unit":
					if scope == "PLATFORM" {
						changed.UnitID = &secondUnit.Unit.ID
					} else {
						changed.UnitID = &secondUnit.Unit.ID
					}
				case "starts":
					changed.StartsAt = &later
				case "expires":
					end := first.StartsAt.Add(time.Hour)
					changed.ExpiresAt = &end
				case "reason":
					changed.Reason = "changed original reason"
				}
				if _, err = r.AdminGrantRoleBinding(ctx, p, changed); !errors.Is(err, ErrConflict) || snapshot() != before {
					t.Fatal("original admin key accepted changed intent/effects", scope, change, err)
				}
			}
		}
	})

	t.Run("direct-expiry-waits-for-user-before-locking-assignment", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "expiry-order-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "expiry-order-target")
		now := time.Now().UTC().Truncate(time.Microsecond)
		end := now.Add(time.Hour)
		request := RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_IDENTITY_ADMIN", ExpiresAt: &end, IdempotencyKey: uuid.NewString()}
		item, err := r.AdminGrantPlatformRoleBinding(ctx, admin, request)
		if err != nil {
			t.Fatal(err)
		}
		r.now = func() time.Time { return now.Add(2 * time.Hour) }
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = blocker.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, target.ID); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, e := r.expireDirectRoleAssignment(ctx, item.ID); result <- e }()
		var blockerPID int32
		if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}
		blocked := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND usename=current_user AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			blocker.Rollback(ctx)
			t.Fatal("actual expiry did not reach production USER barrier")
		}
		var waitingQuery string
		if err = r.pool.QueryRow(ctx, `SELECT query FROM pg_stat_activity WHERE usename=current_user AND $1=ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1`, blockerPID).Scan(&waitingQuery); err != nil {
			t.Fatal(err)
		}
		t.Log("actual expiry waiting SQL:", waitingQuery)
		_, orderErr := blocker.Exec(ctx, `SELECT id FROM role_assignments WHERE id=$1 FOR UPDATE NOWAIT`, item.ID)
		if orderErr != nil {
			blocker.Rollback(ctx)
		} else {
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case err = <-result:
			if err != nil {
				t.Fatal("actual expiry failed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("actual expiry did not finish after USER release")
		}
		if orderErr != nil {
			t.Fatal("expiry locked assignment before its blocked USER write; reverse order permits deadlock", orderErr)
		}
		stored, err := roleBindingByID(ctx, r.pool, item.ID)
		if err != nil || stored.Status != "EXPIRED" {
			t.Fatal(stored.Status, err)
		}
		replay, err := r.AdminGrantPlatformRoleBinding(ctx, admin, request)
		if err != nil || replay.ID != item.ID || replay.Status != "EXPIRED" {
			t.Fatal("old key revived expired role", replay.Status, err)
		}
	})

	t.Run("actual-http-recent-auth-rejection-is-explicit-no-write", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "reauth-admin")
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "reauth-target")
		stale := admin
		stale.AuthenticatedAt = time.Now().Add(-time.Hour)
		h := userCodeHandler(r, map[string]identity.Principal{"stale": stale, "recent": admin})
		starts := time.Now().UTC().Truncate(time.Microsecond)
		end := starts.Add(time.Hour)
		key := uuid.NewString()
		body := map[string]any{"user_id": target.ID, "role_code": "PLATFORM_AUDIT_PII_EXPORT", "starts_at": starts, "expires_at": end, "idempotency_key": key, "reason": "controlled explicit recent authentication boundary"}
		denied := platformRoleHTTP(h, "stale", "POST", "/admin/v1/role-bindings", body)
		if denied.Code != 401 || !strings.Contains(denied.Body.String(), `"code":"RECENT_AUTHENTICATION_REQUIRED"`) {
			t.Fatal("explicit pre-write denial changed", denied.Code, denied.Body.String())
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE idempotency_key=$1`, key).Scan(&n); err != nil || n != 0 {
			t.Fatal("reauth refusal wrote grant", n, err)
		}
		resumed := platformRoleHTTP(h, "recent", "POST", "/admin/v1/role-bindings", body)
		if resumed.Code != 201 {
			t.Fatal("same original body/key after explicit no-write auth failure failed", resumed.Code, resumed.Body.String())
		}
	})

	t.Run("concurrent-one-admin-key-cannot-create-two-subject-grants", func(t *testing.T) {
		r := f.repository(t)
		admin, _, _ := userCodeActor(t, r, "concurrent-key-admin")
		a := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "concurrent-a")
		b := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "concurrent-b")
		key := uuid.NewString()
		type result struct {
			request RoleBindingGrant
			item    RoleBinding
			err     error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			request := RoleBindingGrant{UserID: id, RoleCode: "PLATFORM_MODEL_ADMIN", IdempotencyKey: key, Reason: "controlled concurrent original key"}
			go func() {
				<-start
				item, e := r.AdminGrantPlatformRoleBinding(ctx, admin, request)
				results <- result{request, item, e}
			}()
		}
		close(start)
		successes := 0
		for i := 0; i < 2; i++ {
			var got result
			select {
			case got = <-results:
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent original key did not complete")
			}
			if got.err == nil {
				successes++
				continue
			}
			if !errors.Is(got.err, ErrConflict) {
				var state interface{ SQLState() string }
				if !errors.As(got.err, &state) || state.SQLState() != "40001" {
					t.Fatal("unexpected lock/replay failure", got.err)
				}
			}
			if _, e := r.AdminGrantPlatformRoleBinding(ctx, admin, got.request); !errors.Is(e, ErrConflict) {
				t.Fatal("original losing subject/key did not resolve to conflict", e)
			}
		}
		if successes != 1 {
			t.Fatal("one original key did not have exactly one winning subject", successes)
		}
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM role_assignments WHERE idempotency_key=$1 AND grant_source='ADMIN'`, key).Scan(&n); err != nil || n != 1 {
			t.Fatal("one key created multiple subject grants", n, err)
		}
	})

}
