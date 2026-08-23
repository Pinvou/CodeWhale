package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestNormalizeMainlandMobile(t *testing.T) {
	for _, input := range []string{"13800138000", "+8613800138000", "8613800138000"} {
		mobile, err := NormalizeMainlandMobile(input)
		if err != nil || mobile != "+8613800138000" {
			t.Fatalf("normalize %q: mobile=%q err=%v", input, mobile, err)
		}
	}
	for _, input := range []string{"", "12800138000", "+85213800138000"} {
		if _, err := NormalizeMainlandMobile(input); err == nil {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
}

func TestSMSRateLimitReturnsRemainingCooldown(t *testing.T) {
	now := time.Date(2026, time.August, 20, 3, 0, 0, 0, time.UTC)
	last := now.Add(-17 * time.Second)
	err := smsRateLimit(now, &last, &last, 1)
	var limited *SMSRateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 43*time.Second {
		t.Fatalf("unexpected retry delay: err=%v limited=%#v", err, limited)
	}
	oldest := now.Add(-52 * time.Minute)
	err = smsRateLimit(now, nil, &oldest, 5)
	if !errors.As(err, &limited) || limited.RetryAfter != 8*time.Minute {
		t.Fatalf("unexpected hourly retry delay: err=%v limited=%#v", err, limited)
	}
}

func TestNormalizeRegistrationUsername(t *testing.T) {
	for input, expected := range map[string]string{"pinvou": "pinvou", " Zhuo.WP ": "zhuo.wp", "user_2026": "user_2026"} {
		actual, err := NormalizeRegistrationUsername(input)
		if err != nil || actual != expected {
			t.Fatalf("normalize %q: username=%q err=%v", input, actual, err)
		}
	}
	for _, input := range []string{"", "ab", "1pinvou", "手机用户", "pinvou account", "a23456789012345678901234567890123"} {
		if _, err := NormalizeRegistrationUsername(input); err == nil {
			t.Fatalf("expected username %q to be rejected", input)
		}
	}
}

type mobileAuthRepositoryFake struct {
	mobileExists       bool
	identitySubject    string
	identityErr        error
	usernameExists     bool
	challenge          SMSChallenge
	challengeSent      bool
	challengeFailed    bool
	readyErr           error
	consumeVerified    bool
	registeredUsername string
	registeredMobile   string
	captchaConsumed    bool
	sliderChallenge    SliderCaptcha
	captchaErr         error
	operation          RegistrationOperation
}

func (f *mobileAuthRepositoryFake) MobileExists(context.Context, string) (bool, error) {
	return f.mobileExists, nil
}

func (f *mobileAuthRepositoryFake) MobileIdentitySubject(context.Context, string, string) (string, error) {
	if f.identitySubject == "" && f.identityErr == nil {
		return "keycloak-subject", nil
	}
	return f.identitySubject, f.identityErr
}

func (f *mobileAuthRepositoryFake) UsernameExists(context.Context, string) (bool, error) {
	return f.usernameExists, nil
}

func (f *mobileAuthRepositoryFake) CreateSliderCaptcha(_ context.Context, challenge SliderCaptcha) error {
	f.sliderChallenge = challenge
	return nil
}

func (f *mobileAuthRepositoryFake) ConsumeSliderCaptcha(context.Context, uuid.UUID, string, SMSPurpose, int) error {
	f.captchaConsumed = true
	return f.captchaErr
}

func (f *mobileAuthRepositoryFake) CreateSMSChallenge(_ context.Context, challenge SMSChallenge) error {
	f.challenge = challenge
	return nil
}

func (f *mobileAuthRepositoryFake) CreateRegistrationOperation(_ context.Context, operation RegistrationOperation, challenge SMSChallenge) error {
	f.operation, f.challenge = operation, challenge
	return nil
}

func (f *mobileAuthRepositoryFake) MarkSMSChallengeSent(_ context.Context, _ uuid.UUID, _ string, _ []byte) error {
	f.challengeSent = true
	return nil
}

func (f *mobileAuthRepositoryFake) MarkSMSChallengeFailed(context.Context, uuid.UUID) error {
	f.challengeFailed = true
	return nil
}

func (f *mobileAuthRepositoryFake) SMSChallengeReady(context.Context, uuid.UUID, string, SMSPurpose) (SMSVerificationData, error) {
	return SMSVerificationData{Provider: SMSProviderDescriptor{ID: "fake", AdapterVersion: "v1"}, Opaque: []byte("verification")}, f.readyErr
}

func (f *mobileAuthRepositoryFake) ConsumeSMSChallenge(_ context.Context, _ uuid.UUID, _ string, _ SMSPurpose, verified bool) error {
	f.consumeVerified = verified
	if !verified {
		return ErrSMSInvalid
	}
	return nil
}

func (f *mobileAuthRepositoryFake) ConsumeRegistrationSMS(_ context.Context, operationID, _ uuid.UUID, _ string, verified bool) error {
	f.consumeVerified = verified
	if !verified {
		return ErrSMSInvalid
	}
	if f.operation.ID != operationID {
		return ErrRegistrationConflict
	}
	f.operation.Status = "REGISTRATION_IN_PROGRESS"
	return nil
}

func (f *mobileAuthRepositoryFake) RegistrationOperation(_ context.Context, operationID uuid.UUID) (RegistrationOperation, error) {
	if f.operation.ID != operationID {
		return RegistrationOperation{}, ErrNotFound
	}
	return f.operation, nil
}

func (f *mobileAuthRepositoryFake) MarkRegistrationIdentityCreating(_ context.Context, operationID uuid.UUID) error {
	if f.operation.ID != operationID {
		return ErrNotFound
	}
	f.operation.Status = "IDENTITY_CREATING"
	return nil
}

func (f *mobileAuthRepositoryFake) MarkRegistrationIdentityCreated(_ context.Context, operationID uuid.UUID, subject string) error {
	f.operation.Status, f.operation.IdentitySubject = "IDENTITY_CREATED", subject
	return nil
}

func (f *mobileAuthRepositoryFake) CommitRegistration(_ context.Context, operationID uuid.UUID, _ string) (RegistrationOperation, error) {
	userID := uuid.MustParse("018f0f76-3f5d-7d43-b630-163e3f635e5a")
	f.registeredUsername, f.registeredMobile = f.operation.Username, f.operation.Mobile
	f.operation.Status, f.operation.UserID, f.operation.UserCode = "PROVISIONING", &userID, "USR-0123-4567-89AB-CDEF-MV"
	return f.operation, nil
}

func (*mobileAuthRepositoryFake) ScheduleRegistrationRetry(context.Context, uuid.UUID, string) error {
	return nil
}
func (f *mobileAuthRepositoryFake) CompensatePreIdentityRegistration(_ context.Context, _ uuid.UUID, code string) error {
	f.operation.Status, f.operation.LastErrorCode = "COMPENSATED", code
	return nil
}

func (f *mobileAuthRepositoryFake) RegisterMobileIdentity(_ context.Context, _, _, username, mobile string) (uuid.UUID, string, error) {
	f.registeredUsername = username
	f.registeredMobile = mobile
	return uuid.MustParse("018f0f76-3f5d-7d43-b630-163e3f635e5a"), "USR-0123-4567-89AB-CDEF-MV", nil
}

type smsProviderFake struct {
	sentChallenge uuid.UUID
	sendErr       error
	verifyCalls   int
	verified      bool
	verifyErr     error
}

func (*smsProviderFake) Descriptor() SMSProviderDescriptor {
	return SMSProviderDescriptor{ID: "fake", AdapterVersion: "v1"}
}

func (f *smsProviderFake) SendCode(_ context.Context, _ string, _ SMSPurpose, challengeID uuid.UUID) (SMSDelivery, error) {
	f.sentChallenge = challengeID
	return SMSDelivery{MessageID: "provider-message", VerificationData: []byte("verification")}, f.sendErr
}

func (f *smsProviderFake) VerifyCode(context.Context, string, uuid.UUID, SMSVerificationData, string) (bool, error) {
	f.verifyCalls++
	return f.verified, f.verifyErr
}

type identityProvisionerFake struct {
	createCalls int
	username    string
	mobile      string
}

func (*identityProvisionerFake) ValidateMobileUser(context.Context, uuid.UUID, string, string, string, string) error {
	return nil
}

func (*identityProvisionerFake) FindMobileUserByRegistrationOperation(context.Context, uuid.UUID) (string, bool, error) {
	return "", false, nil
}

func (f *identityProvisionerFake) CreateMobileUser(_ context.Context, _ uuid.UUID, username, mobile, _, _ string) (string, error) {
	f.createCalls++
	f.username = username
	f.mobile = mobile
	return "keycloak-subject", nil
}

func (*identityProvisionerFake) DeleteUser(context.Context, string) error            { return nil }
func (*identityProvisionerFake) DisableUserAndConfirm(context.Context, string) error { return nil }
func (*identityProvisionerFake) UpdateUsername(context.Context, string, uuid.UUID, string) error {
	return nil
}
func (*identityProvisionerFake) UpdateMobile(context.Context, string, uuid.UUID, string) error {
	return nil
}
func (*identityProvisionerFake) ResetPassword(context.Context, string, uuid.UUID, string, string, string) error {
	return nil
}
func (*identityProvisionerFake) RevokeSessions(context.Context, string) error { return nil }

func newMobileAuthForTest(t *testing.T, repository MobileAuthRepository, sms SMSProvider, identities IdentityProvisioner) *MobileAuthService {
	t.Helper()
	tokens, err := NewRegistrationTokenSigner([]byte(strings.Repeat("q", 32)))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewMobileAuthService(repository, sms, identities, "https://account.example.com/realms/pinvou", tokens)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, time.August, 20, 3, 0, 0, 0, time.UTC) }
	service.loginResponseFloor = 0
	return service
}

