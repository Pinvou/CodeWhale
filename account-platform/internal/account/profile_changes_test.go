package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestProfileChangeChallengeErrorsStayActionable(t *testing.T) {
	service := &Service{}
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "captcha", err: ErrCaptchaInvalid, status: http.StatusBadRequest, code: "CAPTCHA_INVALID"},
		{name: "sms provider", err: errors.Join(ErrSMSUnavailable, errors.New("provider rejected request")), status: http.StatusServiceUnavailable, code: "SMS_UNAVAILABLE"},
		{name: "expired password reference", err: ErrPasswordReferenceInvalid, status: http.StatusGone, code: "PASSWORD_REFERENCE_INVALID"},
		{name: "mismatched password reference", err: ErrPasswordReferenceConflict, status: http.StatusConflict, code: "PASSWORD_REFERENCE_CONFLICT"},
		{name: "password policy", err: ErrPasswordInvalid, status: http.StatusBadRequest, code: "PASSWORD_INVALID"},
		{name: "password edge space", err: ErrPasswordEdgeSpace, status: http.StatusBadRequest, code: "PASSWORD_EDGE_SPACE_NOT_ALLOWED"},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/me/profile-change-requests/request/mobile-challenges", nil)
			service.writeProfileError(response, request, item.err)
			if response.Code != item.status {
				t.Fatalf("status=%d, want %d", response.Code, item.status)
			}
			if body := response.Body.String(); !strings.Contains(body, `"code":"`+item.code+`"`) {
				t.Fatalf("body=%s, want code %s", body, item.code)
			} else if strings.Contains(body, "provider rejected request") {
				t.Fatalf("body leaked provider error: %s", body)
			}
		})
	}
}

func TestCompleteProfileChangeDoesNotRecoverAnotherUsersRequest(t *testing.T) {
	now := time.Date(2026, time.September, 27, 8, 0, 0, 0, time.UTC)
	requestID := uuid.New()
	store := &profileStoreFake{change: ProfileChange{
		ID: requestID, UserID: uuid.New(), IdentityIssuer: "issuer", IdentitySubject: "victim",
		Field: "MOBILE", NewValue: "+8613800138000", Status: "RETRY",
	}}
	service := &Service{profiles: &ProfileChangeService{store: store, now: func() time.Time { return now }}}
	principal := identity.Principal{Issuer: "issuer", Subject: "attacker", SessionID: "session", AuthenticatedAt: now.Add(-time.Minute)}
	handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) { return principal, nil }, http.HandlerFunc(service.completeProfileChange))
	request := httptest.NewRequest(http.MethodPost, "/v1/me/profile-change-requests/"+requestID.String()+"/complete", strings.NewReader(`{"code":"123456"}`))
	request.SetPathValue("request_id", requestID.String())
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-user recovery status=%d body=%s, want 404", response.Code, response.Body.String())
	}
	if body := response.Body.String(); strings.Contains(body, store.change.NewValue) || strings.Contains(body, "RETRY") {
		t.Fatalf("cross-user recovery leaked profile change: %s", body)
	}
}

type profileStoreFake struct {
	mu               sync.Mutex
	change           ProfileChange
	activeChallenge  *SMSChallenge
	leaseID          uuid.UUID
	claimErr         error
	reserveErr       error
	markSentErr      error
	markFailedErr    error
	markUncertainErr error
	reserveCalls     int
	profileErr       error
	calls            []string
	reset            PasswordResetOperation
	resetChallenge   uuid.UUID
	retryCode        string
	failedCode       string
}

func (f *profileStoreFake) CreateProfileChange(_ context.Context, p identity.Principal, field, value string) (ProfileChange, error) {
	f.change = ProfileChange{ID: uuid.New(), UserID: uuid.New(), IdentityIssuer: p.Issuer, IdentitySubject: p.Subject, Field: field, OldValue: "old-name", NewValue: value, ExpectedProfileVersion: 3, Status: "PENDING", SLAExpiresAt: time.Now().Add(time.Hour)}
	return f.change, nil
}

