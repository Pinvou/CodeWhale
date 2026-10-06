package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestA09LifecycleHTTPRequiresCompleteBoundAuthority(t *testing.T) {
	organizationID, userID, unitID := uuid.New(), uuid.New(), uuid.New()
	for _, method := range []string{"organization", "unit", "member"} {
		t.Run(method, func(t *testing.T) {
			for _, shape := range []string{"valid", "wrong_target", "trailing_json", "oversized_tail", "redirect", "missing_readiness", "false_readiness"} {
				if method == "member" && (shape == "missing_readiness" || shape == "false_readiness") {
					continue
				}
				t.Run(shape, func(t *testing.T) {
					var requests, redirected atomic.Int32
					destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						redirected.Add(1)
						w.WriteHeader(http.StatusOK)
					}))
					defer destination.Close()
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer controlled-service" || !strings.HasPrefix(r.URL.Path, "/prefix/internal/v1/organizations/"+organizationID.String()) {
							t.Error("request lost its fixed authority, method or configured path prefix")
						}
						if shape == "redirect" {
							http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
							return
						}
						response := map[string]any{"organization_id": organizationID, "status": "DISABLED", "account_version": 3, "ready": true, "blockers": []any{}}
						if method == "unit" {
							response["unit_id"], response["status"], response["structure_version"] = unitID, "DELETION_PENDING", 3
						} else if method == "member" {
							response["user_id"], response["status"], response["membership_version"] = userID, "REVOKED", 3
						}
						if shape == "wrong_target" {
							response["organization_id"] = uuid.New()
						} else if shape == "missing_readiness" {
							delete(response, "ready")
						} else if shape == "false_readiness" {
							response["ready"] = false
						}
						raw, _ := json.Marshal(response)
						if shape == "trailing_json" {
							raw = append(raw, []byte(`{}`)...)
						} else if shape == "oversized_tail" {
							raw = append(raw, []byte(strings.Repeat(" ", organizationLifecycleResponseLimit)+"x")...)
						}
						_, _ = w.Write(raw)
					}))
					defer server.Close()
					client, err := NewHTTPOrganizationLifecycleClient(server.URL+"/prefix", "controlled-service", "BILLING")
					if err != nil {
						t.Fatal(err)
					}
					ready := true
					switch method {
					case "organization":
						var assessment OrganizationLifecycleAssessment
						assessment, err = client.Apply(context.Background(), organizationID, "DISABLED", 3)
						ready = assessment.Ready
					case "unit":
						service := NewService(&fakeRepository{}, nil).WithOrganizationUnitLifecycle(client)
						var assessment OrganizationUnitDeletionAssessment
						assessment, err = service.applyOrganizationUnitLifecycle(context.Background(), organizationID, unitID, "DELETION_PENDING", 3)
						ready = assessment.Ready
					case "member":
						_, err = client.ApplyMemberLifecycle(context.Background(), organizationID, userID, "REVOKED", 3)
					}
					if shape == "valid" || shape == "false_readiness" {
						if err != nil || ready != (shape == "valid") {
							t.Fatal("valid authority response or its negative readiness was misrepresented")
						}
					} else if err == nil {
						t.Fatal("incomplete, foreign, redirected or oversized authority was accepted")
					}
					if requests.Load() != 1 || redirected.Load() != 0 {
						t.Fatal("request followed an authority redirect")
					}
				})
			}
		})
	}
}

