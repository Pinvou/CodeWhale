package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTokenOwnerCandidatesPostgresIntegration needs an isolated PostgreSQL
// database because migration deltas, authorization locks, and the 30/60
// multi-replica limits cannot be proved with mocks. Run remotely with:
// ACCOUNT_DATABASE_URL='postgresql://.../isolated_test_db?sslmode=require' go test ./internal/account -run TestTokenOwnerCandidatesPostgresIntegration -count=1
func TestTokenOwnerCandidatesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		databaseURL = strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	}
	if databaseURL == "" {
		t.Skip("ACCOUNT_DATABASE_URL is not configured; PostgreSQL integration was not executed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, connection, _, loaded := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 43)
	migration := loaded[len(loaded)-1]
	if migration.Version != 43 {
		t.Fatalf("latest account migration=%d, want 43 for this isolated test", migration.Version)
	}
	var freshPermissionScope string
	if err := repository.pool.QueryRow(ctx, `SELECT scope FROM permissions WHERE code=$1`, PermissionPlatformTokenOwnerCandidatesRead).Scan(&freshPermissionScope); err != nil || freshPermissionScope != "PLATFORM" {
		t.Fatalf("fresh migration permission scope=%q err=%v", freshPermissionScope, err)
	}
	assertTokenOwnerCandidateGrants(t, ctx, repository.pool)
	if err := migrations.DownOne(ctx, connection, loaded); err != nil {
		t.Fatalf("rollback 000043: %v", err)
	}
	migrations.Unlock(ctx, connection)

	superActor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "super")
	modelActor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_MODEL_ADMIN", "model")
	auditorActor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_AUDITOR", "auditor")
	deniedActor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_OPERATIONS_ADMIN", "denied")
	if _, err := repository.pool.Exec(ctx, `INSERT INTO permissions(code,scope,description) VALUES($1,'ORGANIZATION','wrong pre-existing definition')`, PermissionPlatformTokenOwnerCandidatesRead); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO role_permissions(role_code,permission_code) VALUES
	('PLATFORM_SUPER_ADMIN',$1),('PLATFORM_MODEL_ADMIN',$1),('PLATFORM_AUDITOR',$1),('PERSONAL_USER',$1)`, PermissionPlatformTokenOwnerCandidatesRead); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES
	($1,$3,'PLATFORM_MODEL_ADMIN','INTEGRATION_TEST','INTEGRATION_TEST',now()-interval '1 hour',now()-interval '1 hour'),
	($2,$3,'PERSONAL_USER','INTEGRATION_TEST','INTEGRATION_TEST',now()-interval '1 hour',now()-interval '1 hour')`, uuid.New(), uuid.New(), superActor.ID); err != nil {
		t.Fatal(err)
	}
	before := authorizationVersions(t, ctx, repository.pool, superActor.ID, modelActor.ID, auditorActor.ID)
	beforePersonal := authorizationScopeVersion(t, ctx, repository.pool, superActor.ID, "PERSONAL", uuid.Nil)
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatalf("apply 000043: %v", err)
	}
	migrations.Unlock(ctx, connection)
	after := authorizationVersions(t, ctx, repository.pool, superActor.ID, modelActor.ID, auditorActor.ID)
	for index := range before {
		if after[index] != before[index]+1 {
			t.Fatalf("authorization version[%d]=%d, want %d", index, after[index], before[index]+1)
		}
	}
	afterPersonal := authorizationScopeVersion(t, ctx, repository.pool, superActor.ID, "PERSONAL", uuid.Nil)
	if afterPersonal != beforePersonal+1 {
		t.Fatalf("personal authorization version=%d, want %d", afterPersonal, beforePersonal+1)
	}
	assertTokenOwnerCandidateGrants(t, ctx, repository.pool)
	if _, err := repository.pool.Exec(ctx, migration.UpSQL); err != nil {
		t.Fatalf("repeat up: %v", err)
	}
	if repeated := authorizationVersions(t, ctx, repository.pool, superActor.ID, modelActor.ID, auditorActor.ID); fmt.Sprint(repeated) != fmt.Sprint(after) {
		t.Fatalf("repeat up changed versions: before=%v after=%v", after, repeated)
	}
	if repeated := authorizationScopeVersion(t, ctx, repository.pool, superActor.ID, "PERSONAL", uuid.Nil); repeated != afterPersonal {
		t.Fatalf("repeat up changed personal version: before=%d after=%d", afterPersonal, repeated)
	}
	beforeDown := authorizationVersions(t, ctx, repository.pool, superActor.ID, modelActor.ID, auditorActor.ID)
	if _, err := repository.pool.Exec(ctx, migration.DownSQL); err != nil {
		t.Fatalf("first raw down: %v", err)
	}
	downVersions := authorizationVersions(t, ctx, repository.pool, superActor.ID, modelActor.ID, auditorActor.ID)
	if downVersions[0] != beforeDown[0]+1 || downVersions[1] != beforeDown[1]+1 || downVersions[2] != beforeDown[2] {
		t.Fatalf("first down versions=%v, want [%d %d %d]", downVersions, beforeDown[0]+1, beforeDown[1]+1, beforeDown[2])
	}
	if personalAfterDown := authorizationScopeVersion(t, ctx, repository.pool, superActor.ID, "PERSONAL", uuid.Nil); personalAfterDown != afterPersonal {
		t.Fatalf("first down changed personal version: before=%d after=%d", afterPersonal, personalAfterDown)
	}
	if _, err := repository.pool.Exec(ctx, migration.DownSQL); err != nil {
		t.Fatalf("repeat raw down: %v", err)
	}
	if repeated := authorizationVersions(t, ctx, repository.pool, superActor.ID, modelActor.ID, auditorActor.ID); fmt.Sprint(repeated) != fmt.Sprint(downVersions) {
		t.Fatalf("repeat down changed versions: before=%v after=%v", downVersions, repeated)
	}
	if _, err := repository.pool.Exec(ctx, migration.UpSQL); err != nil {
		t.Fatalf("restore 000043: %v", err)
	}

	serviceActor := TokenOwnerCandidateActor{Issuer: superActor.Issuer, Subject: superActor.Subject, CallerService: "token-gateway", CallerSubject: "system:serviceaccount:test:token-gateway"}
	if _, err := repository.TokenOwnerCandidates(ctx, TokenOwnerCandidateActor{Issuer: deniedActor.Issuer, Subject: deniedActor.Subject, CallerService: serviceActor.CallerService, CallerSubject: serviceActor.CallerSubject}, TokenOwnerCandidateFilter{Limit: 20}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unprivileged actor error=%v, want forbidden", err)
	}
	if response := requestTokenOwnerCandidates(repository, deniedActor, ""); response.Code != http.StatusForbidden {
		t.Fatalf("unprivileged HTTP status=%d body=%s", response.Code, response.Body.String())
	}
	empty, err := repository.TokenOwnerCandidates(ctx, serviceActor, TokenOwnerCandidateFilter{OwnerType: "UNIT", Limit: 20})
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("authorized empty page=%+v err=%v", empty, err)
	}
	assertTokenOwnerCandidateLifecycleWhitelist(t, ctx, repository, serviceActor, superActor.ID)
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts`); err != nil {
		t.Fatal(err)
	}
	seedTokenOwnerCandidateOrganizations(t, ctx, repository.pool, superActor.ID, 1601)
	scanStarted := time.Now()
	scanFilter := TokenOwnerCandidateFilter{OwnerType: "ORGANIZATION", Limit: 500}
	scanPage, err := repository.TokenOwnerCandidates(ctx, serviceActor, scanFilter)
	if err != nil {
		t.Fatal(err)
	}
	total := len(scanPage.Items)
	lateOrganizationID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	lateTx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lateTx.Rollback(ctx)
	if _, err := lateTx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'late organization','ACTIVE',$2,clock_timestamp(),clock_timestamp())`, lateOrganizationID, superActor.ID); err != nil {
		t.Fatal(err)
	}
	lateMembershipID := uuid.New()
	if _, err := lateTx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,version,joined_at,updated_at)
