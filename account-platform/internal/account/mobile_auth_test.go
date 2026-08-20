package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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

func TestRegistrationPasswordPolicy(t *testing.T) {
	if !validPassword("pinvou2026") || validPassword("password") || validPassword("12345678") || validPassword("a1") {
		t.Fatal("registration password policy mismatch")
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
	usernameExists     bool
	challenge          SMSChallenge
	challengeSent      bool
	challengeFailed    bool
	readyErr           error
	consumeVerified    bool
	registeredUsername string
	registeredMobile   string
}

func (f *mobileAuthRepositoryFake) MobileExists(context.Context, string) (bool, error) {
	return f.mobileExists, nil
}

func (f *mobileAuthRepositoryFake) UsernameExists(context.Context, string) (bool, error) {
	return f.usernameExists, nil
}

func (f *mobileAuthRepositoryFake) CreateSMSChallenge(_ context.Context, challenge SMSChallenge) error {
	f.challenge = challenge
	return nil
}

func (f *mobileAuthRepositoryFake) MarkSMSChallengeSent(_ context.Context, _ uuid.UUID, _ string) error {
	f.challengeSent = true
	return nil
}

func (f *mobileAuthRepositoryFake) MarkSMSChallengeFailed(context.Context, uuid.UUID) error {
	f.challengeFailed = true
	return nil
}

func (f *mobileAuthRepositoryFake) SMSChallengeReady(context.Context, uuid.UUID, string, SMSPurpose) error {
	return f.readyErr
}

func (f *mobileAuthRepositoryFake) ConsumeSMSChallenge(_ context.Context, _ uuid.UUID, _ string, _ SMSPurpose, verified bool) error {
	f.consumeVerified = verified
	if !verified {
		return ErrSMSInvalid
	}
	return nil
}

func (f *mobileAuthRepositoryFake) RegisterMobileIdentity(_ context.Context, _, _, username, mobile string) (uuid.UUID, error) {
	f.registeredUsername = username
	f.registeredMobile = mobile
	return uuid.MustParse("018f0f76-3f5d-7d43-b630-163e3f635e5a"), nil
}

type smsProviderFake struct {
	sentChallenge uuid.UUID
	sendErr       error
	verifyCalls   int
	verified      bool
	verifyErr     error
}

func (f *smsProviderFake) SendCode(_ context.Context, _ string, _ SMSPurpose, challengeID uuid.UUID) (string, error) {
	f.sentChallenge = challengeID
	return "provider-message", f.sendErr
}

func (f *smsProviderFake) VerifyCode(context.Context, string, uuid.UUID, string) (bool, error) {
	f.verifyCalls++
	return f.verified, f.verifyErr
}

type identityProvisionerFake struct {
	createCalls int
	username    string
	mobile      string
}

func (f *identityProvisionerFake) CreateMobileUser(_ context.Context, username, mobile, _ string) (string, error) {
	f.createCalls++
	f.username = username
	f.mobile = mobile
	return "keycloak-subject", nil
}

func (*identityProvisionerFake) DeleteUser(context.Context, string) error { return nil }

func newMobileAuthForTest(t *testing.T, repository MobileAuthRepository, sms SMSProvider, identities IdentityProvisioner) *MobileAuthService {
	t.Helper()
	service, err := NewMobileAuthService(repository, sms, identities, "https://account.example.com/realms/pinvou")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, time.August, 20, 3, 0, 0, 0, time.UTC) }
	return service
}

func TestMobileAuthSendUsesProviderGeneratedChallenge(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	challenge, err := service.Send(context.Background(), "13800138000", SMSPurposeRegister)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.ID == uuid.Nil || challenge.ID != repository.challenge.ID || challenge.ID != sms.sentChallenge || !repository.challengeSent {
		t.Fatalf("challenge was not consistently persisted and sent: %#v", challenge)
	}
}

func TestMobileAuthSendMarksProviderFailureAndKeepsUnavailableClassification(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{sendErr: errors.New("provider rejected request")}
	service := newMobileAuthForTest(t, repository, sms, &identityProvisionerFake{})
	_, err := service.Send(context.Background(), "13800138000", SMSPurposeRegister)
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
	challenge, err := service.Send(context.Background(), "13800138000", SMSPurposeLogin)
	if err != nil || challenge.ID == uuid.Nil {
		t.Fatalf("unknown account response must be indistinguishable: challenge=%#v err=%v", challenge, err)
	}
	if sms.sentChallenge != uuid.Nil || repository.challengeSent {
		t.Fatal("SMS must not be sent to an unknown login account")
	}
	if err := service.VerifyLogin(context.Background(), "13800138000", challenge.ID, "123456"); !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("unexpected verification error: %v", err)
	}
	if sms.verifyCalls != 0 {
		t.Fatal("local challenge state must be checked before calling Aliyun")
	}
}

func TestMobileAuthRegisterVerifiesWithProviderBeforeProvisioning(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{verified: true}
	identities := &identityProvisionerFake{}
	service := newMobileAuthForTest(t, repository, sms, identities)
	userID, err := service.Register(context.Background(), " Pinvou.User ", "13800138000", uuid.New(), "123456", "pinvou2026")
	if err != nil || userID == uuid.Nil {
		t.Fatalf("register userID=%s err=%v", userID, err)
	}
	if sms.verifyCalls != 1 || !repository.consumeVerified || identities.createCalls != 1 {
		t.Fatalf("verification/provisioning flow mismatch: sms=%d consumed=%v identity=%d", sms.verifyCalls, repository.consumeVerified, identities.createCalls)
	}
	if identities.username != "pinvou.user" || repository.registeredUsername != "pinvou.user" || repository.registeredMobile != "+8613800138000" {
		t.Fatalf("normalized identity was not shared across systems: identity=%q repository=%q mobile=%q", identities.username, repository.registeredUsername, repository.registeredMobile)
	}
}

func TestMobileAuthRegisterRejectsFailedProviderVerification(t *testing.T) {
	repository := &mobileAuthRepositoryFake{}
	sms := &smsProviderFake{verified: false}
	identities := &identityProvisionerFake{}
	service := newMobileAuthForTest(t, repository, sms, identities)
	_, err := service.Register(context.Background(), "pinvou.user", "13800138000", uuid.New(), "000000", "pinvou2026")
	if !errors.Is(err, ErrSMSInvalid) {
		t.Fatalf("expected invalid SMS code, got %v", err)
	}
	if identities.createCalls != 0 {
		t.Fatal("identity must not be provisioned after a failed SMS verification")
	}
}