func TestPostgresA09UnitCreationRequiresDependentReadiness(t *testing.T) {
	for _, scope := range []string{"organization", "admin"} {
		t.Run(scope, func(t *testing.T) {
			repository := accountDatabase(t)
			owner, _ := accountUser(t, repository)
			organization := accountOrganization(t, repository, owner)
			principal := owner
			prefix := "/v1"
			if scope == "admin" {
				principal = fixturePlatformAdmin(t, repository)
				prefix = "/admin/v1"
			}
			ready := false
			var calls int
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Status  string `json:"status"`
					Version int64  `json:"structure_version"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Status != "ACTIVE" {
					t.Fatal("invalid unit lifecycle request")
				}
				parts := strings.Split(r.URL.Path, "/")
				unitID, err := uuid.Parse(parts[len(parts)-2])
				if err != nil {
					t.Fatal(err)
				}
				_ = json.NewEncoder(w).Encode(OrganizationUnitDeletionAssessment{OrganizationID: organization.ID, UnitID: unitID, Status: body.Status, StructureVersion: body.Version, Ready: ready, Blockers: []OrganizationLifecycleBlocker{}})
			}))
			defer peer.Close()
			client, err := NewHTTPOrganizationLifecycleClient(peer.URL, "controlled", "TOKEN")
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(repository, nil).WithOrganizationUnitLifecycle(client)
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			key := uuid.NewString()
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, prefix+"/organizations/"+organization.ID.String()+"/units", strings.NewReader(`{"name":"dependent unit"}`))
				r.Header.Set("Authorization", "Bearer fixture")
				r.Header.Set("Idempotency-Key", key)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				return response
			}
			if response := request(); response.Code != http.StatusServiceUnavailable {
				t.Fatal("unit creation reported success while the dependent authority was not ready")
			}
			ready = true
			if response := request(); response.Code != http.StatusOK {
				t.Fatal("same-key retry did not finish the original unit activation")
			}
			var count int
			if err := repository.pool.QueryRow(context.Background(), `SELECT count(*) FROM organization_units WHERE organization_id=$1`, organization.ID).Scan(&count); err != nil || count != 1 || calls != 2 {
				t.Fatal("retry duplicated the unit or skipped authority activation")
			}
		})
	}
}

func TestPostgresA09MembershipReplayUsesCurrentAuthority(t *testing.T) {
	for _, operation := range []string{"admin_add", "accepted_invitation"} {
		t.Run(operation, func(t *testing.T) {
			repository := accountDatabase(t)
			owner, _ := accountUser(t, repository)
			member, me := accountUser(t, repository)
			organization := accountOrganization(t, repository, owner)
			admin := fixturePlatformAdmin(t, repository)
			key := uuid.NewString()
			var token string
			if operation == "admin_add" {
				if _, err := repository.AdminAddMember(context.Background(), admin, organization.ID, me.UserID, []string{"MEMBER"}, nil, key); err != nil {
					t.Fatal(err)
				}
			} else {
				token = accountInvitation(t, repository, owner, member, organization.ID, []string{"MEMBER"}).Token
			}
			removed, err := repository.AdminRemoveMember(context.Background(), admin, organization.ID, me.UserID, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Status  string `json:"status"`
					Version int64  `json:"membership_version"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Status != "REVOKED" || body.Version != removed.Member.Version {
					t.Error("replay attempted to reactivate obsolete member facts")
				}
				_ = json.NewEncoder(w).Encode(OrganizationMemberLifecycleState{OrganizationID: organization.ID, UserID: me.UserID, Status: body.Status, MembershipVersion: body.Version})
			}))
			defer peer.Close()
			client, err := NewHTTPOrganizationLifecycleClient(peer.URL, "controlled", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			principal := member
			path, body := "/v1/invitations/accept", `{"token":"`+token+`"}`
			if operation == "admin_add" {
				principal, path, body = admin, "/admin/v1/organizations/"+organization.ID.String()+"/members", `{"user_id":"`+me.UserID.String()+`","roles":["MEMBER"]}`
			}
			handler := NewService(repository, nil).WithOrganizationMemberLifecycle(client).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer fixture")
			request.Header.Set("Idempotency-Key", key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var result MembershipMutationResult
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Member.Status != "REVOKED" || result.Member.Version != removed.Member.Version {
				t.Fatal("replay misrepresented the current membership")
			}
		})
	}
}