VALUES($1,$2,$3,'ACTIVE',1,clock_timestamp(),clock_timestamp())`, lateMembershipID, lateOrganizationID, superActor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := lateTx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at)
VALUES($1,$2,'OWNER',$3,clock_timestamp(),clock_timestamp()-interval '1 second')`, uuid.New(), lateMembershipID, superActor.ID); err != nil {
		t.Fatal(err)
	}
	var lateOwners int
	if err := lateTx.QueryRow(ctx, `SELECT count(*) FROM memberships membership JOIN role_bindings binding ON binding.membership_id=membership.id
WHERE membership.organization_id=$1 AND membership.status<>'REVOKED' AND binding.role='OWNER' AND binding.revoked_at IS NULL
AND binding.starts_at<=now()`, lateOrganizationID).Scan(&lateOwners); err != nil || lateOwners != 1 {
		t.Fatalf("late organization owners=%d err=%v", lateOwners, err)
	}
	if err := lateTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for scanPage.next != nil {
		scanFilter.Cursor = scanPage.next
		scanPage, err = repository.TokenOwnerCandidates(ctx, serviceActor, scanFilter)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range scanPage.Items {
			if item.ID == lateOrganizationID {
				t.Fatal("database-time high-water admitted a post-first-page insert")
			}
		}
		total += len(scanPage.Items)
	}
	if total != 1603 || time.Since(scanStarted) > 10*time.Second {
		t.Fatalf("bulk scan total=%d elapsed=%s", total, time.Since(scanStarted))
	}
	var logicalScans int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_owner_candidate_query_attempts WHERE actor_user_id=$1 AND caller_service=$2`, superActor.ID, serviceActor.CallerService).Scan(&logicalScans); err != nil || logicalScans != 1 {
		t.Fatalf("logical scan attempts=%d err=%v", logicalScans, err)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		page, err := repository.TokenOwnerCandidates(ctx, serviceActor, TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1})
		if err != nil || page.next == nil {
			t.Fatalf("active scan %d page=%+v err=%v", index, page, err)
		}
	}
	if _, err := repository.TokenOwnerCandidates(ctx, serviceActor, TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1}); !errors.Is(err, ErrTokenOwnerCandidateRateLimited) {
		t.Fatalf("fourth concurrent scan error=%v", err)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts`); err != nil {
		t.Fatal(err)
	}
	activeServiceActors := make([]TokenOwnerCandidateActor, 0, 11)
	for index := 0; index < 11; index++ {
		actor := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_MODEL_ADMIN", fmt.Sprintf("active-service-%02d", index))
		activeServiceActors = append(activeServiceActors, actor.TokenActor("active-scan-service", fmt.Sprintf("replica-%02d", index)))
	}
	for index := 0; index < 10; index++ {
		page, err := repository.TokenOwnerCandidates(ctx, activeServiceActors[index], TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1})
		if err != nil || page.next == nil {
			t.Fatalf("service active scan %d page=%+v err=%v", index, page, err)
		}
	}
	if _, err := repository.TokenOwnerCandidates(ctx, activeServiceActors[10], TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1}); !errors.Is(err, ErrTokenOwnerCandidateRateLimited) {
		t.Fatalf("eleventh service active scan error=%v", err)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts`); err != nil {
		t.Fatal(err)
	}

	cursor := firstTokenOwnerCandidateCursor(t, repository, superActor)
	lateID := uuid.MustParse("ffffffff-ffff-ffff-ffff-fffffffffff0")
	if _, err := repository.pool.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE','late owner',generate_unique_user_code(),clock_timestamp(),clock_timestamp())`, lateID); err != nil {
		t.Fatal(err)
	}
	continuation := cursor
	for pages := 0; continuation != "" && pages < 20; pages++ {
		response := requestTokenOwnerCandidates(repository, superActor, continuation)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), lateID.String()) {
			t.Fatalf("high-water continuation status=%d body=%s", response.Code, response.Body.String())
		}
		var page TokenOwnerCandidatePage
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		continuation = ""
		if page.NextCursor != nil {
			continuation = *page.NextCursor
		}
	}
	if continuation != "" {
		t.Fatal("high-water traversal did not terminate")
	}
	var invalidCursorAttemptsBefore int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_owner_candidate_query_attempts`).Scan(&invalidCursorAttemptsBefore); err != nil {
		t.Fatal(err)
	}
	if status := requestTokenOwnerCandidates(repository, superActor, cursor+"x").Code; status != http.StatusBadRequest {
		t.Fatalf("tampered cursor status=%d", status)
	}
	if status := requestTokenOwnerCandidates(repository, modelActor, cursor).Code; status != http.StatusBadRequest {
		t.Fatalf("cross-subject cursor status=%d", status)
	}
	var invalidCursorAttemptsAfter int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_owner_candidate_query_attempts`).Scan(&invalidCursorAttemptsAfter); err != nil {
		t.Fatal(err)
	}
	if invalidCursorAttemptsAfter != invalidCursorAttemptsBefore+2 {
		t.Fatalf("invalid cursor attempts=%d, want %d", invalidCursorAttemptsAfter, invalidCursorAttemptsBefore+2)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE authorization_versions SET version=version+1,updated_at=now() WHERE user_id=$1 AND scope='PLATFORM'`, superActor.ID); err != nil {
		t.Fatal(err)
	}
	if status := requestTokenOwnerCandidates(repository, superActor, cursor).Code; status != http.StatusGone {
		t.Fatalf("authorization-changed cursor status=%d", status)
	}

	assertTokenOwnerCandidateRateLimit(t, ctx, repository, serviceActor, 31, 30)
	actors := []TokenOwnerCandidateActor{
		{Issuer: superActor.Issuer, Subject: superActor.Subject, CallerService: "shared-token-service", CallerSubject: "replica-a"},
		{Issuer: modelActor.Issuer, Subject: modelActor.Subject, CallerService: "shared-token-service", CallerSubject: "replica-b"},
		{Issuer: auditorActor.Issuer, Subject: auditorActor.Subject, CallerService: "shared-token-service", CallerSubject: "replica-c"},
	}
	// The auditor's incorrect pre-existing grant was removed, so explicitly use
	// the approved model role for the third service-limit actor.
	actors[2] = seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_MODEL_ADMIN", "model-two").TokenActor("shared-token-service", "replica-c")
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts`); err != nil {
		t.Fatal(err)
	}
	assertSharedServiceLimit(t, ctx, repository, actors)
	assertTokenAccountCreateCompositeAuthority(t, ctx, repository, superActor)
}

