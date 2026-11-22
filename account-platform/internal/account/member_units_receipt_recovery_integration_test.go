package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Original public migrations and runtime roles; the actual PUT router stores
// the command before a controlled lifecycle authority loses its acknowledgement.
// Later changes and deletion use real domain operations, not unit-status edits.
func TestMemberUnitsReceiptSurvivesLegalUnitDeletion(t *testing.T) {
	f := retainedWalletFenceActors(t)
	for _, platform := range []bool{false, true} {
		name := "organization"
		if platform {
			name = "platform"
		}
		t.Run(name, func(t *testing.T) {
			r := f.repository(t)
			ctx := context.Background()
			owner, ownerID := f01ActiveFixtureUser(t, r)
			actor, actorID := f01ActiveFixtureUser(t, r)
			outsider, _ := f01ActiveFixtureUser(t, r)
			_, target := f01ActiveFixtureUser(t, r)
			_, otherTarget := f01ActiveFixtureUser(t, r)
			var platformRole string
			if err := r.pool.QueryRow(ctx, `SELECT role.code FROM roles role WHERE role.scope='PLATFORM' AND role.assignable AND EXISTS(SELECT 1 FROM role_permissions permission WHERE permission.role_code=role.code AND permission.permission_code=$1) ORDER BY role.code LIMIT 1`, canonicalPermissionCode(PermissionPlatformOrganizationsMembersManage)).Scan(&platformRole); err != nil {
				t.Fatal("installed assignable membership role", err)
			}
			grantPlatform := func(user uuid.UUID) RoleBinding {
				t.Helper()
				binding, err := r.AdminGrantRoleBinding(ctx, owner, RoleBindingGrant{UserID: user, RoleCode: platformRole, IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
				return binding
			}
			grantPlatform(ownerID)
			org, err := r.AdminCreateOrganization(ctx, owner, ownerID, "Receipt recovery", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			actorRoles := []string{"ORG_ADMIN"}
			var actorPlatformBinding RoleBinding
			if platform {
				actorRoles = []string{"MEMBER"}
				actorPlatformBinding = grantPlatform(actorID)
			}
			for _, user := range []uuid.UUID{actorID, target, otherTarget} {
				roles := []string{"MEMBER"}
				if user == actorID {
					roles = actorRoles
				}
				if _, err := r.AdminAddMember(ctx, owner, org.ID, user, roles, nil, uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			}
			unit, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, "Disposable unit", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			var originalVersion int64
			if err := r.pool.QueryRow(ctx, `SELECT version FROM memberships WHERE organization_id=$1 AND user_id=$2`, org.ID, target).Scan(&originalVersion); err != nil {
				t.Fatal(err)
			}
			key := uuid.NewString()
			projection := &memberUnitsLifecycleHTTPFixture{err: errors.New("controlled lost lifecycle acknowledgement")}
			currentActor := actor
			handler := NewService(r, nil).WithOrganizationMemberLifecycle(projection).Handler(func(context.Context, string) (identity.Principal, error) { return currentActor, nil })
			call := func(p identity.Principal, user uuid.UUID, version int64, units []uuid.UUID, requestKey string) (*httptest.ResponseRecorder, MembershipMutationResult) {
				t.Helper()
				currentActor = p
				body, err := json.Marshal(map[string]any{"unit_ids": units, "expected_version": version})
				if err != nil {
					t.Fatal(err)
				}
				prefix := "/v1/organizations/"
				if platform {
					prefix = "/admin/v1/organizations/"
				}
				request := httptest.NewRequest(http.MethodPut, prefix+org.ID.String()+"/members/"+user.String()+"/units", strings.NewReader(string(body)))
				request.Header.Set("Authorization", "Bearer controlled")
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", requestKey)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				var result MembershipMutationResult
				if response.Code == http.StatusOK {
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
				}
				return response, result
			}
			response, _ := call(actor, target, originalVersion, []uuid.UUID{unit.Unit.ID}, key)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE") || projection.calls != 1 {
				t.Fatal("unknown lifecycle ACK did not retain the original committed intent", response.Code)
			}
			later, err := r.ReplaceMemberUnits(ctx, owner, org.ID, target, originalVersion+1, []uuid.UUID{}, uuid.NewString(), false)
			if err != nil {
				t.Fatal("real later move", err)
			}
			deleted, err := r.AdminDeleteUnit(ctx, owner, org.ID, unit.Unit.ID, uuid.NewString())
			if err != nil || deleted.Unit.Status != "DELETED" {
				t.Fatal("real legal deletion", err)
			}
			snapshot := func() string {
				t.Helper()
				var raw string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),'sessions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY issuer,session_id) FROM user_sessions x),'memberships',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),'units',(SELECT jsonb_agg(to_jsonb(x) ORDER BY membership_id,unit_id) FROM membership_units x),'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY actor_user_id,organization_id,idempotency_key) FROM membership_commands x),'audit',(SELECT count(*) FROM audit_logs WHERE action<>'authorization.permission.denied'),'outbox',(SELECT count(*) FROM outbox_events))::text`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			permissionDenials := func() int {
				t.Helper()
				var count int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='authorization.permission.denied'`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			projection.err = nil
			replay := func(t *testing.T, expected MembershipMutationResult) {
				t.Helper()
				before := snapshot()
				response, result := call(actor, target, originalVersion, []uuid.UUID{unit.Unit.ID}, key)
				if response.Code != http.StatusOK || !result.Replayed || result.Member.UserID != target || result.Member.Version != expected.Member.Version || result.Member.Status != expected.Member.Status || len(result.Member.UnitIDs) != 0 || snapshot() != before {
					t.Fatal("exact receipt did not return current projection without another write", response.Code, result)
				}
				var commands, writes int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM membership_commands WHERE actor_user_id=$1 AND organization_id=$2 AND command_type='UNITS_REPLACE' AND idempotency_key=$3),(SELECT count(*) FROM audit_logs WHERE actor_user_id=$1 AND action='membership.units.replace')`, actorID, org.ID, key).Scan(&commands, &writes); err != nil || commands != 1 || writes != 1 {
					t.Fatal("unknown ACK retry repeated the original command or write", commands, writes, err)
				}
			}
			t.Run("original-key-after-deletion", func(t *testing.T) { replay(t, later) })
			for _, tc := range []struct {
				name    string
				actor   identity.Principal
				target  uuid.UUID
				version int64
				units   []uuid.UUID
				key     string
				status  int
			}{
				{"new-key-deleted-unit", actor, target, later.Member.Version, []uuid.UUID{unit.Unit.ID}, uuid.NewString(), 404},
				{"same-key-different-body", actor, target, originalVersion, []uuid.UUID{}, key, 409},
				{"same-key-different-version", actor, target, originalVersion + 1, []uuid.UUID{unit.Unit.ID}, key, 409},
				{"same-key-different-target", actor, otherTarget, originalVersion, []uuid.UUID{unit.Unit.ID}, key, 409},
				{"other-authorized-actor-cannot-borrow-receipt", owner, target, originalVersion, []uuid.UUID{unit.Unit.ID}, key, 404},
				{"unauthorized-actor", outsider, target, originalVersion, []uuid.UUID{unit.Unit.ID}, key, 403},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before, calls := snapshot(), projection.calls
					denials := permissionDenials()
					response, _ := call(tc.actor, tc.target, tc.version, tc.units, tc.key)
					if response.Code != tc.status || snapshot() != before || projection.calls != calls {
						t.Fatal("rejected retry changed facts or reached the lifecycle authority", response.Code, tc.status)
					}
					if platform && tc.status == http.StatusForbidden {
						denials++
					}
					if permissionDenials() != denials {
						t.Fatal("platform permission denial lost its existing security audit")
					}
				})
			}
			removed, err := r.AdminRemoveMember(ctx, owner, org.ID, target, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			t.Run("original-key-after-member-removal", func(t *testing.T) { replay(t, removed) })
			if platform {
				if _, err := r.AdminRevokeRoleBinding(ctx, owner, actorPlatformBinding.ID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.AdminReplaceMemberRoles(ctx, owner, org.ID, actorID, []string{"MEMBER"}, nil); err != nil {
				t.Fatal(err)
			}
			t.Run("revoked-authority-cannot-recover-receipt", func(t *testing.T) {
				before, calls := snapshot(), projection.calls
				denials := permissionDenials()
				response, _ := call(actor, target, originalVersion, []uuid.UUID{unit.Unit.ID}, key)
				if response.Code != http.StatusForbidden || snapshot() != before || projection.calls != calls {
					t.Fatal("receipt bypassed current actor authority", response.Code)
				}
				if platform {
					denials++
				}
				if permissionDenials() != denials {
					t.Fatal("revoked platform permission denial lost its existing security audit")
				}
			})
		})
	}
}
