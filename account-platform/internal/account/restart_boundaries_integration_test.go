package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type closingBoundaryRepository struct{ *PostgresRepository }

func (r closingBoundaryRepository) ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error) {
	return AuthorizationContext{}, ErrAccountClosing
}

func TestRestartAccountProfileAndAuthorizationBoundaries(t *testing.T) {
	fixture := retainedWalletFenceActors(t)
	repo := fixture.repository(t) // Entire current migration chain, genuine runtime actor.
	ctx := context.Background()
	makeUser := func(t *testing.T) (identity.Principal, uuid.UUID) {
		t.Helper()
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "u" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := repo.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	var mobileSequence int
	bound := func(t *testing.T, p identity.Principal, user uuid.UUID, expired bool) (ProfileChange, uuid.UUID) {
		t.Helper()
		mobileSequence++
		c, err := repo.CreateProfileChange(ctx, p, "MOBILE", fmt.Sprintf("+86138%08d", mobileSequence))
		if err != nil {
			t.Fatal(err)
		}
		sms := uuid.New()
		if _, err = repo.pool.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,provider_id,adapter_version,expires_at,created_at,updated_at) VALUES($1,$2,'PROFILE_MOBILE','SENT','fixture','v1',clock_timestamp()+interval '1 hour',clock_timestamp(),clock_timestamp())`, sms, c.NewValue); err != nil {
			t.Fatal(err)
		}
		expiry := "1 hour"
		if expired {
			expiry = "-1 hour"
		}
		if _, err = repo.pool.Exec(ctx, `UPDATE profile_change_requests SET sms_challenge_id=$2,sla_expires_at=clock_timestamp()+$3::interval WHERE id=$1`, c.ID, sms, expiry); err != nil {
			t.Fatal(err)
		}
		return c, sms
	}
	t.Run("live bound request stays single flight", func(t *testing.T) {
		p, u := makeUser(t)
		old, _ := bound(t, p, u, false)
		if _, err := repo.CreateProfileChange(ctx, p, "MOBILE", "+8613800138002"); !errors.Is(err, ErrProfileChangeConflict) {
			t.Fatalf("live binding replacement: %v", err)
		}
		same, err := repo.CreateProfileChange(ctx, p, "MOBILE", old.NewValue)
		if err != nil || same.ID != old.ID {
			t.Fatal("live same-value identity changed")
		}
	})
	for _, target := range []string{"same", "different", "field"} {
		t.Run("expired "+target, func(t *testing.T) {
			p, u := makeUser(t)
			old, sms := bound(t, p, u, true)
			field, value := "MOBILE", old.NewValue
			if target == "different" {
				value = fmt.Sprintf("+86139%08d", mobileSequence)
			}
			if target == "field" {
				field = "USERNAME"
				value = "new" + uuid.NewString()[:8]
			}
			// Local time skew must not change the expired decision.
			saved := repo.now
			repo.now = func() time.Time { return time.Now().Add(-24 * time.Hour) }
			fresh, err := repo.CreateProfileChange(ctx, p, field, value)
			repo.now = saved
			if err != nil || fresh.ID == old.ID {
				t.Fatalf("expired replacement: %v", err)
			}
			if !fresh.SLAExpiresAt.After(time.Now()) {
				t.Fatal("new SLA used skewed application time")
			}
			preserved, err := repo.ProfileChange(ctx, old.ID, nil)
			if err != nil || preserved.Status != "SUPERSEDED" || preserved.SMSChallengeID == nil || *preserved.SMSChallengeID != sms {
				t.Fatal("old evidence not preserved")
			}
			var audit int
			if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action='account.profile_change.superseded'`, old.ID.String()).Scan(&audit); err != nil || audit != 1 {
				t.Fatal("supersede audit missing")
			}
			if err = repo.CommitProfileSMSVerification(ctx, old.ID, u, sms, true); !errors.Is(err, ErrProfileChangeConflict) {
				t.Fatal("old SMS changed superseded request")
			}
			if err = repo.CommitProfileSMSVerification(ctx, fresh.ID, u, sms, true); !errors.Is(err, ErrProfileChangeConflict) {
				t.Fatal("old SMS bound to new request")
			}
			unchanged, err := repo.ProfileChange(ctx, fresh.ID, nil)
			if err != nil || unchanged.SMSVerifiedAt != nil {
				t.Fatal("old SMS affected new request")
			}
		})
	}
	t.Run("failed replacement rolls back supersede and audit", func(t *testing.T) {
		p, u := makeUser(t)
		old, sms := bound(t, p, u, true)
		_, other := makeUser(t)
		if _, err := repo.pool.Exec(ctx, `UPDATE users SET mobile_e164='+8613700138000' WHERE id=$1`, other); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CreateProfileChange(ctx, p, "MOBILE", "+8613700138000"); !errors.Is(err, ErrMobileAlreadyRegistered) {
			t.Fatalf("expected duplicate mobile: %v", err)
		}
		preserved, err := repo.ProfileChange(ctx, old.ID, nil)
		if err != nil || preserved.Status != "PENDING" || preserved.SMSChallengeID == nil || *preserved.SMSChallengeID != sms {
			t.Fatal("failed replacement discarded old evidence")
		}
		var audit int
		if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action='account.profile_change.superseded'`, old.ID.String()).Scan(&audit); err != nil || audit != 0 {
			t.Fatal("failed replacement left supersede audit")
		}
	})
	for _, column := range []string{"sms_verified_at", "identity_updated_at", "account_committed_at", "sessions_revoked_at"} {
		t.Run("preserve proof "+column, func(t *testing.T) {
			p, u := makeUser(t)
			old, _ := bound(t, p, u, true)
			if _, err := repo.pool.Exec(ctx, `UPDATE profile_change_requests SET `+column+`=clock_timestamp() WHERE id=$1`, old.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.CreateProfileChange(ctx, p, "MOBILE", "+8613800138002"); !errors.Is(err, ErrProfileChangeConflict) {
				t.Fatal("proof-bearing request discarded")
			}
		})
	}
	for _, status := range []string{"VERIFIED", "IDENTITY_UPDATED", "ACCOUNT_COMMITTED", "RETRY", "REPAIR_REQUIRED"} {
		t.Run("preserve state "+status, func(t *testing.T) {
			p, u := makeUser(t)
			old, _ := bound(t, p, u, true)
			if _, err := repo.pool.Exec(ctx, `UPDATE profile_change_requests SET status=$2 WHERE id=$1`, old.ID, status); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.CreateProfileChange(ctx, p, "MOBILE", "+8613800138002"); !errors.Is(err, ErrProfileChangeConflict) {
				t.Fatal("non-pending state discarded")
			}
		})
	}
	t.Run("verification and expiry replacement serialize", func(t *testing.T) {
		p, u := makeUser(t)
		old, sms := bound(t, p, u, true)
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		var createErr, verifyErr error
		go func() {
			defer wg.Done()
			<-start
			_, createErr = repo.CreateProfileChange(ctx, p, "MOBILE", fmt.Sprintf("+86139%08d", mobileSequence))
		}()
		go func() {
			defer wg.Done()
			<-start
			verifyErr = repo.CommitProfileSMSVerification(ctx, old.ID, u, sms, true)
		}()
		close(start)
		wg.Wait()
		if (createErr == nil) == (verifyErr == nil) {
			t.Fatalf("winner count invalid: create=%v verify=%v", createErr, verifyErr)
		}
		loser := createErr
		if loser == nil {
			loser = verifyErr
		}
		if !errors.Is(loser, ErrProfileChangeConflict) {
			t.Fatalf("unexpected race error: %v", loser)
		}
	})
	t.Run("closing is HTTP403 business state", func(t *testing.T) {
		p, _ := makeUser(t)
		// F01's users CHECK does not yet accept CLOSING. Exercise the typed
		// repository boundary without weakening that DDL or importing F08.
		service := &Service{repository: closingBoundaryRepository{repo}}
		handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) { return p, nil }, http.HandlerFunc(service.getAuthorizationContext))
		req := httptest.NewRequest(http.MethodGet, "/v1/me/authorization-context", nil)
		req.Header.Set("Authorization", "Bearer fixture")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != http.StatusForbidden || !strings.Contains(out.Body.String(), `"code":"ACCOUNT_CLOSING"`) {
			t.Fatalf("closing response: %d %s", out.Code, out.Body.String())
		}
	})
	t.Run("infrastructure error remains an error", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := repo.AuthorizeComposite(cancelled, identity.Principal{}, []string{PermissionPlatformRegistrationsRepair}, nil)
		if err == nil || errors.Is(err, ErrForbidden) {
			t.Fatalf("infra error became denial: %v", err)
		}
		service := &Service{repository: repo}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/permissions/check", strings.NewReader(`{"issuer":"https://identity.example.test","subject":"fixture","any_permissions":["platform.registrations.repair"]}`)).WithContext(cancelled)
		response := httptest.NewRecorder()
		service.checkPermission(response, request)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("infra failure became business response: %d %s", response.Code, response.Body.String())
		}
	})
}