func assertTokenAccountCreateCompositeAuthority(t *testing.T, ctx context.Context, repository *PostgresRepository, actor integrationActor) {
	t.Helper()
	permissions := []string{"platform.tokens.manage", PermissionPlatformTokenOwnerCandidatesRead}
	decision, err := repository.AuthorizeComposite(ctx, identity.Principal{Issuer: actor.Issuer, Subject: actor.Subject}, nil, permissions)
	if err != nil || !decision.Allowed || decision.PlatformAuthorizationVersion < 1 || len(decision.GrantedPermissions) != 2 {
		t.Fatalf("all-only Token Account authority=%+v err=%v", decision, err)
	}
	payload, _ := json.Marshal(PermissionCheck{Issuer: actor.Issuer, Subject: actor.Subject, AllPermissions: permissions})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/authorization/check", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer integration-service-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).WithServiceToken("integration-service-token").Handler(nil).ServeHTTP(response, request)
	var projection AuthorizationDecision
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &projection) != nil || !projection.Allowed || projection.PlatformAuthorizationVersion != decision.PlatformAuthorizationVersion {
		t.Fatalf("all-only Account HTTP status=%d body=%s decision=%+v", response.Code, response.Body.String(), decision)
	}
	var ownerVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT version FROM users WHERE id=$1`, actor.ID).Scan(&ownerVersion); err != nil {
		t.Fatal(err)
	}
	issuer, subject, ownerType, ownerID, commandDigest := actor.Issuer, actor.Subject, "USER", actor.ID.String(), strings.Repeat("a", 64)
	subjectHash := sha256.Sum256([]byte(subject))
	authority := tokenauthority.Request{AuthoritySchemaVersion: "1", Mode: tokenauthority.ModeT1Snapshot, IdentityIssuer: &issuer, IdentitySubject: &subject, IdentitySubjectSHA256: authorityString(hex.EncodeToString(subjectHash[:])), OwnerType: &ownerType, OwnerID: &ownerID, RequiredPermissions: tokenauthority.RequiredPermissions, CommandReference: uuid.NewString(), CommandRequestDigest: &commandDigest}
	authority.AuthorityRequestSHA256 = authority.Digest()
	callAuthority := func() (int, tokenauthority.Response, string) {
		raw, _ := json.Marshal(authority)
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/token-account-authority/snapshot", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer integration-service-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		NewService(repository, &fakeWalletProvisioner{}).WithServiceToken("integration-service-token").Handler(nil).ServeHTTP(response, request)
		var result tokenauthority.Response
		_ = json.Unmarshal(response.Body.Bytes(), &result)
		return response.Code, result, response.Body.String()
	}
	status, allowed, body := callAuthority()
	if status != http.StatusOK || allowed.Decision != "ALLOW" || allowed.ReasonCode != nil || allowed.ObservedPlatformAuthorizationVersion != strconv.FormatInt(decision.PlatformAuthorizationVersion, 10) || allowed.ObservedOwnerVersion == nil || *allowed.ObservedOwnerVersion != strconv.FormatInt(ownerVersion, 10) || allowed.OwnerIdentityDigest == nil || *allowed.OwnerIdentityDigest != tokenauthority.OwnerIdentityDigest("USER", actor.ID, "ACTIVE", ownerVersion) {
		t.Fatalf("authority allow status=%d result=%+v body=%s", status, allowed, body)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM role_permissions
WHERE permission_code='platform.tokens.manage' AND role_code IN ('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE authorization_versions SET version=version+1,updated_at=now()
WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'`, actor.ID); err != nil {
		t.Fatal(err)
	}
	status, denied, body := callAuthority()
	deniedVersion, _ := strconv.ParseInt(denied.ObservedPlatformAuthorizationVersion, 10, 64)
	if status != http.StatusOK || denied.Decision != "DENY" || denied.ReasonCode == nil || *denied.ReasonCode != "AUTHORIZATION_REVOKED" || deniedVersion <= decision.PlatformAuthorizationVersion || denied.ObservedOwnerVersion == nil || *denied.ObservedOwnerVersion != strconv.FormatInt(ownerVersion, 10) {
		t.Fatalf("authority revoke status=%d result=%+v body=%s", status, denied, body)
	}
}

