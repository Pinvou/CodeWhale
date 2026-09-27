package account

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestProvisioningPasswordLoginAccountChainPostgresIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("the real PostgreSQL registration login chain runs in its dedicated CI step")
	}
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	var fixture struct {
		IdentityIssuer       string `json:"identity_issuer"`
		IdentitySubject      string `json:"identity_subject"`
		AccountUserID        string `json:"account_user_id"`
		AccountStatus        string `json:"account_status"`
		AuthorizationAllowed bool   `json:"authorization_allowed"`
		Username             string `json:"username"`
	}
	rawFixture, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "provisioning-password-login.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	userID, err := uuid.Parse(fixture.AccountUserID)
	if err != nil || fixture.AccountStatus != "PROVISIONING" || !fixture.AuthorizationAllowed {
		t.Fatalf("invalid provisioning login fixture: %+v err=%v", fixture, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, _, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 0)
	repository.walletPolicyVersion = "registration-login-fixture-v1"
	repository.walletProvisioningSLA = 15 * time.Minute
	now := time.Now().UTC().Add(-time.Minute)
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,created_at,updated_at)
VALUES($1,generate_unique_user_code(),'PROVISIONING',$2,$2,$3,$3)`, userID, fixture.Username, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, fixture.IdentityIssuer, fixture.IdentitySubject, userID, now); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultPersonalRole(ctx, tx, userID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	principal := identity.Principal{Issuer: fixture.IdentityIssuer, Subject: fixture.IdentitySubject, SessionID: "provisioning-login-fixture-session", Username: fixture.Username}
	decision, err := repository.Authorize(ctx, principal, PermissionPersonalSessionLogin, nil, nil)
	if err != nil || !decision.Allowed || decision.UserID != userID || decision.UserStatus != "PROVISIONING" || decision.PermissionScope != "PERSONAL" {
		t.Fatalf("provisioning login authorization=%+v err=%v", decision, err)
	}
	me, created, err := repository.Bootstrap(ctx, principal)
	if err != nil || created || me.UserID != userID || me.Status != "PROVISIONING" || !slices.Contains(me.PersonalPermissions, PermissionPersonalSessionLogin) {
		t.Fatalf("provisioning bootstrap=%+v created=%v err=%v", me, created, err)
	}
}