func TestMobileAuthSendUsesProviderGeneratedChallenge(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	challenge, err := service.Send(context.Background(), "pinvou.user", "13800138000", "password-handle", "user-v1", SMSPurposeRegister, uuid.New(), 128, "192.0.2.1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	if challenge.ID == uuid.Nil || challenge.ID != repository.challenge.ID || challenge.ID != sms.sentChallenge || !repository.challengeSent {
		t.Fatalf("challenge was not consistently persisted and sent: %#v", challenge)
	}
	if !repository.captchaConsumed {
		t.Fatal("SMS must not be sent before slider verification is consumed")
	}
}

func TestSliderCaptchaIsBoundToMobileAndDoesNotExposeTarget(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	service := newMobileAuthForTest(t, repository, &smsProviderFake{}, &identityProvisionerFake{})
	challenge, err := service.CreateSliderCaptcha(context.Background(), "13800138000", SMSPurposeRegister)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.ID == uuid.Nil || challenge.ID != repository.sliderChallenge.ID || repository.sliderChallenge.Mobile != "+8613800138000" {
		t.Fatalf("captcha binding mismatch: response=%#v stored=%#v", challenge, repository.sliderChallenge)
	}
	if len(challenge.BackgroundImage) < 100 || len(challenge.PieceImage) < 100 {
		t.Fatal("captcha images were not generated")
	}
}