func assertTokenOwnerCandidateLifecycleWhitelist(t *testing.T, ctx context.Context, repository *PostgresRepository, actor TokenOwnerCandidateActor, creator uuid.UUID) {
	t.Helper()
	createdAt := time.Now().UTC().Add(-time.Hour)
	userStatuses := []string{"PROVISIONING", "ACTIVE", "DISABLED", "BOOTSTRAP_PENDING", "REPAIR_REQUIRED", "CLOSING", "DELETED"}
	expectedUsers := map[uuid.UUID]string{}
	for _, status := range userStatuses {
		id := uuid.New()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,$2,$3,generate_unique_user_code(),$4,$4)`, id, status, "candidate-"+strings.ToLower(status), createdAt); err != nil {
			t.Fatal(err)
		}
		if status == "PROVISIONING" || status == "ACTIVE" || status == "DISABLED" {
			expectedUsers[id] = status
		}
	}
	activeOrganization, disabledOrganization, deletedOrganization := uuid.New(), uuid.New(), uuid.New()
	organizationTx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer organizationTx.Rollback(ctx)
	for _, organization := range []struct {
		id     uuid.UUID
		status string
	}{{activeOrganization, "ACTIVE"}, {disabledOrganization, "DISABLED"}, {deletedOrganization, "DELETED"}} {
		if _, err := organizationTx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, organization.id, "candidate-"+strings.ToLower(organization.status), organization.status, creator, createdAt); err != nil {
			t.Fatal(err)
		}
		if organization.status == "DELETED" {
			continue
		}
		membershipID := uuid.New()
		if _, err := organizationTx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,version,joined_at,updated_at)
VALUES($1,$2,$3,'ACTIVE',1,$4,$4)`, membershipID, organization.id, creator, createdAt); err != nil {
			t.Fatal(err)
		}
		if _, err := organizationTx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at)
