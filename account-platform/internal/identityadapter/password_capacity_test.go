package identityadapter

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPasswordReferenceCapacityPreservesAcceptedHandles(t *testing.T) {
	v, err := NewPasswordVaultWithCapacity([]byte(strings.Repeat("k", 32)), &directoryFake{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	originalNow := now
	v.now = func() time.Time { return now }
	a, err := v.Precheck("Password123")
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if err = v.ValidateReference(op, a.Handle, a.PolicyVersion, "username", "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	b, err := v.Precheck("OtherPassword123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Precheck("ThirdPassword123"); !errors.Is(err, ErrPasswordReferenceCapacity) {
		t.Fatal(err)
	}
	if err = v.ValidateReference(op, a.Handle, a.PolicyVersion, "username", "+8613800138000"); err != nil {
		t.Fatal("accepted handle evicted", err)
	}
	if a.ExpiresAt.Sub(now.Add(-time.Minute)) != 15*time.Minute {
		t.Fatal("TTL changed")
	}
	now = now.Add(14*time.Minute + time.Second)
	if _, err = v.Precheck("ThirdPassword123"); err != nil {
		t.Fatal("expired capacity not released", err)
	}
	if _, err = v.record(b.Handle, b.PolicyVersion); err != nil {
		t.Fatal("live handle evicted", err)
	}
	if binding, ok := v.operations[op]; !ok || binding.handle != a.Handle || binding.expiresAt.Sub(originalNow) != 48*time.Hour {
		t.Fatal("accepted operation binding changed")
	}
}

func TestPasswordReferenceCapacityIsAtomic(t *testing.T) {
	v, err := NewPasswordVaultWithCapacity([]byte(strings.Repeat("k", 32)), &directoryFake{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Precheck("Password123")
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrPasswordReferenceCapacity) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 8 || len(v.records) != 8 {
		t.Fatal("capacity exceeded", accepted.Load(), len(v.records))
	}
}

func TestPasswordReferenceCapacityHTTPPreservesOriginalHandle(t *testing.T) {
	v, err := NewPasswordVaultWithCapacity([]byte(strings.Repeat("k", 32)), &directoryFake{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{vault: v}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", strings.NewReader(`{"password":"Password123"}`))
		w := httptest.NewRecorder()
		service.precheckPassword(w, r)
		return w
	}
	first := request()
	if first.Code != http.StatusCreated {
		t.Fatal("anonymous precheck compatibility lost", first.Code)
	}
	var original string
	for handle := range v.records {
		original = handle
	}
	second := request()
	if second.Code != http.StatusServiceUnavailable || !strings.Contains(second.Body.String(), "PASSWORD_PRECHECK_CAPACITY_REACHED") || strings.Contains(second.Body.String(), "password_handle") {
		t.Fatal("capacity denial fabricated a handle", second.Code, second.Body.String())
	}
	if _, err = v.record(original, PasswordPolicyVersion); err != nil {
		t.Fatal("accepted reference evicted", err)
	}
	if len(v.records) != 1 {
		t.Fatal("rejected request retained another record")
	}
}

func BenchmarkPasswordReferenceRetainedMemory(b *testing.B) {
	for range b.N {
		v, _ := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
		for range DefaultPasswordReferenceCapacity {
			if _, err := v.Precheck(strings.Repeat("p", 127) + "1"); err != nil {
				b.Fatal(err)
			}
		}
	}
}
