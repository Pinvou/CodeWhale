package account

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProvisioningPasswordLoginFixtureUsesCurrentAccountAuthorization(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is required for actual PostgreSQL")
	}
	var fixture struct {
		Username string    `json:"username"`
		Issuer   string    `json:"identity_issuer"`
		Subject  string    `json:"identity_subject"`
		UserID   uuid.UUID `json:"account_user_id"`
		Status   string    `json:"account_status"`
		Allowed  bool      `json:"authorization_allowed"`
	}
	bytes, err := os.ReadFile("../../test/fixtures/provisioning-password-login.json")
	if err != nil || json.Unmarshal(bytes, &fixture) != nil || fixture.UserID == uuid.Nil || fixture.Status != "PROVISIONING" || !fixture.Allowed {
		t.Fatal("controlled cross-language fixture is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("private database connection failed")
	}
	defer connection.Close(context.Background())
	schema := "spi_login_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = connection.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("owned schema creation failed")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, err := connection.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned schema cleanup failed")
		}
		var remains bool
		if err := connection.QueryRow(cleanup, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&remains); err != nil || remains {
			t.Error("owned schema absence unconfirmed")
		}
	}()
	if _, err = connection.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal("owned schema selection failed")
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal("installed migrations loading failed")
	}
	if err = accountschema.Up(ctx, connection, installed); err != nil {
		t.Fatal("installed migrations failed")
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("owned pool configuration failed")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("owned pool creation failed")
	}
	defer pool.Close()
	// Controlled durable identity and role facts exercise the real Authorize
	// query, not a mocked AuthorizationDecision or a future Token dependency.
	if _, err = connection.Exec(ctx, `INSERT INTO users(id,status,username,user_code,created_at,updated_at) VALUES($1,$2,$3,generate_unique_user_code(),now(),now())`, fixture.UserID, fixture.Status, fixture.Username); err != nil {
		t.Fatal("controlled Account user seed failed")
	}
	if _, err = connection.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,now(),now())`, fixture.Issuer, fixture.Subject, fixture.UserID); err != nil {
		t.Fatal("controlled issuer/subject seed failed")
	}
	if _, err = connection.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,starts_at,created_at) VALUES($1,$2,'PERSONAL_USER','ACCOUNT_LIFECYCLE','ACCOUNT_LIFECYCLE',now(),now())`, uuid.New(), fixture.UserID); err != nil {
		t.Fatal("controlled personal role seed failed")
	}
	repository := &PostgresRepository{pool: pool, now: time.Now}
	principal := identity.Principal{Issuer: fixture.Issuer, Subject: fixture.Subject}
	decision, err := repository.Authorize(ctx, principal, PermissionPersonalSessionLogin, nil, nil)
	if err != nil || decision.Allowed != fixture.Allowed || decision.UserID != fixture.UserID || decision.UserStatus != fixture.Status || decision.Permission != PermissionPersonalSessionLogin || decision.PermissionScope != "PERSONAL" {
		t.Fatal("actual Account decision does not match the Java fixture")
	}
	billing, err := repository.Authorize(ctx, principal, PermissionPersonalBillingRead, nil, nil)
	if err != nil || billing.Allowed {
		t.Fatal("PROVISIONING login must not grant personal billing access")
	}
	if _, err = connection.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1,updated_at=now() WHERE id=$1`, fixture.UserID); err != nil {
		t.Fatal("controlled disable failed")
	}
	disabled, err := repository.Authorize(ctx, principal, PermissionPersonalSessionLogin, nil, nil)
	if err != nil || disabled.Allowed || disabled.UserStatus != "DISABLED" {
		t.Fatal("disabled user may not log in")
	}
	foreign, err := repository.Authorize(ctx, identity.Principal{Issuer: fixture.Issuer, Subject: "foreign-controlled-subject"}, PermissionPersonalSessionLogin, nil, nil)
	if err == nil || foreign.Allowed {
		t.Fatal("a different subject must not inherit the fixture identity")
	}
}