func (f *profileStoreFake) ProfileChange(context.Context, uuid.UUID, *uuid.UUID) (ProfileChange, error) {
	if f.profileErr != nil {
		return ProfileChange{}, f.profileErr
	}
	return f.change, nil
}
func (f *profileStoreFake) ReserveProfileSMSChallenge(_ context.Context, _, _ uuid.UUID, _ uuid.UUID, _ int, candidate SMSChallenge) (SMSChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	if f.reserveErr != nil {
		return SMSChallenge{}, f.reserveErr
	}
	if f.activeChallenge != nil && (f.activeChallenge.Status == "CREATED" || f.activeChallenge.Status == "SENDING" || f.activeChallenge.Status == "UNCERTAIN" || f.activeChallenge.Status == "SENT") {
		return *f.activeChallenge, nil
	}
	candidate.Status = "CREATED"
	f.activeChallenge = &candidate
	f.change.SMSChallengeID = &candidate.ID
	return candidate, nil
}
func (f *profileStoreFake) ClaimProfileSMSDelivery(_ context.Context, _, _, _ uuid.UUID, _ bool) (uuid.UUID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return uuid.Nil, false, f.claimErr
	}
	if f.activeChallenge == nil || f.activeChallenge.Status != "CREATED" || f.leaseID != uuid.Nil {
		return uuid.Nil, false, nil
	}
	f.leaseID = uuid.New()
	return f.leaseID, true, nil
}
func (f *profileStoreFake) MarkProfileSMSDeliverySent(_ context.Context, _, _ uuid.UUID, leaseID uuid.UUID, _ SMSDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markSentErr != nil {
		return f.markSentErr
	}
	if f.activeChallenge == nil || f.leaseID != leaseID {
		return ErrConflict
	}
	f.activeChallenge.Status = "SENT"
	f.leaseID = uuid.Nil
	return nil
}
func (f *profileStoreFake) MarkProfileSMSDeliveryFailed(_ context.Context, _, _ uuid.UUID, leaseID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markFailedErr != nil {
		return f.markFailedErr
	}
	if f.activeChallenge == nil || f.leaseID != leaseID {
		return ErrConflict
	}
	f.activeChallenge.Status = "FAILED"
	f.leaseID = uuid.Nil
	return nil
}
func (f *profileStoreFake) ReleaseProfileSMSDeliveryLease(_ context.Context, _, _ uuid.UUID, leaseID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseID != leaseID {
		return ErrConflict
	}
	f.leaseID = uuid.Nil
	return nil
}
func (f *profileStoreFake) MarkProfileSMSDeliveryUncertain(_ context.Context, _, _ uuid.UUID, leaseID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markUncertainErr != nil {
		return f.markUncertainErr
	}
	if leaseID != uuid.Nil && f.leaseID != leaseID {
		return ErrConflict
	}
	if f.activeChallenge == nil {
		return ErrConflict
	}
	f.activeChallenge.Status = "UNCERTAIN"
	f.leaseID = uuid.Nil
	return nil
}
func (f *profileStoreFake) CommitProfileSMSVerification(_ context.Context, _, _ uuid.UUID, challengeID uuid.UUID, verified bool) error {
	if !verified {
		return ErrSMSInvalid
	}
	now := time.Now()
	f.change.SMSVerifiedAt = &now
	f.change.SMSChallengeID = &challengeID
	f.change.Status = "VERIFIED"
	return nil
}
func (f *profileStoreFake) MarkProfileIdentityUpdated(context.Context, uuid.UUID) error {
	now := time.Now()
	f.change.IdentityUpdatedAt = &now
	f.change.Status = "IDENTITY_UPDATED"
	f.calls = append(f.calls, "identity")
	return nil
}
func (f *profileStoreFake) CommitProfileChange(context.Context, uuid.UUID) error {
	now := time.Now()
	f.change.AccountCommittedAt = &now
	f.change.Status = "ACCOUNT_COMMITTED"
	f.calls = append(f.calls, "account")
	return nil
}
func (f *profileStoreFake) MarkProfileSessionsRevoked(context.Context, uuid.UUID) error {
	now := time.Now()
	f.change.SessionsRevokedAt = &now
	f.calls = append(f.calls, "sessions")
	return nil
}
func (f *profileStoreFake) CompleteProfileChange(context.Context, uuid.UUID) error {
	f.change.Status = "COMPLETED"
	f.calls = append(f.calls, "complete")
	return nil
}
func (*profileStoreFake) ScheduleProfileChangeRetry(context.Context, uuid.UUID, string) error {
	return nil
}
func (*profileStoreFake) ClaimProfileChange(context.Context) (*ProfileChange, error) { return nil, nil }
func (f *profileStoreFake) PasswordResetByChallenge(_ context.Context, challengeID uuid.UUID) (PasswordResetOperation, error) {
	if f.resetChallenge == challengeID && f.reset.ID != uuid.Nil {
		return f.reset, nil
	}
	return PasswordResetOperation{}, ErrNotFound
}
func (f *profileStoreFake) CreatePasswordReset(_ context.Context, _ string, challengeID uuid.UUID, mobile string, verified bool, handle, policy string) (PasswordResetOperation, error) {
	if !verified {
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	f.resetChallenge = challengeID
	f.reset = PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: mobile, PasswordHandle: handle, PasswordPolicyVersion: policy, FenceVersion: 1, Status: "VERIFIED"}
	return f.reset, nil
}
func (f *profileStoreFake) CreateAuthenticatedPasswordReset(_ context.Context, principal identity.Principal, handle, policy string) (PasswordResetOperation, error) {
	f.reset = PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: principal.Subject, Mobile: "+8613800138000", PasswordHandle: handle, PasswordPolicyVersion: policy, FenceVersion: 1, Status: "VERIFIED"}
	return f.reset, nil
}
func (f *profileStoreFake) MarkPasswordWriteStarted(_ context.Context, _ uuid.UUID, owner uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reset.Status != "VERIFIED" || f.reset.PasswordWriteStartedAt != nil {
		return false, nil
	}
	now := time.Now()
	f.reset.PasswordWriteStartedAt = &now
	f.reset.PasswordWriteOwner = &owner
	f.reset.Status = "PASSWORD_STARTED"
	f.calls = append(f.calls, "password-started")
	return true, nil
}
func (f *profileStoreFake) MarkPasswordUpdated(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.reset.PasswordUpdatedAt = &now
	f.reset.Status = "PASSWORD_UPDATED"
	f.calls = append(f.calls, "password-updated")
	return nil
}
func (f *profileStoreFake) MarkPasswordSessionsRevoked(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	f.reset.SessionsRevokedAt = &now
	f.calls = append(f.calls, "password-sessions")
	return nil
}
func (f *profileStoreFake) CompletePasswordReset(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset.Status = "COMPLETED"
	f.calls = append(f.calls, "password-complete")
	return nil
}
func (f *profileStoreFake) FailPasswordReset(_ context.Context, _ uuid.UUID, code string) error {
	f.failedCode = code
	f.reset.Status = "FAILED"
	return nil
}
func (f *profileStoreFake) SchedulePasswordResetRetry(_ context.Context, _ uuid.UUID, code string) error {
	f.retryCode = code
	f.reset.Status = "RETRY"
	return nil
}
func (f *profileStoreFake) RequirePasswordResetRepair(_ context.Context, _ uuid.UUID, code string) error {
	f.retryCode = code
	f.reset.Status = "REPAIR_REQUIRED"
	return nil
}
func (f *profileStoreFake) ClaimPasswordReset(context.Context) (*PasswordResetOperation, error) {
	return nil, nil
}
func (*profileStoreFake) ListProfileRepairs(context.Context) ([]profileChangeRepairItem, error) {
	return nil, nil
}
func (*profileStoreFake) RetryProfileRepair(_ context.Context, _ identity.Principal, id uuid.UUID) (profileRepairRetryResult, error) {
	return profileRepairRetryResult{RequestID: id, ResourceType: "profile_change", Action: "RETRY_SCHEDULED", Status: "RETRY"}, nil
}

