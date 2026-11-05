package account

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This controlled store records production HTTP/service call order. The actual
// PostgreSQL mobile/purpose/one-shot consumer is unchanged, not replaced by a
// claimed database integration test here.
type registrationBoundaryRepository struct {
	mobileAuthRepositoryFake
	events   []string
	reads    int
	used     bool
	finalErr error
}

func (r *registrationBoundaryRepository) AdmitPublicMobileRequest(ctx context.Context, operation, mobile, source string) error {
	r.events = append(r.events, operation)
	return r.mobileAuthRepositoryFake.AdmitPublicMobileRequest(ctx, operation, mobile, source)
}
func (r *registrationBoundaryRepository) MobileExists(ctx context.Context, mobile string) (bool, error) {
	r.events = append(r.events, "mobile")
	r.reads++
	return r.mobileAuthRepositoryFake.MobileExists(ctx, mobile)
}
func (r *registrationBoundaryRepository) UsernameExists(ctx context.Context, name string) (bool, error) {
	r.events = append(r.events, "username")
	r.reads++
	return r.mobileAuthRepositoryFake.UsernameExists(ctx, name)
}
func (r *registrationBoundaryRepository) ConsumeSliderCaptcha(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose, offset int) error {
	r.events = append(r.events, "captcha")
	if r.used {
		return ErrCaptchaInvalid
	}
	err := r.mobileAuthRepositoryFake.ConsumeSliderCaptcha(ctx, id, mobile, purpose, offset)
	if err == nil {
		r.used = true
	}
	return err
}
func (r *registrationBoundaryRepository) CreateRegistrationOperation(ctx context.Context, op RegistrationOperation, c SMSChallenge) error {
	r.events = append(r.events, "operation")
	if r.finalErr != nil {
		return r.finalErr
	}
	return r.mobileAuthRepositoryFake.CreateRegistrationOperation(ctx, op, c)
}

type registrationBoundaryIdentity struct {
	*identityProvisionerFake
	repository *registrationBoundaryRepository
	err        error
}

func (i *registrationBoundaryIdentity) ValidateMobileUser(_ context.Context, id uuid.UUID, _, _, _, _ string) error {
	event := "identity-precheck"
	if id != uuid.Nil {
		event = "identity-bind"
	}
	i.repository.events = append(i.repository.events, event)
	return i.err
}

func registrationBoundaryService(t *testing.T, r *registrationBoundaryRepository, identityErr error) (http.Handler, *smsProviderFake) {
	t.Helper()
	sms := &smsProviderFake{}
	id := &registrationBoundaryIdentity{identityProvisionerFake: &identityProvisionerFake{}, repository: r, err: identityErr}
	auth := newMobileAuthForTest(t, r, sms, id)
	return NewService(nil, nil).WithMobileAuth(auth, "controlled-internal-fixture-token-123456").Handler(nil), sms
}
func registrationBoundaryRequest(handler http.Handler, path string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.14:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-ID", "controlled-device")
	req.Header.Set("X-Pinvou-Client-Source", "203.0.113.99")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}
func registrationBoundarySMSBody() string {
	return `{"username":"Pinvou.User","mobile":"13800138000","password_handle":"controlled-handle","password_policy_version":"controlled-policy","purpose":"REGISTER","captcha_id":"11111111-1111-4111-8111-111111111111","captcha_offset":128}`
}