func TestMobileAuthSendRejectsInvalidCaptchaBeforeProvider(t *testing.T) {
	repository := &mobileAuthRepositoryFake{captchaErr: ErrCaptchaInvalid}
	sms := &smsProviderFake{}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	_, err := service.Send(context.Background(), "pinvou.user", "13800138000", "password-handle", "user-v1", SMSPurposeRegister, uuid.New(), 0, "192.0.2.1", "device-1")
	if !errors.Is(err, ErrCaptchaInvalid) {
		t.Fatalf("expected invalid captcha, got %v", err)
	}
	if sms.sentChallenge != uuid.Nil {
		t.Fatal("provider must not be called after failed slider verification")
	}
}

func TestMobileAuthSendMarksProviderFailureAndKeepsUnavailableClassification(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{sendErr: errors.New("provider rejected request")}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	_, err := service.Send(context.Background(), "pinvou.user", "13800138000", "password-handle", "user-v1", SMSPurposeRegister, uuid.New(), 128, "192.0.2.1", "device-1")
	if !errors.Is(err, ErrSMSUnavailable) {
		t.Fatalf("expected SMS_UNAVAILABLE classification, got %v", err)
	}
	if !repository.challengeFailed {
		t.Fatal("provider failure must mark the local challenge as failed")
	}
}

