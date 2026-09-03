package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
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
		t.Skip("ACCOUNT_DATABASE_URL is not configured; PostgreSQL integration was not executed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, connection, loaded := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL)
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
	if _, err := repository.pool.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'late organization','ACTIVE',$2,clock_timestamp(),clock_timestamp())`, lateOrganizationID, superActor.ID); err != nil {
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
	if total != 1601 || time.Since(scanStarted) > 10*time.Second {
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
	for _, organization := range []struct {
		id     uuid.UUID
		status string
	}{{activeOrganization, "ACTIVE"}, {disabledOrganization, "DISABLED"}, {deletedOrganization, "DELETED"}} {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, organization.id, "candidate-"+strings.ToLower(organization.status), organization.status, creator, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	activeUnit, disabledUnit, deletedUnit := uuid.New(), uuid.New(), uuid.New()
	for _, unit := range []struct {
		id     uuid.UUID
		status string
	}{{activeUnit, "ACTIVE"}, {disabledUnit, "DISABLED"}, {deletedUnit, "DELETED"}} {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO organization_units(id,organization_id,parent_unit_id,name,status,created_at,updated_at) VALUES($1,$2,NULL,$3,$4,$5,$5)`, unit.id, activeOrganization, "candidate-"+strings.ToLower(unit.status), unit.status, createdAt); err != nil {
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

type integrationActor struct {
	ID      uuid.UUID
	Issuer  string
	Subject string
}

func (actor integrationActor) TokenActor(service, subject string) TokenOwnerCandidateActor {
	return TokenOwnerCandidateActor{Issuer: actor.Issuer, Subject: actor.Subject, CallerService: service, CallerSubject: subject}
}

func newTokenOwnerCandidateIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string) (*PostgresRepository, *pgx.Conn, []migrations.Migration) {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "account_owner_candidates_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 70
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = make(map[string]string)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
	return repository, connection, loaded
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
	rows := make([][]any, 0, count)
	createdAt := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < count; index++ {
		rows = append(rows, []any{uuid.New(), fmt.Sprintf("bulk-%04d", index), "ACTIVE", creator, createdAt, createdAt})
	}
	inserted, err := pool.CopyFrom(ctx, pgx.Identifier{"organizations"}, []string{"id", "name", "status", "created_by", "created_at", "updated_at"}, pgx.CopyFromRows(rows))
	if err != nil || inserted != int64(count) {
		t.Fatalf("seed organizations inserted=%d err=%v", inserted, err)
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
	var success atomic.Int64
	var limited atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < attempts; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.TokenOwnerCandidates(ctx, actor, TokenOwnerCandidateFilter{OwnerType: "UNIT", Limit: 1})
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, ErrTokenOwnerCandidateRateLimited):
				limited.Add(1)
			default:
				t.Errorf("concurrent candidate query: %v", err)
			}
		}()
	}
	wait.Wait()
	if success.Load() != int64(expectedSuccess) || limited.Load() != int64(attempts-expectedSuccess) {
		t.Fatalf("rate boundary success=%d limited=%d", success.Load(), limited.Load())
	}
}

func assertSharedServiceLimit(t *testing.T, ctx context.Context, repository *PostgresRepository, actors []TokenOwnerCandidateActor) {
	t.Helper()
	var success atomic.Int64
	var limited atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < 61; index++ {
		actor := actors[index%len(actors)]
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.TokenOwnerCandidates(ctx, actor, TokenOwnerCandidateFilter{OwnerType: "UNIT", Limit: 1})
			if err == nil {
				success.Add(1)
			} else if errors.Is(err, ErrTokenOwnerCandidateRateLimited) {
				limited.Add(1)
			} else {
				t.Errorf("shared-service query: %v", err)
			}
		}()
	}
	wait.Wait()
	if success.Load() != 60 || limited.Load() != 1 {
		t.Fatalf("service boundary success=%d limited=%d", success.Load(), limited.Load())
	}
}
