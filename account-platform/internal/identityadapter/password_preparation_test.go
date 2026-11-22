package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/google/uuid"
)

type preparationVerifierFixture struct {
	calls atomic.Int32
	facts passwordpreparation.Facts
	err   error
}

type preparationDeadlineReader struct {
	io.Reader
	advance func()
}

func (r preparationDeadlineReader) Read(p []byte) (int, error) {
	r.advance()
	return r.Reader.Read(p)
}

func (v *preparationVerifierFixture) Claim(_ context.Context, c passwordpreparation.Claim) (passwordpreparation.Facts, error) {
	v.calls.Add(1)
	if c.Source != "192.0.2.10" || c.Device != "fixture-device" || c.Owner == uuid.Nil {
		return passwordpreparation.Facts{}, ErrPasswordPreparation
	}
	return v.facts, v.err
}
func preparationHTTP(service *Service, token, password string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", strings.NewReader(`{"password":"`+password+`"}`))
	r.RemoteAddr = "192.0.2.10:4242"
	r.Header.Set("X-Device-ID", "fixture-device")
	if token != "" {
		r.Header.Set("X-Pinvou-Password-Preparation", token)
	}
	w := httptest.NewRecorder()
	service.Handler().ServeHTTP(w, r)
	return w
}
func preparationFixture(t *testing.T) (*Service, *preparationVerifierFixture, string) {
	t.Helper()
	v, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(v, "controlled-development-service-token")
	if err != nil {
		t.Fatal(err)
	}
	verifier := &preparationVerifierFixture{facts: passwordpreparation.Facts{ID: uuid.New(), Mobile: "+8613800138000", Purpose: "REGISTER", ExpiresAt: time.Now().Add(time.Minute)}}
	service, err = service.WithPasswordPreparation(verifier, "")
	if err != nil {
		t.Fatal(err)
	}
	return service, verifier, verifier.facts.ID.String() + ".PUBLIC_SYNTHETIC_MAC"
}