func TestMobileAuthUnknownLoginDoesNotSendOrVerifySMS(t *testing.T) {
	repository := &mobileAuthRepositoryFake{readyErr: ErrSMSInvalid}
	sms := &smsProviderFake{verified: true}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	challenge, err := service.Send(context.Background(), "", "13800138000", "", "", SMSPurposeLogin, uuid.New(), 128, "192.0.2.1", "device-1")
	if err != nil || challenge.ID == uuid.Nil {
		t.Fatalf("unknown account response must be indistinguishable: challenge=%#v err=%v", challenge, err)
	}
	if sms.sentChallenge != uuid.Nil || repository.challengeSent {
		t.Fatal("SMS must not be sent to an unknown login account")
	}
	if _, err := service.VerifyLogin(context.Background(), "13800138000", challenge.ID, "123456"); !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("unexpected verification error: %v", err)
	}
	if sms.verifyCalls != 0 {
		t.Fatal("local challenge state must be checked before calling Aliyun")
	}
}

func TestMobileAuthKnownLoginHidesProviderFailureBehindSameResponse(t *testing.T) {
	repository := &mobileAuthRepositoryFake{mobileExists: true}
	sms := &smsProviderFake{sendErr: errors.New("provider unavailable")}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	challenge, err := service.Send(context.Background(), "", "13800138000", "", "", SMSPurposeLogin, uuid.New(), 128, "192.0.2.1", "device-1")
	if err != nil || challenge.ID == uuid.Nil {
		t.Fatalf("known account response must not expose provider state: challenge=%#v err=%v", challenge, err)
	}
	deadline := time.Now().Add(time.Second)
	for !repository.challengeFailed && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !repository.challengeFailed {
		t.Fatal("asynchronous provider failure must still update challenge state")
	}
}

func TestMobileAuthLoginReturnsAuthoritativeIdentitySubject(t *testing.T) {
	repository := &mobileAuthRepositoryFake{identitySubject: "authoritative-keycloak-subject"}
	sms := &smsProviderFake{verified: true}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	subject, err := service.VerifyLogin(context.Background(), "13800138000", uuid.New(), "123456")
	if err != nil || subject != "authoritative-keycloak-subject" {
		t.Fatalf("subject=%q err=%v", subject, err)
	}
	if sms.verifyCalls != 1 || !repository.consumeVerified {
		t.Fatalf("SMS verification was not completed: calls=%d consumed=%v", sms.verifyCalls, repository.consumeVerified)
	}
}

func TestSMSLoginVerificationReturnsIdentitySubjectToKeycloak(t *testing.T) {
	repository := &mobileAuthRepositoryFake{identitySubject: "authoritative-keycloak-subject"}
	auth := newMobileAuthForTest(t, repository, &smsProviderFake{verified: true}, &identityProvisionerFake{})
	service := NewService(nil, nil).WithMobileAuth(auth, "internal-service-token-for-tests-123456")
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/auth/sms/verify", strings.NewReader(`{"mobile":"13800138000","challenge_id":"018f0f76-3f5d-7d43-b630-163e3f635e5a","code":"123456"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer internal-service-token-for-tests-123456")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if subject := response.Header().Get("X-Pinvou-Identity-Subject"); subject != "authoritative-keycloak-subject" {
		t.Fatalf("identity subject header=%q", subject)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("SMS verification response must not be cached")
	}
}

func TestMobileAuthLoginRejectsMissingIdentityBeforeConsumingCode(t *testing.T) {
	repository := &mobileAuthRepositoryFake{identityErr: pgx.ErrNoRows}
	sms := &smsProviderFake{verified: true}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	if _, err := service.VerifyLogin(context.Background(), "13800138000", uuid.New(), "123456"); !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("expected missing identity to be rejected, got %v", err)
	}
	if sms.verifyCalls != 0 || repository.consumeVerified {
		t.Fatal("a code must not be consumed when no login identity is linked")
	}
}