func TestRegistrationCaptchaPrecheckBoundary(t *testing.T) {
	t.Run("retired-public-precheck-is-not-an-availability-oracle", func(t *testing.T) {
		r := &registrationBoundaryRepository{}
		handler, sms := registrationBoundaryService(t, r, nil)
		w := registrationBoundaryRequest(handler, "/v1/auth/registration/precheck", `{"username":"pinvou.user","mobile":"13800138000"}`)
		if w.Code != 404 || len(r.events) != 0 || sms.sendCalls != 0 {
			t.Fatal(w.Code, r.events, sms.sendCalls)
		}
	})
	t.Run("challenge-generation-does-not-read-registration-availability", func(t *testing.T) {
		r := &registrationBoundaryRepository{mobileAuthRepositoryFake: mobileAuthRepositoryFake{mobileExists: true, usernameExists: true}}
		handler, sms := registrationBoundaryService(t, r, nil)
		w := registrationBoundaryRequest(handler, "/v1/auth/captcha/challenges", `{"mobile":"13800138000","purpose":"REGISTER"}`)
		if w.Code != 201 || r.reads != 0 || !reflect.DeepEqual(r.events, []string{"CAPTCHA"}) || sms.sendCalls != 0 {
			t.Fatal(w.Code, r.events, w.Body.String())
		}
	})
	t.Run("invalid-captcha-stops-before-precheck-admission-and-identity", func(t *testing.T) {
		r := &registrationBoundaryRepository{mobileAuthRepositoryFake: mobileAuthRepositoryFake{captchaErr: ErrCaptchaInvalid}}
		handler, sms := registrationBoundaryService(t, r, nil)
		w := registrationBoundaryRequest(handler, "/v1/auth/sms/challenges", registrationBoundarySMSBody())
		if w.Code != 400 || !strings.Contains(w.Body.String(), "CAPTCHA_INVALID") || !reflect.DeepEqual(r.events, []string{"captcha"}) || sms.sendCalls != 0 {
			t.Fatal(w.Code, r.events, w.Body.String())
		}
	})
	for _, failure := range []string{"username", "mobile", "password", "rate", "final-unique"} {
		t.Run(failure+"-failure-after-captcha-sends-no-sms", func(t *testing.T) {
			r := &registrationBoundaryRepository{}
			var identityErr error
			want := 409
			switch failure {
			case "username":
				r.usernameExists = true
			case "mobile":
				r.mobileExists = true
			case "password":
				identityErr = ErrPasswordReferenceInvalid
				want = http.StatusGone
			case "rate":
				r.publicAdmissionErr = &SMSRateLimitError{RetryAfter: time.Minute}
				want = 429
			case "final-unique":
				r.finalErr = ErrUsernameAlreadyRegistered
			}
			handler, sms := registrationBoundaryService(t, r, identityErr)
			w := registrationBoundaryRequest(handler, "/v1/auth/sms/challenges", registrationBoundarySMSBody())
			if w.Code != want || sms.sendCalls != 0 || r.operation.ID != uuid.Nil || len(r.events) < 2 || r.events[0] != "captcha" || r.events[1] != "PRECHECK" {
				t.Fatal(w.Code, r.events, w.Body.String())
			}
			before := len(r.events)
			replay := registrationBoundaryRequest(handler, "/v1/auth/sms/challenges", registrationBoundarySMSBody())
			if replay.Code != 400 || len(r.events) != before+1 || r.events[before] != "captcha" {
				t.Fatal("consumed proof was reused", replay.Code, r.events)
			}
		})
	}
	t.Run("fresh-challenge-preserves-original-fields-and-admission-once", func(t *testing.T) {
		r := &registrationBoundaryRepository{}
		handler, sms := registrationBoundaryService(t, r, nil)
		w := registrationBoundaryRequest(handler, "/v1/auth/sms/challenges", registrationBoundarySMSBody())
		want := []string{"captcha", "PRECHECK", "identity-precheck", "username", "mobile", "mobile", "identity-bind", "operation"}
		if w.Code != 202 || !reflect.DeepEqual(r.events, want) || sms.sendCalls != 1 {
			t.Fatal(w.Code, r.events, w.Body.String())
		}
		if r.publicAdmissionSource != "198.51.100.14" || r.challenge.Source != "198.51.100.14" || r.challenge.Device != "controlled-device" || r.challenge.Mobile != "+8613800138000" || r.challenge.Purpose != SMSPurposeRegister || r.operation.Username != "pinvou.user" {
			t.Fatal(r.publicAdmissionSource, r.challenge, r.operation.Username)
		}
		var response map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response["registration_operation_id"] == nil || response["registration_query_token"] == nil {
			t.Fatal(response, err)
		}
	})
	t.Run("precheck-rejection-retries-only-with-new-challenge", func(t *testing.T) {
		r := &registrationBoundaryRepository{mobileAuthRepositoryFake: mobileAuthRepositoryFake{usernameExists: true}}
		handler, sms := registrationBoundaryService(t, r, nil)
		if w := registrationBoundaryRequest(handler, "/v1/auth/sms/challenges", registrationBoundarySMSBody()); w.Code != 409 {
			t.Fatal(w.Code)
		}
		r.usernameExists = false
		r.used = false
		r.events = nil
		body := strings.ReplaceAll(registrationBoundarySMSBody(), "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
		w := registrationBoundaryRequest(handler, "/v1/auth/sms/challenges", body)
		if w.Code != 202 || sms.sendCalls != 1 || r.events[0] != "captcha" || r.events[1] != "PRECHECK" {
			t.Fatal(w.Code, r.events)
		}
	})
}
