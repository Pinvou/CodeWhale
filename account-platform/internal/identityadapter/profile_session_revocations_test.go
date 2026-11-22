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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
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

type profileAuthorityReceipt struct {
	subject string
	state   string
	token   uuid.UUID
}

// This test authority survives adapter reconstruction. The Account integration
// root separately exercises the real durable admission and SQL state machine.
type profileAuthorityFake struct {
	mu       sync.Mutex
	receipts map[uuid.UUID]profileAuthorityReceipt
}

func newProfileAuthority(id uuid.UUID) *profileAuthorityFake {
	return &profileAuthorityFake{receipts: map[uuid.UUID]profileAuthorityReceipt{id: {subject: "subject", state: "RESERVED"}}}
}

func (a *profileAuthorityFake) ClaimProfileRevocation(_ context.Context, command profilerevocation.Command) (profilerevocation.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	receipt, ok := a.receipts[command.OperationID]
	if !ok {
		return profilerevocation.Result{}, profilerevocation.ErrUnconfirmed
	}
	if receipt.subject != command.Subject {
		return profilerevocation.Result{}, profilerevocation.ErrConflict
	}
	if receipt.state == "CONFIRMED" {
		return profilerevocation.Result{State: "CONFIRMED"}, nil
	}
	if receipt.state != "RESERVED" || command.ReplayOnly {
		return profilerevocation.Result{}, profilerevocation.ErrUnconfirmed
	}
	receipt.state, receipt.token = "DISPATCHED", command.DispatchToken
	a.receipts[command.OperationID] = receipt
	return profilerevocation.Result{State: "DISPATCH"}, nil
}

func (a *profileAuthorityFake) ConfirmProfileRevocation(_ context.Context, command profilerevocation.Command) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	receipt := a.receipts[command.OperationID]
	if receipt.subject != command.Subject || receipt.token != command.DispatchToken {
		return profilerevocation.ErrUnconfirmed
	}
	receipt.state = "CONFIRMED"
	a.receipts[command.OperationID] = receipt
	return nil
}

func profileRevocationService(t *testing.T, d *profileRevocationDirectoryFake, authority *profileAuthorityFake) *Service {
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
	if authority != nil {
		if _, err = s.WithProfileRevocationAuthority(authority); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func callProfileRevocation(s *Service, subject string, id uuid.UUID, first bool) int {
	b, _ := json.Marshal(map[string]any{"operation_id": id, "first_attempt": first, "receipt_protocol": 2})
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/users/"+subject+"/profile-revocations", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer controlled-profile-service-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}
func TestProfileSessionRevocationReceipts(t *testing.T) {
	t.Run("confirmed-concurrent-replay-one-provider-effect", func(t *testing.T) {
		d := &profileRevocationDirectoryFake{}
		id := uuid.New()
		authority := newProfileAuthority(id)
		s := profileRevocationService(t, d, authority)
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
			id := uuid.New()
			authority := newProfileAuthority(id)
			if mode == "missing" {
				delete(authority.receipts, id)
			}
			s := profileRevocationService(t, d, authority)
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
				s = profileRevocationService(t, d, authority)
			}
			before := d.calls.Load()
			for range 3 {
				want := 503
				if mode == "restart" {
					want = 204
				}
				if got := callProfileRevocation(s, "subject", id, false); got != want {
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
		s := profileRevocationService(t, d, nil)
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
	v.authority = newProfileAuthority(id)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := v.revoke(ctx, id, "subject", false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if d.calls.Load() != 0 {
		t.Fatal("cancelled before dispatch")
	}
	if err := v.revoke(context.Background(), id, "subject", false); err != nil || d.calls.Load() != 1 {
		t.Fatal("proven undispatched receipt did not resume", err)
	}
}