type profileIdentityFake struct {
	mu        sync.Mutex
	calls     []string
	resetErr  error
	revokeErr error
}

func (*profileIdentityFake) ValidateMobileUser(context.Context, uuid.UUID, string, string, string, string) error {
	return nil
}
func (*profileIdentityFake) CreateMobileUser(context.Context, uuid.UUID, string, string, string, string) (string, error) {
	return "", nil
}
func (*profileIdentityFake) FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error) {
	return "", false, nil
}
func (*profileIdentityFake) DisableUserAndConfirm(context.Context, string) error { return nil }
func (*profileIdentityFake) DeleteUser(context.Context, string) error            { return nil }
func (f *profileIdentityFake) UpdateUsername(context.Context, string, uuid.UUID, int64, string) error {
	f.calls = append(f.calls, "identity")
	return nil
}
func (*profileIdentityFake) UpdateMobile(context.Context, string, uuid.UUID, int64, string) error {
	return nil
}
func (f *profileIdentityFake) ResetPassword(context.Context, string, uuid.UUID, int64, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "password")
	return f.resetErr
}
func (f *profileIdentityFake) RevokeSessions(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "sessions")
	return f.revokeErr
}

func TestPasswordResetRetriesSessionRevocationWithoutRepeatingPasswordMutation(t *testing.T) {
	store := &profileStoreFake{reset: PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: "+8613800138000", PasswordHandle: "handle", PasswordPolicyVersion: "user-v1", FenceVersion: 1, Status: "VERIFIED"}}
	identities := &profileIdentityFake{revokeErr: errors.New("keycloak temporarily unavailable")}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(context.Background(), store.reset); err == nil {
		t.Fatal("first attempt must report the incomplete session revocation")
	}
	if store.reset.PasswordUpdatedAt == nil || store.reset.Status != "RETRY" || store.retryCode != "SESSION_REVOCATION_FAILED" {
		t.Fatalf("partial reset was not checkpointed for retry: %#v code=%s", store.reset, store.retryCode)
	}
	identities.revokeErr = nil
	if err := service.resumePasswordReset(context.Background(), store.reset); err != nil {
		t.Fatal(err)
	}
	if store.reset.Status != "COMPLETED" || store.reset.SessionsRevokedAt == nil {
		t.Fatalf("retry did not converge: %#v", store.reset)
	}
	if got := strings.Join(identities.calls, ","); got != "password,sessions,sessions" {
		t.Fatalf("identity calls=%s, password must not be applied twice", got)
	}
}