func TestRegistrationPrecheckRejectsDuplicatesBeforeSMS(t *testing.T) {
	repository := &mobileAuthRepositoryFake{usernameExists: true}
	service := newMobileAuthForTest(t, repository, &smsProviderFake{}, &identityProvisionerFake{})
	if _, err := service.PrecheckRegistration(context.Background(), "pinvou.user", "13800138000", "password-handle", "user-v1"); !errors.Is(err, ErrUsernameAlreadyRegistered) {
		t.Fatalf("expected duplicate username, got %v", err)
	}
	if repository.captchaConsumed {
		t.Fatal("registration precheck must finish before captcha or SMS consumption")
	}
}

func TestMobileAuthRegisterVerifiesWithProviderBeforeProvisioning(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{verified: true}
	identities := &identityProvisionerFake{}
	service := newMobileAuthForTest(t, repository, sms, identities)
	challenge, err := service.Send(context.Background(), " Pinvou.User ", "13800138000", "password-handle", "user-v1", SMSPurposeRegister, uuid.New(), 128, "192.0.2.1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Register(context.Background(), challenge.RegistrationOperationID, challenge.RegistrationQueryToken, " Pinvou.User ", "13800138000", challenge.ID, "123456", "password-handle", "user-v1")
	if err != nil || result.UserID == nil {
		t.Fatalf("register result=%+v err=%v", result, err)
	}
	if sms.verifyCalls != 1 || !repository.consumeVerified || identities.createCalls != 1 {
		t.Fatalf("verification/provisioning flow mismatch: sms=%d consumed=%v identity=%d", sms.verifyCalls, repository.consumeVerified, identities.createCalls)
	}
	if identities.username != "pinvou.user" || repository.registeredUsername != "pinvou.user" || repository.registeredMobile != "+8613800138000" {
		t.Fatalf("normalized identity was not shared across systems: identity=%q repository=%q mobile=%q", identities.username, repository.registeredUsername, repository.registeredMobile)
	}
	if result.UserCode != "USR-0123-4567-89AB-CDEF-MV" || result.Status != "PROVISIONING" {
		t.Fatalf("registration result=%+v", result)
	}
}

func TestMobileAuthRegistrationRetryReturnsCommittedResultWithoutRepeatingSideEffects(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{verified: true}
	identities := &identityProvisionerFake{}
	service := newMobileAuthForTest(t, repository, sms, identities)
	challenge, err := service.Send(context.Background(), "pinvou.user", "13800138000", "password-handle", "user-v1", SMSPurposeRegister, uuid.New(), 128, "192.0.2.1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Register(context.Background(), challenge.RegistrationOperationID, challenge.RegistrationQueryToken, "pinvou.user", "13800138000", challenge.ID, "123456", "password-handle", "user-v1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Register(context.Background(), challenge.RegistrationOperationID, challenge.RegistrationQueryToken, "pinvou.user", "13800138000", challenge.ID, "123456", "password-handle", "user-v1")
	if err != nil || first.UserID == nil || second.UserID == nil || *first.UserID != *second.UserID {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	if sms.verifyCalls != 1 || identities.createCalls != 1 {
		t.Fatalf("sms verify calls=%d identity create calls=%d", sms.verifyCalls, identities.createCalls)
	}
}

func TestMobileAuthRegisterRejectsFailedProviderVerification(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{verified: false}
	identities := &identityProvisionerFake{}
	service := newMobileAuthForTest(t, repository, sms, identities)
	challenge, err := service.Send(context.Background(), "pinvou.user", "13800138000", "password-handle", "user-v1", SMSPurposeRegister, uuid.New(), 128, "192.0.2.1", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Register(context.Background(), challenge.RegistrationOperationID, challenge.RegistrationQueryToken, "pinvou.user", "13800138000", challenge.ID, "000000", "password-handle", "user-v1")
	if !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("expected invalid SMS code, got %v", err)
	}
	if identities.createCalls != 0 {
		t.Fatal("identity must not be provisioned after a failed SMS verification")
	}
}
