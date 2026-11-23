package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

type unavailableSMSOnlyIdentity struct{ identityProvisionerFake }

func (*unavailableSMSOnlyIdentity) ValidateMobileUser(context.Context, uuid.UUID, string, string, string, string) error {
	return ErrPasswordReferenceInvalid
}

func testConsentedMobileSignup(t *testing.T, f *retainedWalletFenceFixture) {
	r := f.repository(t)
	ctx := context.Background()
	if err := r.ConfigureRegistrationBonusFingerprint("fixture-mobile-signup", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PrepareRegistrationBonusEligibility(ctx); err != nil {
		t.Fatal(err)
	}
	provider := &smsProviderFake{}
	identities := &identityProvisionerFake{}
	auth := newMobileAuthForTest(t, r, provider, identities)
	auth.now = r.now
	const mobile = "+8613800138000"
	challenge := func(number, consent string, purpose SMSPurpose) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := r.CreateSMSChallenge(ctx, SMSChallenge{
			ID: id, Mobile: number, Purpose: purpose, Provider: provider.Descriptor(),
			ExpiresAt: r.now().Add(5 * time.Minute), SignupConsentVersion: consent, SignupIssuer: auth.issuer,
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkSMSChallengeSent(ctx, id, "controlled", nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := challenge(mobile, mobileSignupConsentVersion, SMSPurposeLogin)
	if _, _, err := auth.verifyLogin(ctx, mobile, id, "123456", false); !errors.Is(err, ErrSMSUnavailable) || provider.verifyCalls != 0 {
		t.Fatal("an old authenticator consumed a challenge requiring the final binding gate", err)
	}
	if _, err := auth.VerifyLogin(ctx, mobile, id, "123456"); !errors.Is(err, ErrSMSInvalid) || identities.createCalls != 0 {
		t.Fatal("unverified code created an identity", err, identities.createCalls)
	}
	provider.verified = true
	legacy := challenge("+8613800138001", "", SMSPurposeLogin)
	if _, err := auth.VerifyLogin(ctx, "+8613800138001", legacy, "123456"); !errors.Is(err, ErrSMSInvalid) || identities.createCalls != 0 {
		t.Fatal("unconsented code created an identity", err, identities.createCalls)
	}
	reset := challenge("+8613800138002", "", SMSPurposePasswordReset)
	if _, err := auth.VerifyLogin(ctx, "+8613800138002", reset, "123456"); !errors.Is(err, ErrSMSInvalid) || identities.createCalls != 0 {
		t.Fatal("wrong code purpose created an identity", err, identities.createCalls)
	}
	if _, err := auth.VerifyLogin(ctx, "+8613800138003", id, "123456"); !errors.Is(err, ErrSMSInvalid) || identities.createCalls != 0 {
		t.Fatal("wrong mobile borrowed consent", err, identities.createCalls)
	}
	subject, err := auth.VerifyLogin(ctx, mobile, id, "123456")
	if err != nil || subject != "keycloak-subject" || identities.createCalls != 1 {
		t.Fatalf("expected signup and subject, got subject=%q creates=%d err=%v", subject, identities.createCalls, err)
	}
	var userID uuid.UUID
	var status, mode, consent string
	if err := r.pool.QueryRow(ctx, `SELECT u.id,u.status,ro.password_policy_version,ro.signup_consent_version
FROM registration_operations ro JOIN users u ON u.id=ro.committed_user_id
JOIN wallet_provisioning_jobs j ON j.user_id=u.id AND j.registration_operation_id=ro.id
WHERE ro.sms_challenge_id=$1 AND j.status='PENDING'`, id).Scan(&userID, &status, &mode, &consent); err != nil ||
		userID == uuid.Nil || status != "PROVISIONING" || mode != identity.MobileSignupCredentialMode || consent != mobileSignupConsentVersion {
		t.Fatal("expected one committed personal account with pending wallet and consent", err)
	}
	decision, err := r.Authorize(ctx, identity.Principal{Issuer: auth.issuer, Subject: subject}, PermissionPersonalSessionLogin, nil, nil)
	if err != nil || !decision.Allowed {
		t.Fatal("provisioning account could not sign in", err)
	}
	principal := identity.Principal{Issuer: auth.issuer, Subject: subject}
	if decision, err := r.AuthorizeBoundSMSLogin(ctx, principal, id); err != nil || !decision.Allowed || decision.UserID != userID {
		t.Fatal("the final login gate did not authorize the committed SMS account", err)
	}
	if _, err := auth.VerifyLogin(ctx, mobile, id, "123456"); !errors.Is(err, ErrSMSInvalid) || identities.createCalls != 1 {
		t.Fatal("replayed code provided another session or identity", err, identities.createCalls)
	}

	const reassignedMobile = "+8613800138004"
	stale := challenge(reassignedMobile, mobileSignupConsentVersion, SMSPurposeLogin)
	// Install a different ownership generation between issue and verification.
	if _, err := r.pool.Exec(ctx, `UPDATE users SET mobile_e164=$2,mobile_verified_at=now(),
identity_security_version=identity_security_version+1 WHERE id=$1`, userID, reassignedMobile); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.VerifyLogin(ctx, reassignedMobile, stale, "123456"); !errors.Is(err, ErrSMSInvalid) || identities.createCalls != 1 {
		t.Fatal("a code issued to an unbound mobile authenticated a newly bound identity", err, identities.createCalls)
	}
	if _, err := r.AuthorizeBoundSMSLogin(ctx, principal, id); !errors.Is(err, ErrForbidden) {
		t.Fatal("a proof consumed before the move authorized a later session", err)
	}
	if _, err := r.AuthorizeBoundSMSLogin(ctx, principal, stale); !errors.Is(err, ErrForbidden) {
		t.Fatal("a consumed but refused challenge authorized a session", err)
	}
	r.now = func() time.Time { return time.Now().UTC().Add(time.Minute + time.Second) }
	current := challenge(reassignedMobile, mobileSignupConsentVersion, SMSPurposeLogin)
	if subject, err := auth.VerifyLogin(ctx, reassignedMobile, current, "123456"); err != nil || subject != "keycloak-subject" || identities.createCalls != 1 {
		t.Fatal("a new code did not sign in to the existing identity", err, identities.createCalls)
	}
	if decision, err := r.AuthorizeBoundSMSLogin(ctx, principal, current); err != nil || !decision.Allowed {
		t.Fatal("the final gate refused a fresh proof from the current phone", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE registration_operations SET status='REPAIR_REQUIRED' WHERE committed_user_id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	otherPrincipal := identity.Principal{Issuer: auth.issuer, Subject: "unrelated-profile", SessionID: "unrelated-session", Username: "unrelated-user"}
	if _, _, err := r.Bootstrap(ctx, otherPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateProfileChange(ctx, otherPrincipal, "MOBILE", mobile); err != nil {
		t.Fatal("wallet repair on an already committed user blocked transfer to a released number", err)
	}

	unavailableIdentity := &unavailableSMSOnlyIdentity{}
	auth.identities = unavailableIdentity
	const otherMobile = "+8613800138005"
	notYetSupported := challenge(otherMobile, mobileSignupConsentVersion, SMSPurposeLogin)
	if _, err := auth.VerifyLogin(ctx, otherMobile, notYetSupported, "123456"); !errors.Is(err, ErrRegistrationRecovering) || unavailableIdentity.createCalls != 0 {
		t.Fatal("adapter preflight failure should preserve the original operation", err)
	}
	var canRetry bool
	var recoveryOperation uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id,identity_create_attempted_at IS NULL AND status='IDENTITY_CREATING'
FROM registration_operations WHERE sms_challenge_id=$1`, notYetSupported).Scan(&recoveryOperation, &canRetry); err != nil || !canRetry {
		t.Fatal("adapter preflight failure consumed the durable first grant", err)
	}
	first, firstVersion, err := r.MarkRegistrationCreateAttempted(ctx, recoveryOperation)
	if err != nil || !first {
		t.Fatal("recovered operation lost the first grant", err)
	}
	laterFirst, laterVersion, err := r.MarkRegistrationCreateAttempted(ctx, recoveryOperation)
	if err != nil || laterFirst || laterVersion <= firstVersion {
		t.Fatal("query-only recovery could obtain a second grant", err)
	}
	if err := r.CompensatePreIdentityRegistration(ctx, recoveryOperation, signupCreateNotDispatched, laterVersion); !errors.Is(err, ErrRegistrationConflict) {
		t.Fatal("proof for a query-only version released the reservation", err)
	}
	if err := r.CompensatePreIdentityRegistration(ctx, recoveryOperation, signupCreateNotDispatched, firstVersion); err != nil {
		t.Fatal("authoritative first-dispatch proof could not release the reservation", err)
	}

	const knownFailed, unknownUnsent, resetFailed = "+8613800138006", "+8613800138007", "+8613800138008"
	issue := func(mobile string, purpose SMSPurpose) (uuid.UUID, error) {
		id := uuid.New()
		return id, r.CreateSMSChallenge(ctx, SMSChallenge{ID: id, Mobile: mobile,
			Purpose: purpose, Provider: provider.Descriptor(), ExpiresAt: r.now().Add(5 * time.Minute)})
	}
	failed, err := issue(knownFailed, SMSPurposeLogin)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkSMSChallengeFailed(ctx, failed); err != nil {
		t.Fatal(err)
	}
	reset, err = issue(resetFailed, SMSPurposePasswordReset)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkSMSChallengeFailed(ctx, reset); err != nil {
		t.Fatal(err)
	}
	if _, err := issue(unknownUnsent, SMSPurposeLogin); err != nil {
		t.Fatal(err)
	}
	for _, mobile := range []string{knownFailed, unknownUnsent, resetFailed} {
		if _, err := issue(mobile, SMSPurposeLogin); !errors.Is(err, ErrSMSRateLimited) {
			t.Fatal("failed and unsent login must have the same cooldown", err)
		}
	}

	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	rollbackCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	conn, err := r.pool.Acquire(rollbackCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err = accountschema.DownOne(rollbackCtx, conn.Conn(), installed); err == nil || !strings.Contains(err.Error(), "mobile signup consent exists") {
		t.Fatal("database rollback did not produce the consent protection result", err)
	}
	if _, err := migrations.VerifyApplied(ctx, conn.Conn(), installed); err != nil {
		t.Fatal("blocked migration rollback changed the schema ledger", err)
	}
	if err := conn.QueryRow(ctx, `SELECT signup_consent_version FROM registration_operations WHERE sms_challenge_id=$1`, id).
		Scan(&consent); err != nil || consent != mobileSignupConsentVersion {
		t.Fatal("blocked migration rollback lost consent", err)
	}
}