func TestPasswordResetDefiniteReferenceFailureDoesNotPretendThePasswordChanged(t *testing.T) {
	store := &profileStoreFake{reset: PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: "+8613800138000", PasswordHandle: "handle", PasswordPolicyVersion: "user-v1", FenceVersion: 1, Status: "VERIFIED"}}
	identities := &profileIdentityFake{resetErr: ErrPasswordReferenceInvalid}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(context.Background(), store.reset); !errors.Is(err, ErrPasswordReferenceInvalid) {
		t.Fatalf("uncertain reset error=%v", err)
	}
	if store.reset.PasswordWriteStartedAt == nil || store.reset.SessionsRevokedAt != nil || store.reset.Status != "FAILED" {
		t.Fatalf("definite non-application was not terminated safely: %#v", store.reset)
	}
	if got := strings.Join(identities.calls, ","); got != "password" {
		t.Fatalf("definite non-application calls=%s", got)
	}
}

func TestPasswordResetUncertainAtomicResultRevokesSessionsBeforeRetry(t *testing.T) {
	store := &profileStoreFake{reset: PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: "+8613800138000", PasswordHandle: "handle", PasswordPolicyVersion: "user-v1", FenceVersion: 1, Status: "VERIFIED"}}
	identities := &profileIdentityFake{resetErr: errors.New("atomic endpoint response lost")}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(context.Background(), store.reset); err == nil {
		t.Fatal("uncertain atomic result must remain incomplete")
	}
	if store.reset.SessionsRevokedAt == nil || store.reset.Status != "RETRY" || store.retryCode != "IDENTITY_RESET_UNCERTAIN" {
		t.Fatalf("uncertain reset did not revoke sessions before retry: %#v code=%s", store.reset, store.retryCode)
	}
	if got := strings.Join(identities.calls, ","); got != "password,sessions" {
		t.Fatalf("uncertain reset calls=%s", got)
	}
}

func TestPasswordResetStartedReplayUsesTheAtomicIdentityProbe(t *testing.T) {
	now := time.Now()
	store := &profileStoreFake{reset: PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", FenceVersion: 3, Status: "PASSWORD_STARTED", PasswordWriteStartedAt: &now, LastErrorCode: "IDENTITY_RESET_UNCERTAIN"}}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(context.Background(), store.reset); err != nil {
		t.Fatalf("started replay error=%v", err)
	}
	if got := strings.Join(identities.calls, ","); got != "password,sessions" {
		t.Fatalf("started replay calls=%s", got)
	}
	if store.reset.Status != "COMPLETED" || store.reset.SessionsRevokedAt == nil {
		t.Fatalf("started replay result=%#v", store.reset)
	}
}

func TestLegacyAmbiguousPasswordResetOnlyRevokesSessionsAndRequiresRepair(t *testing.T) {
	now := time.Now()
	store := &profileStoreFake{reset: PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", FenceVersion: 1, Status: "PASSWORD_STARTED", PasswordWriteStartedAt: &now, LastErrorCode: "LEGACY_SESSION_REVOCATION_UNCERTAIN"}}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(context.Background(), store.reset); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy recovery error=%v", err)
	}
	if got := strings.Join(identities.calls, ","); got != "sessions" {
		t.Fatalf("legacy recovery calls=%s", got)
	}
	if store.reset.Status != "REPAIR_REQUIRED" || store.reset.SessionsRevokedAt == nil {
		t.Fatalf("legacy recovery result=%#v", store.reset)
	}
}

func TestPasswordResetVerifiedSnapshotCanOnlyAcquireOneIdentityWrite(t *testing.T) {
	operation := PasswordResetOperation{ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: "+8613800138000", PasswordHandle: "handle", PasswordPolicyVersion: "user-v1", FenceVersion: 1, Status: "VERIFIED"}
	store := &profileStoreFake{reset: operation}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			errorsSeen <- service.resumePasswordReset(context.Background(), operation)
		}()
	}
	wait.Wait()
	close(errorsSeen)
	successes, conflicts := 0, 0
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent reset error=%v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent reset successes=%d conflicts=%d", successes, conflicts)
	}
	identities.mu.Lock()
	defer identities.mu.Unlock()
	if got := strings.Count(strings.Join(identities.calls, ","), "password"); got != 1 {
		t.Fatalf("identity password calls=%v, want one", identities.calls)
	}
}

func TestPasswordResetResponseLossResumesTheOperationBoundToTheConsumedChallenge(t *testing.T) {
	challengeID := uuid.New()
	store := &profileStoreFake{
		resetChallenge: challengeID,
		reset: PasswordResetOperation{
			ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: "+8613800138000",
			PasswordHandle: "handle", PasswordPolicyVersion: "user-v1", FenceVersion: 7, Status: "VERIFIED",
		},
	}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: time.Now}
	if err := service.ResetPassword(context.Background(), "13800138000", challengeID, "already-consumed", "different-handle", "user-v1"); err != nil {
		t.Fatal(err)
	}
	if store.reset.Status != "COMPLETED" {
		t.Fatalf("recovered operation status=%s", store.reset.Status)
	}
	if got := strings.Join(identities.calls, ","); got != "password,sessions" {
		t.Fatalf("recovery calls=%s", got)
	}
}