func TestA09LifecycleClientRejectsCredentialOrQueryEndpoints(t *testing.T) {
	for _, rawURL := range []string{"ftp://example.test", "https://user:password@example.test", "https://example.test?token=controlled", "https://example.test#fragment"} {
		if _, err := NewHTTPOrganizationLifecycleClient(rawURL, "controlled", "TOKEN"); err == nil {
			t.Fatal("an invalid or credential-bearing endpoint was accepted")
		}
	}
	// Reading is bounded even when no JSON is present.
	if _, err := readOrganizationLifecycleResponse(&http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("x", organizationLifecycleResponseLimit+1)))}); err == nil {
		t.Fatal("an oversized response was accepted")
	}
}

func TestPostgresA09CreateUnitReplayAfterDeletionKeepsReceiptWithoutOldActivation(t *testing.T) {
	for _, route := range []string{"organization", "admin"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			repository := accountDatabase(t)
			owner, _ := accountUser(t, repository)
			admin := fixturePlatformAdmin(t, repository)
			organization := accountOrganization(t, repository, owner)
			pool := testdb.Open(t, "billing")
			peerRepo, err := billing.NewPostgresRepository(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatal("billing fixture constructor failed")
			}
			t.Cleanup(peerRepo.Close)
			peer := httptest.NewServer(billing.NewService(peerRepo).Handler("controlled-service"))
			defer peer.Close()
			client, err := NewHTTPOrganizationLifecycleClient(peer.URL, "controlled-service", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			principal := owner
			prefix := "/v1"
			if route == "admin" {
				principal = admin
				prefix = "/admin/v1"
			}
			handler := NewService(repository, nil).WithOrganizationUnitLifecycle(client).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			key := uuid.NewString()
			createPath := prefix + "/organizations/" + organization.ID.String() + "/units"
			request := func(method, path, body, key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer fixture")
				r.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			first := request("POST", createPath, `{"name":"replay unit"}`, key)
			var initial UnitMutationResult
			if first.Code != 201 || json.Unmarshal(first.Body.Bytes(), &initial) != nil {
				t.Fatalf("initial creation status=%d", first.Code)
			}
			principal = admin
			removed := request("DELETE", "/admin/v1/organizations/"+organization.ID.String()+"/units/"+initial.Unit.ID.String(), "", uuid.NewString())
			if removed.Code != 200 {
				t.Fatalf("delete status=%d", removed.Code)
			}
			principal = owner
			if route == "admin" {
				principal = admin
			}
			replay := request("POST", createPath, `{"name":"replay unit"}`, key)
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM organization_unit_lifecycle_states WHERE organization_id=$1 AND unit_id=$2`, organization.ID, initial.Unit.ID).Scan(&status); err != nil || status != "DELETED" {
				t.Fatal("peer deletion fence was lost")
			}
			var result UnitMutationResult
			if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &result) != nil || result.Unit.ID != initial.Unit.ID || !result.Replayed {
				t.Fatalf("completed immutable create receipt must be replayable after later deletion, status=%d", replay.Code)
			}
		})
	}
}

func TestPostgresA09CreationReplaySynchronizesCurrentLifecycle(t *testing.T) {
	for _, mutation := range []string{"rename", "move", "organization-disabled", "organization-deleted"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			repository := accountDatabase(t)
			owner, ownerMe := accountUser(t, repository)
			admin := fixturePlatformAdmin(t, repository)
			pool := testdb.Open(t, "billing")
			peerRepository, err := billing.NewPostgresRepository(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(peerRepository.Close)
			peer := httptest.NewServer(billing.NewService(peerRepository).Handler("controlled-service"))
			defer peer.Close()
			client, err := NewHTTPOrganizationLifecycleClient(peer.URL, "controlled-service", "BILLING")
			if err != nil {
				t.Fatal(err)
			}
			handler := NewService(repository, nil).WithOrganizationLifecycle(client).WithOrganizationUnitLifecycle(client).Handler(func(context.Context, string) (identity.Principal, error) { return admin, nil })
			request := func(method, path, body, key string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer fixture")
				r.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			organizationKey := uuid.NewString()
			organizationBody := `{"name":"current lifecycle organization","owner_user_id":"` + ownerMe.UserID.String() + `"}`
			createdOrganization := request(http.MethodPost, "/admin/v1/organizations", organizationBody, organizationKey)
			var organization Organization
			if createdOrganization.Code != http.StatusCreated || json.Unmarshal(createdOrganization.Body.Bytes(), &organization) != nil {
				t.Fatal("organization creation failed")
			}
			unitKey := uuid.NewString()
			unitPath := "/admin/v1/organizations/" + organization.ID.String() + "/units"
			unitBody := `{"name":"immutable unit"}`
			createdUnit := request(http.MethodPost, unitPath, unitBody, unitKey)
			var initial UnitMutationResult
			if createdUnit.Code != http.StatusCreated || json.Unmarshal(createdUnit.Body.Bytes(), &initial) != nil {
				t.Fatal("unit creation failed")
			}
			switch mutation {
			case "rename":
				if _, err := repository.AdminRenameUnit(ctx, admin, organization.ID, initial.Unit.ID, "renamed unit", uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			case "move":
				destination, err := repository.CreateUnit(ctx, owner, organization.ID, nil, "destination", uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repository.MoveUnit(ctx, owner, organization.ID, initial.Unit.ID, &destination.Unit.ID, accountStructureVersion(t, repository, organization.ID), uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			default:
				if response := request(http.MethodPatch, "/admin/v1/organizations/"+organization.ID.String()+"/status", `{"status":"DISABLED"}`, ""); response.Code != http.StatusNoContent {
					t.Fatal("organization disable failed")
				}
				if mutation == "organization-deleted" {
					if response := request(http.MethodDelete, "/admin/v1/organizations/"+organization.ID.String(), "", ""); response.Code != http.StatusNoContent {
						t.Fatal("organization deletion failed")
					}
				}
			}
			before := accountMutationSnapshot(t, repository, organization.ID)
			replay := request(http.MethodPost, unitPath, unitBody, unitKey)
			var result UnitMutationResult
			if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &result) != nil {
				t.Fatal("unit replay failed")
			}
			initial.Replayed = true
			expected, _ := json.Marshal(initial)
			actual, _ := json.Marshal(result)
			if string(expected) != string(actual) {
				t.Fatal("replay changed the immutable creation receipt")
			}
			if mutation == "rename" || mutation == "move" {
				var status string
				var version int64
				if err := pool.QueryRow(ctx, `SELECT status,structure_version FROM organization_unit_lifecycle_states WHERE organization_id=$1 AND unit_id=$2`, organization.ID, initial.Unit.ID).Scan(&status, &version); err != nil || status != "ACTIVE" || version != accountStructureVersion(t, repository, organization.ID) {
					t.Fatal("unit replay did not synchronize current structure authority")
				}
			} else {
				organizationReplay := request(http.MethodPost, "/admin/v1/organizations", organizationBody, organizationKey)
				var current Organization
				if organizationReplay.Code != http.StatusCreated || json.Unmarshal(organizationReplay.Body.Bytes(), &current) != nil {
					t.Fatal("closed organization creation receipt could not be replayed")
				}
				var status string
				var version int64
				if err := pool.QueryRow(ctx, `SELECT status,account_version FROM organization_lifecycle_states WHERE organization_id=$1`, organization.ID).Scan(&status, &version); err != nil || status != current.Status || version != current.Version || status == "ACTIVE" {
					t.Fatal("replay reactivated a closed organization")
				}
			}
			if accountMutationSnapshot(t, repository, organization.ID) != before {
				t.Fatal("creation replay changed Account data, versions, audit or outbox")
			}
		})
	}
}
