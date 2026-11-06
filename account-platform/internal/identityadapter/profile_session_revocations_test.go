package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

type profileRevocationDirectoryFake struct {
	directoryFake
	calls   atomic.Int64
	unknown bool
}

func (d *profileRevocationDirectoryFake) RevokeSessions(context.Context, string) error {
	d.calls.Add(1)
	if d.unknown {
		return errors.New("controlled lost provider acknowledgement")
	}
	return nil
}
func profileRevocationService(t *testing.T, d *profileRevocationDirectoryFake) *Service {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), d)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(vault, "controlled-profile-service-token")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func callProfileRevocation(s *Service, subject string, id uuid.UUID, first bool) int {
	b, _ := json.Marshal(map[string]any{"operation_id": id, "first_attempt": first})
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/users/"+subject+"/profile-revocations", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer controlled-profile-service-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}
func TestProfileSessionRevocationReceipts(t *testing.T) {
	t.Run("confirmed-concurrent-replay-one-provider-effect", func(t *testing.T) {
		d := &profileRevocationDirectoryFake{}
		s := profileRevocationService(t, d)
		id := uuid.New()
		if got := callProfileRevocation(s, "subject", id, true); got != 204 {
			t.Fatal(got)
		}
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(first bool) {
				defer wg.Done()
				if got := callProfileRevocation(s, "subject", id, first); got != 204 {
					t.Errorf("replay %d", got)
				}
			}(i%2 == 0)
		}
		wg.Wait()
		if d.calls.Load() != 1 {
			t.Fatal("replay repeated logout", d.calls.Load())
		}
		if got := callProfileRevocation(s, "other-subject", id, false); got != 409 || d.calls.Load() != 1 {
			t.Fatal("foreign identity borrowed receipt", got)
		}
	})
	for _, mode := range []string{"unknown-ack", "restart", "missing"} {
		t.Run(mode, func(t *testing.T) {
			d := &profileRevocationDirectoryFake{unknown: mode == "unknown-ack"}
			s := profileRevocationService(t, d)
			id := uuid.New()
			if mode != "missing" {
				want := 204
				if mode == "unknown-ack" {
					want = 503
				}
				if got := callProfileRevocation(s, "subject", id, true); got != want {
					t.Fatal(got)
				}
			}
			if mode == "restart" {
				s = profileRevocationService(t, d)
			}
			before := d.calls.Load()
			for range 3 {
				if got := callProfileRevocation(s, "subject", id, false); got != 503 {
					t.Fatal("unproven retry accepted", got)
				}
			}
			if d.calls.Load() != before {
				t.Fatal("unknown or restarted receipt repeated logout")
			}
		})
	}
	t.Run("required-command-and-authentication", func(t *testing.T) {
		d := &profileRevocationDirectoryFake{}
		s := profileRevocationService(t, d)
		for _, body := range []string{"", `{}`, `{"operation_id":"` + uuid.NewString() + `"}`, `{"first_attempt":true}`, `{"operation_id":"` + uuid.NewString() + `","first_attempt":null}`} {
			r := httptest.NewRequest(http.MethodPost, "/internal/v1/users/subject/profile-revocations", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer controlled-profile-service-token")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatalf("command status %d", w.Code)
			}
		}
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/users/subject/profile-revocations", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 || d.calls.Load() != 0 {
			t.Fatal("missing authority reached provider", w.Code)
		}
	})
}
func TestProfileSessionRevocationCancelledBeforeDispatchCanResume(t *testing.T) {
	d := &profileRevocationDirectoryFake{}
	v := newProfileSessionRevocations(d)
	id := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := v.revoke(ctx, id, "subject", true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if d.calls.Load() != 0 {
		t.Fatal("cancelled before dispatch")
	}
	if err := v.revoke(context.Background(), id, "subject", false); err != nil || d.calls.Load() != 1 {
		t.Fatal("proven undispatched receipt did not resume", err)
	}
}
