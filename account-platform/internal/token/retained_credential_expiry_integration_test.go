package token

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type retainedExpiryValidator struct{ advance func() }

func (v retainedExpiryValidator) Validate(context.Context, string, []byte, []string) error {
	v.advance()
	return nil
}
func TestRetainedCredentialExpiryAfterAuthorityWait(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	var clock atomic.Int64
	start := time.Now().UTC().Truncate(time.Microsecond)
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	actor := uuid.New()
	authority := CredentialIsolationAuthoritySnapshot{ActorID: actor, Issuer: "https://controlled.invalid/issuer", Subject: "controlled-subject", AuthorizationVersion: 1, GateEpoch: 1}
	var advanceAtGuard atomic.Bool
	var finalGuardCountdown atomic.Int32
	r.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, s CredentialIsolationAuthoritySnapshot) error {
		if s != authority {
			return ErrForbidden
		}
		if advanceAtGuard.Swap(false) || (finalGuardCountdown.Load() > 0 && finalGuardCountdown.Add(-1) == 0) {
			clock.Store(start.Add(6 * time.Minute).UnixNano())
		}
		return nil
	}))
	setup := func(t *testing.T) (credentialIsolationSession, string) {
		t.Helper()
		clock.Store(start.UnixNano())
		account := uuid.New()
		if _, e := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$5,'ACTIVE',100,$2,$4,$4,'PLATFORM')`, account, actor, uuid.New(), start, account.String()); e != nil {
			t.Fatal(e)
		}
		launch, e := r.CreateCredentialLaunch(withCredentialIsolationAuthority(ctx, authority), account, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: "controlled expiry regression"}, uuid.NewString(), "controlled-expiry-request")
		if e != nil {
			t.Fatal(e)
		}
		binding := strings.Repeat("b", 64)
		session, e := r.ConsumeCredentialLaunch(ctx, launch.LaunchID, "https://credentials.pinvou.com", binding, strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), strings.Repeat("c", 64))
		if e != nil {
			t.Fatal(e)
		}
		return session, binding
	}
	t.Run("live_completion_preserves_ttl", func(t *testing.T) {
		session, binding := setup(t)
		if _, e := r.ConfirmCredentialSession(ctx, session.ID, binding); e != nil {
			t.Fatal(e)
		}
		if e := r.AuthenticateCredentialSession(ctx, session.ID, actor, start); e != nil {
			t.Fatal(e)
		}
		policy, e := NewProviderURLPolicy("test", []string{"provider.invalid"})
		if e != nil {
			t.Fatal(e)
		}
		result, e := r.CompleteCredentialSession(ctx, session.ID, session.CredentialOrigin, binding, strings.Repeat("c", 64), "CONTROLLED", "https://provider.invalid", []string{"model"}, []byte("controlled-secret"), retainedExpiryValidator{advance: func() {}}, policy)
		if e != nil || result.Status != "SUCCEEDED" {
			t.Fatalf("live completion failed: %v", e)
		}
		var expiresAt time.Time
		if e = r.pool.QueryRow(ctx, `SELECT expires_at FROM credential_isolation_sessions WHERE id=$1`, session.ID).Scan(&expiresAt); e != nil {
			t.Fatal(e)
		}
		if !expiresAt.Equal(session.ExpiresAt) {
			t.Fatal("session TTL extended")
		}
	})
	t.Run("confirm_wait_crosses_expiry", func(t *testing.T) {
		session, binding := setup(t)
		advanceAtGuard.Store(true)
		_, e := r.ConfirmCredentialSession(ctx, session.ID, binding)
		if !errors.Is(e, ErrForbidden) {
			t.Fatalf("EXPIRED_CONFIRM_ACCEPTED: rejected=%v", e != nil)
		}
		var heartbeat *time.Time
		if e = r.pool.QueryRow(ctx, `SELECT heartbeat_at FROM credential_isolation_sessions WHERE id=$1`, session.ID).Scan(&heartbeat); e != nil {
			t.Fatal(e)
		}
		if heartbeat != nil {
			t.Fatal("EXPIRED_CONFIRM_WROTE_HEARTBEAT")
		}
	})
	t.Run("authenticate_wait_crosses_expiry", func(t *testing.T) {
		session, binding := setup(t)
		if _, e := r.ConfirmCredentialSession(ctx, session.ID, binding); e != nil {
			t.Fatal(e)
		}
		advanceAtGuard.Store(true)
		e := r.AuthenticateCredentialSession(ctx, session.ID, actor, start)
		if !errors.Is(e, ErrForbidden) {
			t.Fatalf("EXPIRED_AUTHENTICATION_ACCEPTED: rejected=%v", e != nil)
		}
		var status string
		if e = r.pool.QueryRow(ctx, `SELECT status FROM credential_isolation_sessions WHERE id=$1`, session.ID).Scan(&status); e != nil {
			t.Fatal(e)
		}
		if status != "AUTHENTICATING" {
			t.Fatal("EXPIRED_AUTHENTICATION_WROTE_READY")
		}
	})
	t.Run("completion_final_authority_wait_rolls_back", func(t *testing.T) {
		session, binding := setup(t)
		if _, e := r.ConfirmCredentialSession(ctx, session.ID, binding); e != nil {
			t.Fatal(e)
		}
		if e := r.AuthenticateCredentialSession(ctx, session.ID, actor, start); e != nil {
			t.Fatal(e)
		}
		policy, e := NewProviderURLPolicy("test", []string{"provider.invalid"})
		if e != nil {
			t.Fatal(e)
		}
		// Initial session authority, locked resource authority, then the final
		// commit authority check. Only the last wait crosses the existing TTL.
		finalGuardCountdown.Store(3)
		_, e = r.CompleteCredentialSession(ctx, session.ID, session.CredentialOrigin, binding, strings.Repeat("c", 64), "CONTROLLED", "https://provider.invalid", []string{"model"}, []byte("controlled-secret"), retainedExpiryValidator{advance: func() {}}, policy)
		if !errors.Is(e, ErrForbidden) || finalGuardCountdown.Load() != 0 {
			t.Fatalf("final expiry guard not enforced: %v", e)
		}
		var count int
		if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1`, session.TokenAccountID).Scan(&count); e != nil {
			t.Fatal(e)
		}
		if count != 0 {
			t.Fatal("expired completion committed credential")
		}
		var status string
		if e = r.pool.QueryRow(ctx, `SELECT status FROM credential_isolation_results WHERE result_ref=$1`, session.ResultRef).Scan(&status); e != nil {
			t.Fatal(e)
		}
		if status != "PENDING" {
			t.Fatal("expired completion committed result")
		}
	})
	t.Run("complete_validator_crosses_expiry", func(t *testing.T) {
		session, binding := setup(t)
		if _, e := r.ConfirmCredentialSession(ctx, session.ID, binding); e != nil {
			t.Fatal(e)
		}
		if e := r.AuthenticateCredentialSession(ctx, session.ID, actor, start); e != nil {
			t.Fatal(e)
		}
		policy, e := NewProviderURLPolicy("test", []string{"provider.invalid"})
		if e != nil {
			t.Fatal(e)
		}
		_, e = r.CompleteCredentialSession(ctx, session.ID, session.CredentialOrigin, binding, strings.Repeat("c", 64), "CONTROLLED", "https://provider.invalid", []string{"model"}, []byte("controlled-secret"), retainedExpiryValidator{advance: func() { clock.Store(start.Add(6 * time.Minute).UnixNano()) }}, policy)
		if !errors.Is(e, ErrForbidden) {
			t.Fatalf("EXPIRED_COMPLETION_ACCEPTED: rejected=%v", e != nil)
		}
		var count int
		if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE token_account_id=$1`, session.TokenAccountID).Scan(&count); e != nil {
			t.Fatal(e)
		}
		if count != 0 {
			t.Fatal("EXPIRED_COMPLETION_CREATED_CREDENTIAL")
		}
	})
}