VALUES($1,$2,'OWNER',$3,$4,$4)`, uuid.New(), membershipID, creator, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := organizationTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	activeUnit, disabledUnit, deletedUnit := uuid.New(), uuid.New(), uuid.New()
	for _, unit := range []struct {
		id     uuid.UUID
		status string
	}{{activeUnit, "ACTIVE"}, {disabledUnit, "DISABLED"}, {deletedUnit, "DELETED"}} {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO organization_units(id,organization_id,parent_id,name,status,created_at,updated_at) VALUES($1,$2,NULL,$3,$4,$5,$5)`, unit.id, activeOrganization, "candidate-"+strings.ToLower(unit.status), unit.status, createdAt); err != nil {
			t.Fatal(err)
		}
	}

	assertPage := func(ownerType string, expected map[uuid.UUID]string, excluded ...uuid.UUID) {
		t.Helper()
		page, err := repository.TokenOwnerCandidates(ctx, actor, TokenOwnerCandidateFilter{OwnerType: ownerType, Limit: 50000})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uuid.UUID]string{}
		for _, item := range page.Items {
			if item.Status != "ACTIVE" && item.Status != "DISABLED" && item.Status != "PROVISIONING" {
				t.Fatalf("%s emitted non-selectable lifecycle %q", ownerType, item.Status)
			}
			seen[item.ID] = item.Status
		}
		for id, status := range expected {
			if seen[id] != status {
				t.Fatalf("%s candidate %s status=%q want=%q", ownerType, id, seen[id], status)
			}
		}
		for _, id := range excluded {
			if _, ok := seen[id]; ok {
				t.Fatalf("%s emitted excluded lifecycle candidate %s", ownerType, id)
			}
		}
	}
	assertPage("USER", expectedUsers)
	assertPage("ORGANIZATION", map[uuid.UUID]string{activeOrganization: "ACTIVE", disabledOrganization: "DISABLED"}, deletedOrganization)
	assertPage("UNIT", map[uuid.UUID]string{activeUnit: "ACTIVE", disabledUnit: "DISABLED"}, deletedUnit)
}

func TestAccountApprovalAuthorizationMigrationChainPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, connection, _, loaded := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 0)
	if latest := loaded[len(loaded)-1]; latest.Version != 74 {
		t.Fatalf("latest account migration=%d, want 74", latest.Version)
	}
	var sessionUser, currentUser string
	var latestVersion int64
	if err := connection.QueryRow(ctx, `SELECT session_user,current_user,(SELECT max(version) FROM schema_migrations)`).
		Scan(&sessionUser, &currentUser, &latestVersion); err != nil || sessionUser != "account_migrator" ||
		currentUser != "account_service" || latestVersion != 74 {
		t.Fatalf("migration identity session=%q current=%q version=%d err=%v", sessionUser, currentUser, latestVersion, err)
	}
	var policyVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT policy_version FROM approval_command_policies
WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' ORDER BY policy_version DESC LIMIT 1`).Scan(&policyVersion); err != nil || policyVersion != 2 {
		t.Fatalf("entitlement correction policy version=%d err=%v", policyVersion, err)
	}
	var controlSchemas, authorizationSchemas int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE nspname='account_approval_control'),
