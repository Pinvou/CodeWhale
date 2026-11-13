package identityadapter

import (
	"context"
	"encoding/json"
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

type preparationCountingBody struct {
	io.Reader
	reads atomic.Int32
}

func (b *preparationCountingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.Reader.Read(p)
}

func preparationWaitHTTP(handler http.Handler, ctx context.Context, token string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", body).WithContext(ctx)
	r.RemoteAddr = "192.0.2.10:4242"
	r.Header.Set("X-Device-ID", "fixture-device")
	r.Header.Set("X-Pinvou-Password-Preparation", token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func assertPreparationUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"PASSWORD_PREPARATION_UNAVAILABLE"`) {
		t.Fatal("interrupted wait must use existing retryable failure", w.Code, w.Body.String())
	}
}

func awaitPreparationUsers(t *testing.T, state *passwordPreparationState, id uuid.UUID, want int) {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		state.mu.Lock()
		lock := state.locks[id]
		registered := lock != nil && lock.users == want
		state.mu.Unlock()
		if registered {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("request never registered behind the held owner")
		}
	}
}

func TestPasswordPreparationAlreadyCanceledHasNoAdmissionEffect(t *testing.T) {
	s, verifier, token := preparationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := &preparationCountingBody{Reader: strings.NewReader(`{"password":"FixturePassword9"}`)}
	w := preparationWaitHTTP(s.Handler(), ctx, token, body)
	assertPreparationUnavailable(t, w)
	s.preparations.mu.Lock()
	locks, records := len(s.preparations.locks), len(s.preparations.records)
	s.preparations.mu.Unlock()
	if verifier.calls.Load() != 0 || body.reads.Load() != 0 || locks != 0 || records != 0 || len(s.vault.records) != 0 {
		t.Fatal("canceled request retained work, authority, body or credential state")
	}
	if next := preparationHTTP(s, token, "FixturePassword9"); next.Code != http.StatusCreated {
		t.Fatal("cancellation poisoned the next request", next.Code)
	}
}

// Hold actual handler authority I/O at a channel barrier. Observe the waiter's
// registration, cancel it, and demand it finish while the owner is still held.
func TestPasswordPreparationCanceledWaitPreservesOwner(t *testing.T) {
	for _, purpose := range []string{"REGISTER", "PASSWORD_RESET"} {
		t.Run(purpose, func(t *testing.T) {
			s, fixture, token := preparationFixture(t)
			facts := fixture.facts
			facts.Purpose = purpose
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var calls atomic.Int32
			s.preparations.verifier = preparationClaimFunc(func(context.Context, passwordpreparation.Claim) (passwordpreparation.Facts, error) {
				if calls.Add(1) == 1 {
					close(started)
					<-release
				}
				return facts, nil
			})
			handler := s.Handler()
			ownerDone, waiterDone := make(chan struct{}), make(chan struct{})
			var owner, waiter *httptest.ResponseRecorder
			waiterStarted := false
			t.Cleanup(func() {
				unblock()
				for _, done := range []struct {
					started bool
					channel <-chan struct{}
				}{{true, ownerDone}, {waiterStarted, waiterDone}} {
					if done.started {
						select {
						case <-done.channel:
						case <-time.After(3 * time.Second):
							t.Error("owned request did not retire after barrier release")
						}
					}
				}
			})
			go func() {
				owner = preparationWaitHTTP(handler, context.Background(), token, strings.NewReader(`{"password":"FixturePassword9"}`))
				close(ownerDone)
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("owner did not reach the authority barrier")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &preparationCountingBody{Reader: strings.NewReader(`{"password":"FixturePassword9"}`)}
			waiterStarted = true
			go func() {
				waiter = preparationWaitHTTP(handler, ctx, token, body)
				close(waiterDone)
			}()
			awaitPreparationUsers(t, s.preparations, facts.ID, 2)
			cancel()
			stopped := false
			select {
			case <-waiterDone:
				stopped = true
				assertPreparationUnavailable(t, waiter)
			case <-time.After(2 * time.Second):
				t.Error("canceled waiter remained blocked behind authority I/O")
			}
			if stopped {
				s.preparations.mu.Lock()
				lock := s.preparations.locks[facts.ID]
				ownerPreserved := lock != nil && lock.users == 1 && len(s.preparations.records) == 0
				s.preparations.mu.Unlock()
				if !ownerPreserved || calls.Load() != 1 || body.reads.Load() != 0 {
					t.Fatal("cancellation released owner, claimed again or decoded a password")
				}
			}
			unblock()
			select {
			case <-ownerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("owner did not recover")
			}
			if owner.Code != http.StatusCreated {
				t.Fatal("owner result changed", owner.Code)
			}
			select {
			case <-waiterDone:
			case <-time.After(3 * time.Second):
				t.Fatal("waiter did not retire after owner recovered")
			}
			var original, replay PasswordReference
			if json.Unmarshal(owner.Body.Bytes(), &original) != nil || original.Handle == "" {
				t.Fatal("owner did not produce a reference")
			}
			w := preparationHTTP(s, token, "FixturePassword9")
			if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &replay) != nil || replay != original || calls.Load() != 1 {
				t.Fatal("next request did not replay the owner's original result")
			}
			if changed := preparationHTTP(s, token, "DifferentPassword8"); changed.Code != http.StatusConflict {
				t.Fatal("cancellation allowed changed secret replacement", changed.Code)
			}
			s.preparations.mu.Lock()
			locks, records := len(s.preparations.locks), len(s.preparations.records)
			s.preparations.mu.Unlock()
			if locks != 0 || records != 1 || len(s.vault.records) != 1 {
				t.Fatal("waiter bookkeeping leaked or duplicated credentials", locks, records)
			}
		})
	}
}

func TestPasswordPreparationDefaultWaitBudgetPreservesOwner(t *testing.T) {
	s, verifier, token := preparationFixture(t)
	unlock, err := s.preparations.lockGrant(context.Background(), verifier.facts.ID)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(unlock) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var response *httptest.ResponseRecorder
	body := &preparationCountingBody{Reader: strings.NewReader(`{"password":"FixturePassword9"}`)}
	handler := s.Handler()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("budget probe request did not retire after release")
		}
	})
	go func() {
		response = preparationWaitHTTP(handler, ctx, token, body)
		close(done)
	}()
	awaitPreparationUsers(t, s.preparations, verifier.facts.ID, 2)
	select {
	case <-done:
		assertPreparationUnavailable(t, response)
	case <-time.After(20 * time.Second):
		t.Fatal("queue wait outlived the default finite budget; watchdog is not a product pass")
	}
	s.preparations.mu.Lock()
	lock := s.preparations.locks[verifier.facts.ID]
	ownerPreserved := lock != nil && lock.users == 1 && len(s.preparations.records) == 0
	s.preparations.mu.Unlock()
	if !ownerPreserved || verifier.calls.Load() != 0 || body.reads.Load() != 0 || len(s.vault.records) != 0 {
		t.Fatal("wait timeout affected owner or started authority/password work")
	}
	release()
	if next := preparationHTTP(s, token, "FixturePassword9"); next.Code != http.StatusCreated || verifier.calls.Load() != 1 {
		t.Fatal("next request did not recover after default wait timeout", next.Code)
	}
}
