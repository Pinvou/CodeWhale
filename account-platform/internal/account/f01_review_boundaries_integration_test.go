package account

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestF01ReviewBoundariesPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	t.Run("password-audit-session", func(t *testing.T) {
		p, _ := f01ActiveFixtureUser(t, r)
		op, err := r.CreateAuthenticatedPasswordReset(ctx, p, "controlled-reference", "controlled-policy")
		if err != nil {
			t.Fatal(err)
		}
		var sid string
		if err = r.pool.QueryRow(ctx, `SELECT session_id FROM audit_logs WHERE action='account.password_change.verified' AND resource_id=$1`, op.ID.String()).Scan(&sid); err != nil || sid != p.SessionID || sid == p.Subject {
			t.Fatal("audit did not preserve session identity", err)
		}
	})
	t.Run("mixed-case-bootstrap-rename", func(t *testing.T) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "Mixed" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}
		if _, _, err := r.Bootstrap(ctx, p); err != nil {
			t.Fatal(err)
		}
		other, _ := f01ActiveFixtureUser(t, r)
		if _, err := r.CreateProfileChange(ctx, other, "USERNAME", strings.ToLower(p.Username)); !errors.Is(err, ErrUsernameAlreadyRegistered) {
			t.Fatal("mixed-case existing name was not rejected at precheck", err)
		}
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM profile_change_requests WHERE user_id=(SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2)`, other.Issuer, other.Subject).Scan(&count); err != nil || count != 0 {
			t.Fatal("rejected rename persisted an intent", err)
		}
	})
	t.Run("live-session-version-projection", func(t *testing.T) {
		p, user := f01ActiveFixtureUser(t, r)
		for _, scope := range []string{"PERSONAL", "PLATFORM"} {
			if _, err := r.pool.Exec(ctx, `INSERT INTO authorization_versions(user_id,scope,scope_id,version,updated_at) VALUES($1,$2,'00000000-0000-0000-0000-000000000000',9,now()) ON CONFLICT(user_id,scope,scope_id) DO UPDATE SET version=9`, user, scope); err != nil {
				t.Fatal(err)
			}
		}
		me, err := r.GetMe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.GetAuthorizationVersions(ctx, p)
		if err != nil || v.ContextVersion != me.ActiveContext.ContextVersion || v.AuthorizationVersion != me.ActiveContext.AuthorizationVersion || v.PersonalAuthorizationVersion != 9 || v.PlatformAuthorizationVersion != 9 || v.OrganizationAuthorizationVersion != me.ActiveContext.OrganizationAuthorizationVersion {
			t.Fatal("live versions differ", err)
		}
		org := uuid.New()
		if _, err = r.pool.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'controlled organization','ACTIVE',$2,now(),now())`, org, user); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE user_active_contexts SET context_type='ORGANIZATION',organization_id=$2,context_version=context_version+1,updated_at=now() WHERE user_id=$1`, user, org); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO authorization_versions(user_id,scope,scope_id,version,updated_at) VALUES($1,'ORGANIZATION',$2,12,now())`, user, org); err != nil {
			t.Fatal(err)
		}
		me, err = r.GetMe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		v, err = r.GetAuthorizationVersions(ctx, p)
		if err != nil || v.ContextVersion != me.ActiveContext.ContextVersion || v.AuthorizationVersion != 12 || v.OrganizationAuthorizationVersion != 12 || v.PersonalAuthorizationVersion != 9 || v.PlatformAuthorizationVersion != 9 {
			t.Fatal("organization context did not resolve all five live versions", err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE authorization_versions SET version=13,updated_at=now() WHERE user_id=$1 AND scope='ORGANIZATION' AND scope_id=$2`, user, org); err != nil {
			t.Fatal(err)
		}
		v, err = r.GetAuthorizationVersions(ctx, p)
		if err != nil || v.AuthorizationVersion != 13 || v.OrganizationAuthorizationVersion != 13 {
			t.Fatal("version projection cached stale values", err)
		}
		p.SessionID = uuid.NewString()
		if _, err = r.GetAuthorizationVersions(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Fatal("foreign session resolved", err)
		}
	})
	for _, expired := range []bool{false, true} {
		name := "captcha-attempt-write-error"
		if expired {
			name = "captcha-expiry-write-error"
		}
		t.Run(name, func(t *testing.T) {
			cfg := f.config.Copy()
			cfg.Database = r.pool.Config().ConnConfig.Database
			admin, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal("owned fault setup connection failed")
			}
			defer func() {
				if err := admin.Close(ctx); err != nil {
					t.Error("owned fault setup close failed", err)
				}
			}()
			// A test-only trigger injects a genuine UPDATE fault; no production
			// schema, grants, migration bytes or runtime identity are changed.
			if _, err = admin.Exec(ctx, `CREATE FUNCTION public.f01_captcha_write_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled captcha write fault' USING ERRCODE='P0001'; END $$; CREATE TRIGGER f01_captcha_write_fault BEFORE UPDATE ON slider_captcha_challenges FOR EACH ROW EXECUTE FUNCTION public.f01_captcha_write_fault()`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := admin.Exec(ctx, `DROP TRIGGER f01_captcha_write_fault ON slider_captcha_challenges; DROP FUNCTION public.f01_captcha_write_fault()`); err != nil {
					t.Error(err)
				}
			}()
			id := uuid.New()
			expiry := time.Now().Add(time.Minute)
			if expired {
				expiry = time.Now().Add(-time.Minute)
			}
			if _, err = r.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138000','LOGIN',20,'CREATED',$2,now(),now())`, id, expiry); err != nil {
				t.Fatal(err)
			}
			err = r.ConsumeSliderCaptcha(ctx, id, "+8613800138000", SMSPurposeLogin, 100)
			var pgerr *pgconn.PgError
			if errors.Is(err, ErrCaptchaInvalid) || !errors.As(err, &pgerr) || pgerr.Code != "P0001" {
				t.Fatal("database write fault masked as invalid captcha", err)
			}
			var attempts int
			var status string
			if err = r.pool.QueryRow(ctx, `SELECT attempts,status FROM slider_captcha_challenges WHERE id=$1`, id).Scan(&attempts, &status); err != nil || attempts != 0 || status != "CREATED" {
				t.Fatal("failed transaction changed challenge", err)
			}
		})
	}
}
