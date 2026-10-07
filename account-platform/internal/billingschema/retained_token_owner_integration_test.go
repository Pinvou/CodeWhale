package billingschema

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Public Account migrations, genuine migrator/service actors and actual
// authority/directory HTTP handlers; controlled development workload identity.
func TestF04PublicTokenUserOwnerAuthorityAndDirectory(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit isolated PostgreSQL fixture required")
	}
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	all, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, all); err != nil {
		t.Fatal("public Account migrations", err)
	}
	if _, err = migrations.VerifyApplied(ctx, c, all); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, c)
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	issuer := "https://owned.invalid"
	subject := uuid.NewString()
	actor := identity.Principal{Issuer: issuer, Subject: subject, SessionID: uuid.NewString()}
	bootstrap := func(principal identity.Principal) uuid.UUID {
		t.Helper()
		me, _, err := r.Bootstrap(ctx, principal)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := r.LeaseWalletProvisioningJobs(ctx, 20, 30*time.Second)
		if err != nil || len(jobs) != 1 || jobs[0].UserID != me.UserID {
			t.Fatalf("actual wallet job claim failed: %v", err)
		}
		if err = r.CompleteWalletProvisioning(ctx, me.UserID); err != nil {
			t.Fatal(err)
		}
		return me.UserID
	}
	actorID := bootstrap(actor)
	ownerID := bootstrap(identity.Principal{Issuer: issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()})
	var ownerVersion int64
	if err := c.QueryRow(ctx, `SELECT version FROM users WHERE id=$1`, ownerID).Scan(&ownerVersion); err != nil || ownerVersion < 1 {
		t.Fatalf("real owner version unavailable: %v", err)
	}
	binding := uuid.New()
	if _, err = c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_MODEL_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, binding, actorID); err != nil {
		t.Fatal(err)
	}
	service := account.NewService(r, nil).WithServiceToken("controlled-service").WithTokenOwnerCandidateCursorKey(bytes.Repeat([]byte{7}, 32))
	handler := service.Handler(func(context.Context, string) (identity.Principal, error) { return actor, nil })
	send := func(method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer controlled-service")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	sref := func(s string) *string { return &s }
	digest := sha256.Sum256([]byte(subject))
	owner := ownerID.String()
	base := tokenauthority.Request{AuthoritySchemaVersion: "1", Mode: tokenauthority.ModeT1Snapshot, IdentityIssuer: sref(issuer), IdentitySubject: sref(subject), IdentitySubjectSHA256: sref(hex.EncodeToString(digest[:])), OwnerType: sref("USER"), OwnerID: &owner, RequiredPermissions: append([]string{}, tokenauthority.RequiredPermissions...), CommandReference: uuid.NewString(), CommandRequestDigest: sref(strings.Repeat("a", 64))}
	request := func(input tokenauthority.Request) *httptest.ResponseRecorder {
		input.AuthorityRequestSHA256 = input.Digest()
		body, _ := json.Marshal(input)
		return send(http.MethodPost, "/internal/v1/token-owner-authority", body)
	}
	var allowed tokenauthority.Response
	t.Run("live-user-authority", func(t *testing.T) {
		w := request(base)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &allowed) != nil || allowed.Decision != "ALLOW" || allowed.ActorUserID != actorID.String() || allowed.ObservedOwnerVersion == nil || *allowed.ObservedOwnerVersion != strconv.FormatInt(ownerVersion, 10) || allowed.OwnerIdentityDigest == nil || allowed.OwnerOrganizationID != nil {
			t.Fatalf("authority decision: status=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("operator-authority", func(t *testing.T) {
		input := base
		input.Mode = tokenauthority.ModeOperatorAuthorize
		input.OwnerType = nil
		input.OwnerID = nil
		input.CommandRequestDigest = nil
		w := request(input)
		var result tokenauthority.Response
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Decision != "ALLOW" || result.OwnerStatus != nil {
			t.Fatalf("operator authority %d", w.Code)
		}
	})
	t.Run("disabled-owner-denied", func(t *testing.T) {
		if _, err := c.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1 WHERE id=$1`, ownerID); err != nil {
			t.Fatal(err)
		}
		w := request(base)
		var result tokenauthority.Response
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Decision != "DENY" || result.ReasonCode == nil || *result.ReasonCode != "OWNER_INACTIVE" {
			t.Fatalf("disabled owner %d %s", w.Code, w.Body.String())
		}
		if _, err := c.Exec(ctx, `UPDATE users SET status='ACTIVE',version=version+1 WHERE id=$1`, ownerID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("future-organization-owner-rejected", func(t *testing.T) {
		input := base
		input.OwnerType = sref("ORGANIZATION")
		if w := request(input); w.Code != 400 {
			t.Fatalf("future owner accepted %d", w.Code)
		}
	})
	t.Run("malformed-authority-rejected", func(t *testing.T) {
		if w := send(http.MethodPost, "/internal/v1/token-owner-authority", []byte("{")); w.Code != 400 {
			t.Fatalf("malformed input returned %d", w.Code)
		}
	})
	directoryPath := "/internal/v1/token-owner-candidates?actor_issuer=https%3A%2F%2Fowned.invalid&actor_subject=" + subject + "&owner_type=USER&limit=1"
	var firstCursor string
	t.Run("real-user-directory-pagination", func(t *testing.T) {
		path := directoryPath
		seen := map[uuid.UUID]bool{}
		for pageNo := 0; pageNo < 10; pageNo++ {
			w := send(http.MethodGet, path, nil)
			var page account.TokenOwnerCandidatePage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.AuthorizationVersion < 1 || len(page.Items) != 1 {
				t.Fatalf("directory page %d %s", w.Code, w.Body.String())
			}
			item := page.Items[0]
			if item.Type != "USER" || seen[item.ID] || page.OwnerVersions[item.ID.String()] < 1 || strings.Contains(w.Body.String(), `"email"`) || strings.Contains(w.Body.String(), `"mobile"`) {
				t.Fatal("invalid directory projection")
			}
			seen[item.ID] = true
			if !page.HasMore {
				break
			}
			if page.NextCursor == nil {
				t.Fatal("missing cursor")
			}
			if firstCursor == "" {
				firstCursor = *page.NextCursor
			}
			path = directoryPath + "&cursor=" + *page.NextCursor
		}
		if !seen[actorID] || !seen[ownerID] {
			t.Fatal("real users not enumerated")
		}
	})
	t.Run("future-directory-filter-rejected", func(t *testing.T) {
		if w := send(http.MethodGet, strings.Replace(directoryPath, "owner_type=USER", "owner_type=ORGANIZATION", 1), nil); w.Code != 400 {
			t.Fatalf("future directory accepted %d", w.Code)
		}
	})
	t.Run("revoked-actor-authority-denied", func(t *testing.T) {
		if _, err := c.Exec(ctx, `UPDATE role_assignments SET revoked_at=now() WHERE id=$1`, binding); err != nil {
			t.Fatal(err)
		}
		w := request(base)
		var result tokenauthority.Response
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Decision != "DENY" || result.ReasonCode == nil || *result.ReasonCode != "AUTHORIZATION_REVOKED" {
			t.Fatalf("revoked authority %d %s", w.Code, w.Body.String())
		}
		if w := send(http.MethodGet, directoryPath, nil); w.Code != 403 {
			t.Fatalf("revoked directory %d", w.Code)
		}
	})
	t.Run("changed-authorization-cursor-expired", func(t *testing.T) {
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_MODEL_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, uuid.New(), actorID); err != nil {
			t.Fatal(err)
		}
		if firstCursor == "" {
			t.Fatal("first cursor missing")
		}
		if w := send(http.MethodGet, directoryPath+"&cursor="+firstCursor, nil); w.Code != 410 {
			t.Fatalf("changed cursor returned %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("disabled-while-waiting-for-directory-lock", func(t *testing.T) {
		operator := identity.Principal{Issuer: issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
		operatorID := bootstrap(operator)
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_IDENTITY_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, uuid.New(), operatorID); err != nil {
			t.Fatal(err)
		}
		locked, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer locked.Rollback(context.Background())
		if _, err := locked.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('token-owner-actor:'||$1::text,0))`, actorID); err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			_, err := r.TokenOwnerCandidates(ctx, account.TokenOwnerCandidateActor{Issuer: issuer, Subject: subject, CallerService: "owned-directory", CallerSubject: "owned-workload"}, account.TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1})
			finished <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND wait_event='advisory' AND query LIKE '%token-owner-actor:%')`, c.Config().Database).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("directory did not reach actual advisory lock wait")
			}
			time.Sleep(5 * time.Millisecond)
		}
		adminHandler := service.Handler(func(context.Context, string) (identity.Principal, error) { return operator, nil })
		req := httptest.NewRequest(http.MethodPatch, "/admin/v1/users/"+actorID.String()+"/status", strings.NewReader(`{"status":"DISABLED"}`))
		req.Header.Set("Authorization", "Bearer controlled-user")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		adminHandler.ServeHTTP(response, req)
		if response.Code != http.StatusNoContent {
			t.Fatalf("legitimate administrator disable failed: status=%d", response.Code)
		}
		if err := locked.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-finished:
			if !errors.Is(err, account.ErrForbidden) {
				t.Fatalf("disabled actor directory must be forbidden, observed=%v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("directory did not finish")
		}
	})

	t.Run("expired-while-waiting-for-directory-lock", func(t *testing.T) {
		// This actor has exactly one permission assignment, independent of the
		// earlier cursor-version test's newly assigned role.
		subject := uuid.NewString()
		actorID := bootstrap(identity.Principal{Issuer: issuer, Subject: subject, SessionID: uuid.NewString()})
		binding := uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_MODEL_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, binding, actorID); err != nil {
			t.Fatal(err)
		}

		operator := identity.Principal{Issuer: issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
		operatorID := bootstrap(operator)
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_IDENTITY_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, uuid.New(), operatorID); err != nil {
			t.Fatal(err)
		}
		locked, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer locked.Rollback(context.Background())
		if _, err := locked.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('token-owner-actor:'||$1::text,0))`, actorID); err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			_, err := r.TokenOwnerCandidates(ctx, account.TokenOwnerCandidateActor{Issuer: issuer, Subject: subject, CallerService: "owned-directory", CallerSubject: "owned-workload"}, account.TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1})
			finished <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND wait_event='advisory' AND query LIKE '%token-owner-actor:%')`, c.Config().Database).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("directory did not reach actual advisory lock wait")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := c.Exec(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()+make_interval(secs=>0.1) WHERE id=$1`, binding); err != nil {
			t.Fatal(err)
		}
		for {
			var expired bool
			if err := locked.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM role_assignments WHERE id=$1`, binding).Scan(&expired); err != nil {
				t.Fatal(err)
			}
			if expired {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("actual assigned role did not expire")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := locked.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-finished:
			if !errors.Is(err, account.ErrForbidden) {
				t.Fatalf("expired actor directory must be forbidden, observed=%v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("directory did not finish")
		}
	})

	t.Run("revoked-while-waiting-for-version-lock", func(t *testing.T) {
		subject := uuid.NewString()
		actorID := bootstrap(identity.Principal{Issuer: issuer, Subject: subject, SessionID: uuid.NewString()})
		binding := uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_MODEL_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, binding, actorID); err != nil {
			t.Fatal(err)
		}

		operator := identity.Principal{Issuer: issuer, Subject: uuid.NewString(), SessionID: uuid.NewString()}
		operatorID := bootstrap(operator)
		if _, err := c.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_IDENTITY_ADMIN','ACCOUNT',now()-interval '1 minute',now())`, uuid.New(), operatorID); err != nil {
			t.Fatal(err)
		}
		locked, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer locked.Rollback(context.Background())
		if _, err := locked.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE id=$1`, binding); err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			_, err := r.TokenOwnerCandidates(ctx, account.TokenOwnerCandidateActor{Issuer: issuer, Subject: subject, CallerService: "owned-directory", CallerSubject: "owned-workload"}, account.TokenOwnerCandidateFilter{OwnerType: "USER", Limit: 1})
			finished <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if err := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND wait_event='transactionid' AND (query LIKE '%role_assignments assignment%' OR query LIKE '%SELECT version FROM authorization_versions%'))`, c.Config().Database).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("directory did not reach actual authorization-version lock wait")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := locked.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-finished:
			if !errors.Is(err, account.ErrForbidden) {
				t.Fatalf("revoked actor directory must be forbidden, observed=%v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("directory did not finish")
		}
	})

}