count(*) FILTER (WHERE nspname='account_approval_authorization') FROM pg_namespace`).Scan(&controlSchemas, &authorizationSchemas); err != nil ||
		controlSchemas != 1 || authorizationSchemas != 1 {
		t.Fatalf("production authorization schemas control=%d authorization=%d err=%v", controlSchemas, authorizationSchemas, err)
	}
	if _, err := connection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	var durable string
	var proofWire, legacyWire bool
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('ENTITLEMENT_COMPONENT_ADJUSTMENT',2,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-entitlement-component-adjustment-executor-p2-v1','billing-command-executor','ENTITLEMENT_COMPONENT_ADJUSTMENT',2,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-entitlement-component-adjustment-executor-p2-v1','billing-command-executor','ENTITLEMENT_COMPONENT_ADJUSTMENT',2,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&durable, &proofWire, &legacyWire); err != nil || durable != "billing-entitlement-component-adjustment-executor-p2-v1" || !proofWire || legacyWire {
		t.Fatalf("entitlement consumer route durable=%q proof=%v legacy=%v err=%v", durable, proofWire, legacyWire, err)
	}
	// LEDGER_REPAIR joins the dedicated routing with its original v1 policy: the
	// shared legacy durable must not route it and the dedicated durable accepts
	// only the frozen RFC8785 wire.
	var ledgerRepairLegacy *string
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('LEDGER_REPAIR',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&ledgerRepairLegacy); err != nil || ledgerRepairLegacy != nil {
		t.Fatalf("legacy ledger repair route=%v err=%v", ledgerRepairLegacy, err)
	}
	var ledgerRepairDurable string
	var ledgerRepairProofWire, ledgerRepairLegacyWire bool
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('LEDGER_REPAIR',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-ledger-repair-executor-p1-v1','billing-command-executor','LEDGER_REPAIR',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-ledger-repair-executor-p1-v1','billing-command-executor','LEDGER_REPAIR',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&ledgerRepairDurable, &ledgerRepairProofWire, &ledgerRepairLegacyWire); err != nil ||
		ledgerRepairDurable != "billing-ledger-repair-executor-p1-v1" || !ledgerRepairProofWire || ledgerRepairLegacyWire {
		t.Fatalf("ledger repair consumer route durable=%q proof=%v legacy=%v err=%v", ledgerRepairDurable, ledgerRepairProofWire, ledgerRepairLegacyWire, err)
	}
	// RECEIVABLE_WRITE_OFF joins the dedicated routing with its original v1
	// policy under the same proof-era-only wire rule.
	var writeOffLegacy *string
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('RECEIVABLE_WRITE_OFF',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&writeOffLegacy); err != nil || writeOffLegacy != nil {
		t.Fatalf("legacy receivable write-off route=%v err=%v", writeOffLegacy, err)
	}
	var writeOffDurable string
	var writeOffProofWire, writeOffLegacyWire bool
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('RECEIVABLE_WRITE_OFF',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-receivable-write-off-executor-p1-v1','billing-command-executor','RECEIVABLE_WRITE_OFF',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-receivable-write-off-executor-p1-v1','billing-command-executor','RECEIVABLE_WRITE_OFF',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&writeOffDurable, &writeOffProofWire, &writeOffLegacyWire); err != nil ||
		writeOffDurable != "billing-receivable-write-off-executor-p1-v1" || !writeOffProofWire || writeOffLegacyWire {
		t.Fatalf("receivable write-off consumer route durable=%q proof=%v legacy=%v err=%v", writeOffDurable, writeOffProofWire, writeOffLegacyWire, err)
	}
	// PAYMENT_FINANCIAL_REPAIR joins the dedicated routing under the same rule.
	var paymentRepairLegacy *string
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('PAYMENT_FINANCIAL_REPAIR',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&paymentRepairLegacy); err != nil || paymentRepairLegacy != nil {
		t.Fatalf("legacy payment financial repair route=%v err=%v", paymentRepairLegacy, err)
	}
	var paymentRepairDurable string
	var paymentRepairProofWire, paymentRepairLegacyWire bool
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('PAYMENT_FINANCIAL_REPAIR',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-payment-financial-repair-executor-p1-v1','billing-command-executor','PAYMENT_FINANCIAL_REPAIR',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-payment-financial-repair-executor-p1-v1','billing-command-executor','PAYMENT_FINANCIAL_REPAIR',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&paymentRepairDurable, &paymentRepairProofWire, &paymentRepairLegacyWire); err != nil ||
		paymentRepairDurable != "billing-payment-financial-repair-executor-p1-v1" || !paymentRepairProofWire || paymentRepairLegacyWire {
		t.Fatalf("payment financial repair consumer route durable=%q proof=%v legacy=%v err=%v", paymentRepairDurable, paymentRepairProofWire, paymentRepairLegacyWire, err)
	}
	// REFUND_EXCEPTION_REPAIR joins the dedicated routing under the same rule.
	var refundExceptionLegacy *string
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('REFUND_EXCEPTION_REPAIR',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&refundExceptionLegacy); err != nil || refundExceptionLegacy != nil {
		t.Fatalf("legacy refund exception repair route=%v err=%v", refundExceptionLegacy, err)
	}
	var refundExceptionDurable string
	var refundExceptionProofWire, refundExceptionLegacyWire bool
	if err := connection.QueryRow(ctx, `SELECT account_approval_authorization.authorization_consumer_route_durable('REFUND_EXCEPTION_REPAIR',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-refund-exception-repair-executor-p1-v1','billing-command-executor','REFUND_EXCEPTION_REPAIR',1,'billing-command-executor','APPROVAL_RFC8785_DECIMAL_V1'),
account_approval_authorization.authorization_consumer_mapping_allows('billing-refund-exception-repair-executor-p1-v1','billing-command-executor','REFUND_EXCEPTION_REPAIR',1,'billing-command-executor','LEGACY_TYPED_V1')`).
		Scan(&refundExceptionDurable, &refundExceptionProofWire, &refundExceptionLegacyWire); err != nil ||
		refundExceptionDurable != "billing-refund-exception-repair-executor-p1-v1" || !refundExceptionProofWire || refundExceptionLegacyWire {
		t.Fatalf("refund exception repair consumer route durable=%q proof=%v legacy=%v err=%v", refundExceptionDurable, refundExceptionProofWire, refundExceptionLegacyWire, err)
	}
	if _, err := connection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
}

type integrationActor struct {
	ID      uuid.UUID
	Issuer  string
	Subject string
}

func (actor integrationActor) TokenActor(service, subject string) TokenOwnerCandidateActor {
	return TokenOwnerCandidateActor{Issuer: actor.Issuer, Subject: actor.Subject, CallerService: service, CallerSubject: subject}
}

func newTokenOwnerCandidateIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string, maximumVersion int64) (*PostgresRepository, *pgx.Conn, *ApprovalRelayStore, []migrations.Migration) {
	t.Helper()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	fixtureDatabase := "account_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	databaseIdentifier := pgx.Identifier{fixtureDatabase}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+databaseIdentifier+` OWNER account_service`); err != nil {
		_ = admin.Close(context.Background())
		t.Fatal(err)
	}
	var connection *pgx.Conn
	var pool, writerPool, relayPool *pgxpool.Pool
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if connection != nil {
			_ = connection.Close(cleanupCtx)
		}
		if pool != nil {
			pool.Close()
		}
		if writerPool != nil {
			writerPool.Close()
		}
		if relayPool != nil {
			relayPool.Close()
		}
		_, _ = admin.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+databaseIdentifier)
		_ = admin.Close(context.Background())
	})

	migrationConfiguration, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	migrationConfiguration.Database = fixtureDatabase
	connection, err = pgx.ConnectConfig(ctx, migrationConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `GRANT CREATE ON DATABASE `+databaseIdentifier+` TO account_service WITH GRANT OPTION`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION account_service; GRANT CREATE ON DATABASE `+databaseIdentifier+` TO account_migrator`); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	connection = nil
	connection, err = pgx.ConnectConfig(ctx, migrationConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service; SET search_path TO public,pg_temp`); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAccountIntegrationMigrations(t)
	if err != nil {
		t.Fatal(err)
	}
	if maximumVersion > 0 {
		bounded := make([]migrations.Migration, 0, len(loaded))
		for _, migration := range loaded {
			if migration.Version <= maximumVersion {
				bounded = append(bounded, migration)
			}
		}
		loaded = bounded
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)

	runtimeConfiguration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfiguration.MaxConns = 70
	runtimeConfiguration.ConnConfig.Database = fixtureDatabase
	runtimeConfiguration.AfterConnect = func(connectContext context.Context, connect *pgx.Conn) error {
		_, connectErr := connect.Exec(connectContext, `SET SESSION AUTHORIZATION account_service; SET ROLE account_service; SET search_path TO public,pg_temp`)
		return connectErr
	}
	pool, err = pgxpool.NewWithConfig(ctx, runtimeConfiguration)
	if err != nil {
		t.Fatal(err)
	}

	writerConfiguration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	writerConfiguration.MaxConns = 10
	writerConfiguration.ConnConfig.Database = fixtureDatabase
	writerConfiguration.AfterConnect = func(connectContext context.Context, connect *pgx.Conn) error {
		_, connectErr := connect.Exec(connectContext, `SET SESSION AUTHORIZATION account_approval_writer; SET ROLE account_approval_writer; SET search_path TO public,pg_temp`)
		return connectErr
	}
	writerPool, err = pgxpool.NewWithConfig(ctx, writerConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	relayConfiguration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	relayConfiguration.MaxConns = 10
	relayConfiguration.ConnConfig.Database = fixtureDatabase
	relayConfiguration.AfterConnect = func(connectContext context.Context, connect *pgx.Conn) error {
		_, connectErr := connect.Exec(connectContext, `SET SESSION AUTHORIZATION account_approval_relay; SET ROLE account_approval_relay; SET search_path TO public,pg_temp`)
		return connectErr
	}
	relayPool, err = pgxpool.NewWithConfig(ctx, relayConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	repository := &PostgresRepository{pool: pool, approvalWriterPool: writerPool, now: func() time.Time { return time.Now().UTC() }}
	return repository, connection, &ApprovalRelayStore{pool: relayPool}, loaded
}

// loadAccountIntegrationMigrations keeps the production public/control schema
// names intact in a dedicated fixture database. Only migration 11 is adapted to
// reuse the cluster extension already installed by the database owner.
func loadAccountIntegrationMigrations(t *testing.T) ([]migrations.Migration, error) {
	t.Helper()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		return nil, err
	}
	for index := range loaded {
		if loaded[index].Version != 11 {
			continue
		}
		const extension = "CREATE EXTENSION IF NOT EXISTS pgcrypto;"
		if count := strings.Count(loaded[index].UpSQL, extension); count != 1 {
			return nil, fmt.Errorf("migration 11 fixture adapter expected one pgcrypto creation, got %d", count)
		}
		loaded[index].UpSQL = strings.ReplaceAll(loaded[index].UpSQL, extension, "-- fixture: pgcrypto is preinstalled for the low-privilege migration role")
		normalized := strings.ReplaceAll(strings.ReplaceAll(loaded[index].UpSQL, "\r\n", "\n"), "\r", "\n")
		checksum := sha256.Sum256([]byte(normalized))
		loaded[index].Checksum = hex.EncodeToString(checksum[:])
	}
	return loaded, nil
}

func seedTokenOwnerCandidateActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role, suffix string) integrationActor {
	t.Helper()
	actor := integrationActor{ID: uuid.New(), Issuer: "https://issuer.integration.example", Subject: "owner-candidate-" + suffix}
	now := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,status,display_name,user_code,created_at,updated_at) VALUES($1,'ACTIVE',$2,generate_unique_user_code(),$3,$3)`, actor.ID, suffix, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, actor.Issuer, actor.Subject, actor.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES($1,$2,$3,'INTEGRATION_TEST','INTEGRATION_TEST',$4,$4)`, uuid.New(), actor.ID, role, now); err != nil {
		t.Fatal(err)
	}
	return actor
}

func seedTokenOwnerCandidateOrganizations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, creator uuid.UUID, count int) {
	t.Helper()
	organizationRows := make([][]any, 0, count)
	membershipRows := make([][]any, 0, count)
	ownerRows := make([][]any, 0, count)
	createdAt := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < count; index++ {
		organizationID, membershipID := uuid.New(), uuid.New()
		organizationRows = append(organizationRows, []any{organizationID, fmt.Sprintf("bulk-%04d", index), "ACTIVE", creator, createdAt, createdAt})
		membershipRows = append(membershipRows, []any{membershipID, organizationID, creator, "ACTIVE", 1, createdAt, createdAt})
		ownerRows = append(ownerRows, []any{uuid.New(), membershipID, "OWNER", creator, createdAt, createdAt})
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	inserted, err := tx.CopyFrom(ctx, pgx.Identifier{"organizations"}, []string{"id", "name", "status", "created_by", "created_at", "updated_at"}, pgx.CopyFromRows(organizationRows))
	if err != nil || inserted != int64(count) {
		t.Fatalf("seed organizations inserted=%d err=%v", inserted, err)
	}
	inserted, err = tx.CopyFrom(ctx, pgx.Identifier{"memberships"}, []string{"id", "organization_id", "user_id", "status", "version", "joined_at", "updated_at"}, pgx.CopyFromRows(membershipRows))
	if err != nil || inserted != int64(count) {
		t.Fatalf("seed memberships inserted=%d err=%v", inserted, err)
	}
	inserted, err = tx.CopyFrom(ctx, pgx.Identifier{"role_bindings"}, []string{"id", "membership_id", "role", "created_by", "created_at", "starts_at"}, pgx.CopyFromRows(ownerRows))
	if err != nil || inserted != int64(count) {
		t.Fatalf("seed organization owners inserted=%d err=%v", inserted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func authorizationVersions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userIDs ...uuid.UUID) []int64 {
	t.Helper()
	versions := make([]int64, len(userIDs))
	for index, userID := range userIDs {
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM'),1)`, userID).Scan(&versions[index]); err != nil {
			t.Fatal(err)
		}
	}
	return versions
}

func authorizationScopeVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, scope string, scopeID uuid.UUID) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope=$2 AND scope_id=$3),1)`, userID, scope, scopeID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func assertTokenOwnerCandidateGrants(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT role_code FROM role_permissions WHERE permission_code=$1 ORDER BY role_code`, PermissionPlatformTokenOwnerCandidatesRead)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, role)
	}
	if fmt.Sprint(roles) != fmt.Sprint([]string{"PLATFORM_MODEL_ADMIN", "PLATFORM_SUPER_ADMIN"}) {
		t.Fatalf("owner candidate roles=%v", roles)
	}
}

func firstTokenOwnerCandidateCursor(t *testing.T, repository *PostgresRepository, actor integrationActor) string {
	t.Helper()
	response := requestTokenOwnerCandidates(repository, actor, "")
	if response.Code != http.StatusOK {
		t.Fatalf("first cursor status=%d body=%s", response.Code, response.Body.String())
	}
	var page TokenOwnerCandidatePage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.NextCursor == nil {
		t.Fatalf("first cursor page=%+v err=%v", page, err)
	}
	return *page.NextCursor
}

func requestTokenOwnerCandidates(repository *PostgresRepository, actor integrationActor, cursor string) *httptest.ResponseRecorder {
	query := url.Values{"actor_issuer": {actor.Issuer}, "actor_subject": {actor.Subject}, "owner_type": {"USER"}, "limit": {"1"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/token-owner-candidates?"+query.Encode(), nil)
	request.Header.Set("Authorization", "Bearer integration-service-token")
	response := httptest.NewRecorder()
	NewService(repository, &fakeWalletProvisioner{}).WithServiceToken("integration-service-token").WithTokenOwnerCandidateCursorKey([]byte("0123456789abcdef0123456789abcdef")).Handler(nil).ServeHTTP(response, request)
	return response
}

func assertTokenOwnerCandidateRateLimit(t *testing.T, ctx context.Context, repository *PostgresRepository, actor TokenOwnerCandidateActor, attempts, expectedSuccess int) {
	t.Helper()
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_owner_candidate_query_attempts`); err != nil {
		t.Fatal(err)
	}
	var success, limited int
	for index := 0; index < attempts; index++ {
		_, err := repository.TokenOwnerCandidates(ctx, actor, TokenOwnerCandidateFilter{OwnerType: "UNIT", Limit: 50000})
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrTokenOwnerCandidateRateLimited):
			limited++
		default:
			t.Fatalf("candidate rate query: %v", err)
		}
	}
	if success != expectedSuccess || limited != attempts-expectedSuccess {
		t.Fatalf("rate boundary success=%d limited=%d", success, limited)
	}
}

func assertSharedServiceLimit(t *testing.T, ctx context.Context, repository *PostgresRepository, actors []TokenOwnerCandidateActor) {
	t.Helper()
	var success, limited int
	for index := 0; index < 61; index++ {
		actor := actors[index%len(actors)]
		_, err := repository.TokenOwnerCandidates(ctx, actor, TokenOwnerCandidateFilter{OwnerType: "UNIT", Limit: 50000})
		if err == nil {
			success++
		} else if errors.Is(err, ErrTokenOwnerCandidateRateLimited) {
			limited++
		} else {
			t.Fatalf("shared-service query: %v", err)
		}
	}
	if success != 60 || limited != 1 {
		t.Fatalf("service boundary success=%d limited=%d", success, limited)
	}
}