func TestPasswordPreparationHTTPAdmissionAndReplay(t *testing.T) {
	t.Run("body-read-cannot-extend-the-admission-deadline", func(t *testing.T) {
		service, verifier, token := preparationFixture(t)
		current := verifier.facts.ExpiresAt.Add(-time.Second)
		service.vault.now = func() time.Time { return current }
		request := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", preparationDeadlineReader{
			Reader:  strings.NewReader(`{"password":"FixturePassword9"}`),
			advance: func() { current = verifier.facts.ExpiresAt.Add(time.Second) },
		})
		request.RemoteAddr = "192.0.2.10:4242"
		request.Header.Set("X-Device-ID", "fixture-device")
		request.Header.Set("X-Pinvou-Password-Preparation", token)
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		if response.Code != 403 || len(service.vault.records) != 0 {
			t.Fatal("slow body allocated beyond admission", response.Code)
		}
	})
	t.Run("reset-reference-applies-once-with-the-original-operation", func(t *testing.T) {
		service, verifier, token := preparationFixture(t)
		verifier.facts.Purpose = "PASSWORD_RESET"
		response := preparationHTTP(service, token, "FixturePassword9")
		var reference PasswordReference
		if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &reference) != nil {
			t.Fatal("reset preparation", response.Code)
		}
		if err := service.vault.ValidateReference(uuid.New(), reference.Handle, reference.PolicyVersion, "fixture-subject", verifier.facts.Mobile); !errors.Is(err, ErrReferenceConflict) {
			t.Fatal("reset reference became registration", err)
		}
		operation := uuid.New()
		for range 2 {
			if err := service.vault.ResetPassword(context.Background(), operation, 1, reference.Handle, reference.PolicyVersion, "fixture-subject", verifier.facts.Mobile); err != nil {
				t.Fatal("same reset recovery", err)
			}
		}
		directory := service.vault.directory.(*directoryFake)
		if directory.password != "FixturePassword9" || directory.resetOperationID != operation {
			t.Fatal("reset producer did not preserve original operation")
		}
	})
	t.Run("anonymous-missing-and-unavailable-verifier-never-allocate", func(t *testing.T) {
		service, verifier, token := preparationFixture(t)
		if w := preparationHTTP(service, "", "FixturePassword9"); w.Code != 403 {
			t.Fatal("unadmitted allocation", w.Code, w.Body.String())
		}
		if len(service.vault.records) != 0 || verifier.calls.Load() != 0 {
			t.Fatal("missing capability read a password or allocated")
		}
		verifier.err = errors.New("controlled authority outage")
		if w := preparationHTTP(service, token, "FixturePassword9"); w.Code != 503 || len(service.vault.records) != 0 {
			t.Fatal("authority outage allocated", w.Code)
		}
		service.preparations = nil
		if w := preparationHTTP(service, token, "FixturePassword9"); w.Code != 503 || len(service.vault.records) != 0 {
			t.Fatal("missing configuration allocated", w.Code)
		}
	})
	t.Run("same-grant-concurrent-and-unknown-ACK-return-original-handle", func(t *testing.T) {
		service, verifier, token := preparationFixture(t)
		var wg sync.WaitGroup
		handles := make(chan PasswordReference, 12)
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := preparationHTTP(service, token, "FixturePassword9")
				var r PasswordReference
				if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
					t.Error("admitted preparation", w.Code)
					return
				}
				handles <- r
			}()
		}
		wg.Wait()
		close(handles)
		var original PasswordReference
		for item := range handles {
			if original.Handle == "" {
				original = item
			}
			if item != original {
				t.Fatal("grant allocated or renewed twice")
			}
		}
		if original.Handle == "" || len(service.vault.records) != 1 || verifier.calls.Load() != 1 {
			t.Fatal("one grant must allocate exactly one record", len(service.vault.records), verifier.calls.Load())
		}
		if w := preparationHTTP(service, token, "DifferentPassword8"); w.Code != 409 || len(service.vault.records) != 1 {
			t.Fatal("changed secret reused grant", w.Code)
		}
		// The original admission window may end after an ACK-lost allocation. Only the existing receipt is replayed.
		afterAdmission := verifier.facts.ExpiresAt.Add(time.Second)
		service.vault.now = func() time.Time { return afterAdmission }
		if w := preparationHTTP(service, token, "FixturePassword9"); w.Code != 201 || verifier.calls.Load() != 1 {
			t.Fatal("read-only receipt replay lost", w.Code)
		}
	})
	t.Run("expired-new-grant-and-wrong-source-device-fail-closed", func(t *testing.T) {
		service, verifier, token := preparationFixture(t)
		verifier.facts.ExpiresAt = time.Now().Add(-time.Second)
		if w := preparationHTTP(service, token, "FixturePassword9"); w.Code != 403 || len(service.vault.records) != 0 {
			t.Fatal("expired grant allocated", w.Code)
		}
		verifier.facts.ExpiresAt = time.Now().Add(time.Minute)
		r := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", strings.NewReader(`{"password":"FixturePassword9"}`))
		r.RemoteAddr = "198.51.100.2:1111"
		r.Header.Set("X-Device-ID", "fixture-device")
		r.Header.Set("X-Pinvou-Password-Preparation", token)
		r.Header.Set("X-Pinvou-Client-Source", "192.0.2.10")
		w := httptest.NewRecorder()
		service.Handler().ServeHTTP(w, r)
		if w.Code != 403 || len(service.vault.records) != 0 {
			t.Fatal("untrusted forwarded source allocated", w.Code)
		}
	})
	t.Run("new-purpose-binding-and-old-accepted-reference-compatibility", func(t *testing.T) {
		service, _, token := preparationFixture(t)
		w := preparationHTTP(service, token, "FixturePassword9")
		var r PasswordReference
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
			t.Fatal(w.Code)
		}
		if err := service.vault.validatePreparedPurpose(r.Handle, r.PolicyVersion, "PASSWORD_RESET", "+8613800138000"); !errors.Is(err, ErrReferenceConflict) {
			t.Fatal("registration grant became reset capability", err)
		}
		if err := service.vault.validatePreparedPurpose(r.Handle, r.PolicyVersion, "REGISTER", "+8613900139000"); !errors.Is(err, ErrReferenceConflict) {
			t.Fatal("mobile binding lost", err)
		}
		old, err := service.vault.Precheck("ExistingAccepted7")
		if err != nil {
			t.Fatal(err)
		}
		if err = service.vault.ValidateReference(uuid.Nil, old.Handle, old.PolicyVersion, "legacy-user", "+8613800138000"); err != nil {
			t.Fatal("existing accepted handle revoked", err)
		}
	})
	t.Run("retained-handle-capacity-still-fails-without-eviction", func(t *testing.T) {
		service, verifier, token := preparationFixture(t)
		service.vault.capacity = 1
		first := preparationHTTP(service, token, "FixturePassword9")
		if first.Code != 201 {
			t.Fatal(first.Code)
		}
		verifier.facts.ID = uuid.New()
		token = verifier.facts.ID.String() + ".PUBLIC_SYNTHETIC_MAC"
		second := preparationHTTP(service, token, "FixturePassword9")
		if second.Code != 503 || len(service.vault.records) != 1 || !strings.Contains(second.Body.String(), "PASSWORD_PRECHECK_CAPACITY_REACHED") {
			t.Fatal("capacity policy changed", second.Code)
		}
	})
}