func TestPasswordResetResponseLossPreservesTerminalPasswordReferenceError(t *testing.T) {
	challengeID := uuid.New()
	store := &profileStoreFake{
		resetChallenge: challengeID,
		reset: PasswordResetOperation{
			ID: uuid.New(), UserID: uuid.New(), IdentitySubject: "subject", Mobile: "+8613800138000",
			Status: "FAILED", LastErrorCode: "PASSWORD_REFERENCE_INVALID",
		},
	}
	service := &ProfileChangeService{store: store, identities: &profileIdentityFake{}, now: time.Now}
	if err := service.ResetPassword(context.Background(), "13800138000", challengeID, "already-consumed", "new-handle", "user-v1"); !errors.Is(err, ErrPasswordReferenceInvalid) {
		t.Fatalf("recovered terminal error=%v, want PASSWORD_REFERENCE_INVALID", err)
	}
}

func TestUsernameChangeRequiresRecentAuthenticationAndConvergesForward(t *testing.T) {
	now := time.Date(2026, time.August, 23, 4, 0, 0, 0, time.UTC)
	store := &profileStoreFake{}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: func() time.Time { return now }}
	stale := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-6 * time.Minute)}
	if _, err := service.Create(context.Background(), stale, "USERNAME", "new-name"); !errors.Is(err, ErrRecentAuthenticationRequired) {
		t.Fatalf("stale authentication error=%v", err)
	}
	principal := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-time.Minute)}
	change, err := service.Create(context.Background(), principal, "USERNAME", "new-name")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Complete(context.Background(), principal, change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "COMPLETED" {
		t.Fatalf("status=%s", result.Status)
	}
	if len(identities.calls) != 2 || identities.calls[0] != "identity" || identities.calls[1] != "sessions" {
		t.Fatalf("identity calls=%v", identities.calls)
	}
	if len(store.calls) != 4 || store.calls[0] != "identity" || store.calls[1] != "account" || store.calls[2] != "sessions" || store.calls[3] != "complete" {
		t.Fatalf("store calls=%v", store.calls)
	}
}

func TestProfileMobileChallengeReopenReusesBoundChallenge(t *testing.T) {
	now := time.Date(2026, time.September, 27, 8, 0, 0, 0, time.UTC)
	provider := &smsProviderFake{}
	challenge := &SMSChallenge{ID: uuid.New(), Status: "SENT", Purpose: SMSPurposeProfileMobile, ExpiresAt: now.Add(5 * time.Minute), Provider: provider.Descriptor()}
	store := &profileStoreFake{
		change: ProfileChange{
			ID: uuid.New(), UserID: uuid.New(), IdentityIssuer: "issuer", IdentitySubject: "subject",
			Field: "MOBILE", NewValue: "+8613800138000", Status: "PENDING", SMSChallengeID: &challenge.ID,
		},
		activeChallenge: challenge,
	}
	service := &ProfileChangeService{store: store, auth: &MobileAuthService{sms: provider}, now: func() time.Time { return now }}
	principal := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-time.Minute)}
	got, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != challenge.ID {
		t.Fatalf("challenge=%s, want existing %s", got.ID, challenge.ID)
	}
}

type blockingProfileSMSProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (*blockingProfileSMSProvider) Descriptor() SMSProviderDescriptor {
	return SMSProviderDescriptor{ID: "blocking-fake", AdapterVersion: "v1"}
}

func (*blockingProfileSMSProvider) DeliveryRetryPolicy() SMSDeliveryRetryPolicy {
	return SMSDeliveryIdempotent
}

func (*blockingProfileSMSProvider) PrepareVerification(uuid.UUID) ([]byte, error) {
	return []byte("verification"), nil
}

func (p *blockingProfileSMSProvider) SendCode(_ context.Context, _ string, _ SMSPurpose, _ uuid.UUID) (SMSDelivery, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return SMSDelivery{MessageID: "message", VerificationData: []byte("verification")}, nil
}

func (*blockingProfileSMSProvider) VerifyCode(context.Context, string, uuid.UUID, SMSVerificationData, string) (bool, error) {
	return false, nil
}

func (p *blockingProfileSMSProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newProfileMobileSendFixture(now time.Time, provider SMSProvider) (*profileStoreFake, *ProfileChangeService, identity.Principal) {
	store := &profileStoreFake{change: ProfileChange{
		ID: uuid.New(), UserID: uuid.New(), IdentityIssuer: "issuer", IdentitySubject: "subject",
		Field: "MOBILE", NewValue: "+8613800138000", Status: "PENDING",
	}}
	service := &ProfileChangeService{store: store, auth: &MobileAuthService{sms: provider}, now: func() time.Time { return now }}
	principal := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-time.Minute)}
	return store, service, principal
}

func TestProfileMobileChallengeCrossUserIsNotFoundBeforeCaptchaOrProvider(t *testing.T) {
	now := time.Date(2026, time.September, 27, 8, 30, 0, 0, time.UTC)
	provider := &smsProviderFake{}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	principal.Subject = "other-subject"
	if _, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user challenge error=%v, want not found", err)
	}
	if store.reserveCalls != 0 || store.activeChallenge != nil {
		t.Fatalf("cross-user request touched captcha/challenge: reserve=%d challenge=%#v", store.reserveCalls, store.activeChallenge)
	}
	if _, calls := provider.deliveryState(); calls != 0 {
		t.Fatalf("cross-user request called provider %d times", calls)
	}
}

func TestSendProfileMobileChallengeCrossUserHTTPIsIndistinguishableFromMissing(t *testing.T) {
	now := time.Date(2026, time.September, 27, 8, 35, 0, 0, time.UTC)
	provider := &smsProviderFake{}
	store, profiles, principal := newProfileMobileSendFixture(now, provider)
	principal.Subject = "other-subject"
	service := &Service{profiles: profiles}
	handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) { return principal, nil }, http.HandlerFunc(service.sendProfileMobileChallenge))
	request := httptest.NewRequest(http.MethodPost, "/v1/me/profile-change-requests/"+store.change.ID.String()+"/mobile-challenges", strings.NewReader(`{"captcha_id":"`+uuid.NewString()+`","captcha_offset":100}`))
	request.SetPathValue("request_id", store.change.ID.String())
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"PROFILE_CHANGE_NOT_FOUND"`) {
		t.Fatalf("cross-user HTTP status=%d body=%s, want stable not found", response.Code, response.Body.String())
	}
	missingStore := &profileStoreFake{profileErr: ErrNotFound}
	missingService := &Service{profiles: &ProfileChangeService{store: missingStore, auth: &MobileAuthService{sms: provider}, now: func() time.Time { return now }}}
	missingHandler := identity.Middleware(func(context.Context, string) (identity.Principal, error) { return principal, nil }, http.HandlerFunc(missingService.sendProfileMobileChallenge))
	missingRequest := httptest.NewRequest(http.MethodPost, request.URL.String(), strings.NewReader(`{"captcha_id":"`+uuid.NewString()+`","captcha_offset":100}`))
	missingRequest.SetPathValue("request_id", store.change.ID.String())
	missingRequest.Header.Set("Authorization", "Bearer test")
	missingRequest.Header.Set("Content-Type", "application/json")
	missingResponse := httptest.NewRecorder()
	missingHandler.ServeHTTP(missingResponse, missingRequest)
	if missingResponse.Code != response.Code || missingResponse.Body.String() != response.Body.String() {
		t.Fatalf("foreign and missing responses differ: foreign=%d/%s missing=%d/%s", response.Code, response.Body.String(), missingResponse.Code, missingResponse.Body.String())
	}
	if store.reserveCalls != 0 || store.activeChallenge != nil {
		t.Fatalf("cross-user HTTP touched captcha/challenge: reserve=%d challenge=%#v", store.reserveCalls, store.activeChallenge)
	}
	if _, calls := provider.deliveryState(); calls != 0 {
		t.Fatalf("cross-user HTTP called provider %d times", calls)
	}
}

func TestProfileMobileChallengeConcurrentDeliveryIsDatabaseSingleFlight(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 0, 0, 0, time.UTC)
	provider := &blockingProfileSMSProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	type result struct {
		challenge SMSChallenge
		err       error
	}
	first := make(chan result, 1)
	go func() {
		challenge, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
		first <- result{challenge: challenge, err: err}
	}()
	<-provider.started
	observed, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status != "SENDING" || provider.callCount() != 1 {
		t.Fatalf("concurrent observer status=%s provider calls=%d", observed.Status, provider.callCount())
	}
	close(provider.release)
	delivered := <-first
	if delivered.err != nil || delivered.challenge.Status != "SENT" {
		t.Fatalf("winner result=%#v", delivered)
	}
	reopened, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || reopened.ID != delivered.challenge.ID || reopened.Status != "SENT" || provider.callCount() != 1 {
		t.Fatalf("response-loss recovery challenge=%#v err=%v calls=%d", reopened, err, provider.callCount())
	}
}

func TestProfileMobileChallengeClaimedCrashIsRetryableWithoutFalseSent(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 10, 0, 0, time.UTC)
	provider := &smsProviderFake{}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	challenge := &SMSChallenge{ID: uuid.New(), Mobile: store.change.NewValue, Purpose: SMSPurposeProfileMobile, Status: "CREATED", ExpiresAt: now.Add(5 * time.Minute), Provider: provider.Descriptor()}
	store.activeChallenge = challenge
	store.change.SMSChallengeID = &challenge.ID
	store.leaseID = uuid.New() // prior process claimed and exited before provider call
	observed, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || observed.Status != "SENDING" {
		t.Fatalf("claimed recovery status=%s err=%v", observed.Status, err)
	}
	if _, calls := provider.deliveryState(); calls != 0 {
		t.Fatalf("active lease duplicated provider delivery: calls=%d", calls)
	}
	store.mu.Lock()
	store.leaseID = uuid.Nil // durable lease expiry permits the next owner
	store.mu.Unlock()
	delivered, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || delivered.ID != challenge.ID || delivered.Status != "SENT" {
		t.Fatalf("expired lease recovery=%#v err=%v", delivered, err)
	}
	if sentID, calls := provider.deliveryState(); calls != 1 || sentID != challenge.ID {
		t.Fatalf("provider recovery calls=%d challenge=%s", calls, sentID)
	}
}

func TestProfileMobileChallengePreDeliveryCASFailureHasNoProviderSideEffect(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 20, 0, 0, time.UTC)
	for _, configure := range []func(*profileStoreFake){
		func(store *profileStoreFake) { store.reserveErr = ErrConflict },
		func(store *profileStoreFake) { store.claimErr = ErrConflict },
	} {
		provider := &smsProviderFake{}
		store, service, principal := newProfileMobileSendFixture(now, provider)
		configure(store)
		if _, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device"); err == nil {
			t.Fatal("expected durable pre-delivery CAS failure")
		}
		if _, calls := provider.deliveryState(); calls != 0 {
			t.Fatalf("provider called before durable bind/claim: %d", calls)
		}
	}
}

func TestProfileMobileChallengeFailedDeliveryCreatesFreshChallenge(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 30, 0, 0, time.UTC)
	provider := &smsProviderFake{sendErr: &SMSDeliveryRejectedError{Cause: errors.New("provider rejected")}}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	if _, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device"); !errors.Is(err, ErrSMSUnavailable) {
		t.Fatalf("delivery error=%v", err)
	}
	failedID := store.activeChallenge.ID
	if store.activeChallenge.Status != "FAILED" {
		t.Fatalf("failed challenge status=%s", store.activeChallenge.Status)
	}
	provider.sendErr = nil
	delivered, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || delivered.Status != "SENT" || delivered.ID == failedID {
		t.Fatalf("fresh delivery=%#v err=%v failed=%s", delivered, err, failedID)
	}
}

func TestProfileMobileChallengeUnknownDeliveryRetriesSameIdempotencyKey(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 40, 0, 0, time.UTC)
	provider := &smsProviderFake{sendErr: errors.New("transport response lost")}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	if _, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device"); !errors.Is(err, ErrSMSUnavailable) {
		t.Fatalf("unknown delivery error=%v", err)
	}
	originalID := store.activeChallenge.ID
	if store.activeChallenge.Status != "CREATED" || store.leaseID != uuid.Nil {
		t.Fatalf("unknown delivery did not retain retryable challenge: %#v lease=%s", store.activeChallenge, store.leaseID)
	}
	provider.sendErr = nil
	delivered, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || delivered.Status != "SENT" || delivered.ID != originalID {
		t.Fatalf("unknown delivery retry=%#v err=%v original=%s", delivered, err, originalID)
	}
	if _, calls := provider.deliveryState(); calls != 2 {
		t.Fatalf("same-key provider attempts=%d, want 2", calls)
	}
}

func TestProfileMobileChallengeSentPersistenceFailureIsStableAndRetryable(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 50, 0, 0, time.UTC)
	provider := &smsProviderFake{}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	store.markSentErr = errors.New("database unavailable")
	if _, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device"); !errors.Is(err, ErrSMSUnavailable) {
		t.Fatalf("sent persistence error=%v, want stable SMS_UNAVAILABLE", err)
	}
	if store.activeChallenge.Status != "CREATED" || store.leaseID != uuid.Nil {
		t.Fatalf("persistence failure did not retain retryable CREATED: %#v lease=%s", store.activeChallenge, store.leaseID)
	}
	store.markSentErr = nil
	delivered, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || delivered.Status != "SENT" {
		t.Fatalf("persistence retry=%#v err=%v", delivered, err)
	}
}

func TestProfileMobileChallengeAtMostOnceUnknownNeverRedispatches(t *testing.T) {
	now := time.Date(2026, time.September, 27, 10, 0, 0, 0, time.UTC)
	provider := &smsProviderFake{sendErr: errors.New("transport response lost"), retryPolicy: SMSDeliveryAtMostOnce}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	uncertain, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || uncertain.Status != "UNCERTAIN" {
		t.Fatalf("unknown at-most-once delivery=%#v error=%v", uncertain, err)
	}
	if store.activeChallenge.Status != "UNCERTAIN" || store.leaseID != uuid.Nil {
		t.Fatalf("unknown at-most-once delivery=%#v lease=%s", store.activeChallenge, store.leaseID)
	}
	provider.sendErr = nil
	observed, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || observed.Status != "UNCERTAIN" {
		t.Fatalf("uncertain observation=%#v err=%v", observed, err)
	}
	if _, calls := provider.deliveryState(); calls != 1 {
		t.Fatalf("at-most-once provider was redispatched: calls=%d", calls)
	}
}

func TestProfileMobileChallengeAtMostOnceCrashBeforeProviderBecomesUncertain(t *testing.T) {
	now := time.Date(2026, time.September, 27, 10, 10, 0, 0, time.UTC)
	provider := &smsProviderFake{retryPolicy: SMSDeliveryAtMostOnce}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	past := now.Add(-time.Second)
	challenge := &SMSChallenge{
		ID: uuid.New(), Mobile: store.change.NewValue, Purpose: SMSPurposeProfileMobile, Status: "SENDING",
		ExpiresAt: now.Add(5 * time.Minute), Provider: provider.Descriptor(), DeliveryLeaseUntil: &past,
	}
	store.activeChallenge = challenge
	store.change.SMSChallengeID = &challenge.ID
	store.leaseID = uuid.New()
	observed, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || observed.Status != "UNCERTAIN" {
		t.Fatalf("expired at-most-once dispatch=%#v err=%v", observed, err)
	}
	if _, calls := provider.deliveryState(); calls != 0 {
		t.Fatalf("crashed at-most-once dispatch retried provider: calls=%d", calls)
	}
}

func TestProfileMobileChallengeAtMostOnceStaleDispatchDoesNotInventUncertainState(t *testing.T) {
	now := time.Date(2026, time.September, 27, 10, 15, 0, 0, time.UTC)
	provider := &smsProviderFake{retryPolicy: SMSDeliveryAtMostOnce}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	past := now.Add(-time.Second)
	challenge := &SMSChallenge{
		ID: uuid.New(), Mobile: store.change.NewValue, Purpose: SMSPurposeProfileMobile, Status: "SENDING",
		ExpiresAt: now.Add(5 * time.Minute), Provider: provider.Descriptor(), DeliveryLeaseUntil: &past,
	}
	store.activeChallenge = challenge
	store.change.SMSChallengeID = &challenge.ID
	store.leaseID = uuid.New()
	store.markUncertainErr = ErrConflict
	if _, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device"); !errors.Is(err, ErrSMSUnavailable) {
		t.Fatalf("stale delivery CAS error=%v, want SMS_UNAVAILABLE", err)
	}
	if store.activeChallenge.Status != "SENDING" {
		t.Fatalf("stale delivery was falsely projected as %s", store.activeChallenge.Status)
	}
	if _, calls := provider.deliveryState(); calls != 0 {
		t.Fatalf("stale at-most-once dispatch retried provider: calls=%d", calls)
	}
}

func TestProfileMobileChallengeAtMostOnceSentPersistenceFailureIsUncertain(t *testing.T) {
	now := time.Date(2026, time.September, 27, 10, 20, 0, 0, time.UTC)
	provider := &smsProviderFake{retryPolicy: SMSDeliveryAtMostOnce}
	store, service, principal := newProfileMobileSendFixture(now, provider)
	store.markSentErr = errors.New("database unavailable")
	uncertain, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || uncertain.Status != "UNCERTAIN" {
		t.Fatalf("sent persistence result=%#v error=%v", uncertain, err)
	}
	if store.activeChallenge.Status != "UNCERTAIN" {
		t.Fatalf("at-most-once sent persistence status=%s", store.activeChallenge.Status)
	}
	store.markSentErr = nil
	observed, err := service.SendMobileCode(context.Background(), principal, store.change.ID, uuid.New(), 100, "source", "device")
	if err != nil || observed.Status != "UNCERTAIN" {
		t.Fatalf("sent persistence observation=%#v err=%v", observed, err)
	}
	if _, calls := provider.deliveryState(); calls != 1 {
		t.Fatalf("sent persistence retried provider: calls=%d", calls)
	}
}

func TestAuthenticatedPasswordChangeRequiresRecentAuthentication(t *testing.T) {
	now := time.Date(2026, time.August, 26, 9, 0, 0, 0, time.UTC)
	store := &profileStoreFake{}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: store, identities: identities, now: func() time.Time { return now }}
	stale := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-6 * time.Minute)}
	if err := service.ChangePassword(context.Background(), stale, "password-handle", "user-v1"); !errors.Is(err, ErrRecentAuthenticationRequired) {
		t.Fatalf("stale authentication error=%v", err)
	}
	current := identity.Principal{Issuer: "issuer", Subject: "subject", AuthenticatedAt: now.Add(-time.Minute)}
	if err := service.ChangePassword(context.Background(), current, "password-handle", "user-v1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(identities.calls, ","); got != "password,sessions" {
		t.Fatalf("identity calls=%v", identities.calls)
	}
}
