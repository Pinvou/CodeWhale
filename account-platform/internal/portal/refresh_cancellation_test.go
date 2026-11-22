package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The protocol server performs single-use rotation before releasing its response.
// The memory suite runs in the normal Go gate; the Redis suite uses the same
// scenarios against the production Lua implementation when explicitly configured.
func TestRefreshCancellationBoundaries(t *testing.T) {
	runRefreshCancellationBoundaries(t, func(t *testing.T, ids ...string) Store { return newMemoryStore() })
}

func TestRealRedisRefreshCancellationBoundaries(t *testing.T) {
	realRedisStore(t) // Report a missing fixture as a skipped root, not a green empty suite.
	runRefreshCancellationBoundaries(t, func(t *testing.T, ids ...string) Store { return realRedisStore(t, ids...) })
}

func TestRefreshCompletionDoesNotDetachProxyRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newMemoryStore()
	service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "test-next-access", RefreshToken: "test-next-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 600})
	}))
	var downstream atomic.Int32
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downstream.Add(1) }))
	defer account.Close()
	service.config.AccountURL = parseTestURL(t, account.URL)
	session := validSession(uuid.NewString())
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	store.SaveSession(context.Background(), session)
	request := httptest.NewRequest(http.MethodGet, "/api/account/v1/me", nil).WithContext(ctx)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	latest, err := store.GetSession(context.Background(), session.ID)
	if err != nil || latest.RefreshToken != "test-next-refresh" || downstream.Load() != 0 || response.Code < 400 {
		t.Fatalf("refresh completion detached downstream work: downstream=%d status=%d error=%v", downstream.Load(), response.Code, err)
	}
}

func TestRefreshCompletionRetainsOriginalBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &refreshCancellationStore{Store: newMemoryStore()}
	service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "test-next-access", RefreshToken: "test-next-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 600})
	}))
	id := uuid.NewString()
	session := validSession(id)
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	store.save = func(saveCtx context.Context, before, after Session, owner string) error {
		cancel()
		<-saveCtx.Done() // An unresponsive store must not create unlimited work.
		return saveCtx.Err()
	}
	started := time.Now()
	_, err := service.refreshSession(ctx, session)
	if elapsed := time.Since(started); !errors.Is(err, context.DeadlineExceeded) || elapsed < 19*time.Second || elapsed > 25*time.Second {
		t.Fatalf("refresh did not retain its original 20 second budget: %v after %s", err, elapsed)
	}
	if ok, err := store.TryRefreshLock(context.Background(), id, "next-owner"); err != nil || !ok {
		t.Fatal("budget exhaustion leaked the refresh lease")
	}
}

type refreshCancellationStore struct {
	Store
	try  func(context.Context, string, string) (bool, error)
	save func(context.Context, Session, Session, string) error
}

func (s *refreshCancellationStore) TryRefreshLock(ctx context.Context, id, owner string) (bool, error) {
	if s.try != nil {
		return s.try(ctx, id, owner)
	}
	return s.Store.TryRefreshLock(ctx, id, owner)
}

func (s *refreshCancellationStore) SaveRefreshedSession(ctx context.Context, before, after Session, owner string) error {
	if s.save != nil {
		return s.save(ctx, before, after, owner)
	}
	return s.Store.SaveRefreshedSession(ctx, before, after, owner)
}

func waitRefreshSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh synchronization timed out")
	}
}

func runRefreshCancellationBoundaries(t *testing.T, newStore func(*testing.T, ...string) Store) {
	for _, phase := range []string{"before-lock", "waiting-lock", "acquiring-lock"} {
		t.Run(phase, func(t *testing.T) {
			id := uuid.NewString()
			base := newStore(t, id)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			wrapped := &refreshCancellationStore{Store: base}
			service, _ := oidcTestService(t, wrapped, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				t.Error("cancelled admission dispatched a refresh")
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			session := validSession(id)
			session.AccessExpiresAt = time.Now().Add(-time.Minute)
			if err := base.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			session, err := base.GetSession(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "before-lock":
				cancel()
			case "waiting-lock":
				if ok, err := base.TryRefreshLock(context.Background(), id, "existing-owner"); err != nil || !ok {
					t.Fatal("could not seed existing lease")
				}
				defer base.ReleaseRefreshLock(context.Background(), id, "existing-owner")
				wrapped.try = func(ctx context.Context, id, owner string) (bool, error) {
					ok, err := base.TryRefreshLock(ctx, id, owner)
					cancel()
					return ok, err
				}
			case "acquiring-lock":
				wrapped.try = func(ctx context.Context, id, owner string) (bool, error) {
					ok, err := base.TryRefreshLock(ctx, id, owner)
					cancel() // The lock reply arrives after the caller has cancelled.
					return ok, err
				}
			}
			if _, err := service.refreshSession(ctx, session); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled admission result: %v", err)
			}
			latest, err := base.GetSession(context.Background(), id)
			if err != nil || latest != session || calls.Load() != 0 {
				t.Fatal("cancelled admission changed the session or contacted the IdP")
			}
			if phase != "waiting-lock" {
				if ok, err := base.TryRefreshLock(context.Background(), id, "next-owner"); err != nil || !ok {
					t.Fatal("cancelled acquisition leaked its lease")
				}
				base.ReleaseRefreshLock(context.Background(), id, "next-owner")
			}
		})
	}

	for _, phase := range []string{"before-rotation", "after-rotation", "during-save", "logout", "replacement", "changed-session", "changed-owner", "invalid-grant", "provider-timeout", "save-failure"} {
		t.Run(phase, func(t *testing.T) {
			id, replacementID := uuid.NewString(), uuid.NewString()
			base := newStore(t, id, replacementID)
			wrapped := &refreshCancellationStore{Store: base}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			var rotated atomic.Bool
			var signedIdentity string
			service, key := oidcTestService(t, wrapped, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != "portal" || r.PostForm.Get("refresh_token") != "test-refresh" {
					t.Error("refresh protocol facts changed")
				}
				if phase != "before-rotation" {
					rotated.Store(true)
				}
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				rotated.Store(true)
				if phase == "invalid-grant" {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":"invalid_grant"}`)
					return
				}
				json.NewEncoder(w).Encode(TokenResponse{IDToken: signedIdentity, AccessToken: "test-new-access", RefreshToken: "test-new-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 600})
			}))
			// Include signed identity verification after the caller has cancelled.
			signedIdentity = testIDToken(t, key, nil)
			session := validSession(id)
			session.AccessExpiresAt = time.Now().Add(-time.Minute)
			if err := base.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			session, err := base.GetSession(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			var saveCalls atomic.Int32
			wrapped.save = func(saveCtx context.Context, before, after Session, owner string) error {
				saveCalls.Add(1)
				if phase == "during-save" {
					cancel()
				}
				if err := saveCtx.Err(); err != nil {
					return err
				}
				deadline, bounded := saveCtx.Deadline()
				if !bounded || time.Until(deadline) > 20*time.Second {
					t.Error("admitted refresh lost its finite budget")
				}
				if phase == "save-failure" {
					return errors.New("injected durable-store failure")
				}
				if phase == "changed-owner" {
					if err := base.ReleaseRefreshLock(saveCtx, id, owner); err != nil {
						return err
					}
					if ok, err := base.TryRefreshLock(saveCtx, id, "successor-owner"); err != nil || !ok {
						return errors.New("could not replace lease owner")
					}
				}
				return base.SaveRefreshedSession(saveCtx, before, after, owner)
			}
			if phase == "changed-owner" {
				defer base.ReleaseRefreshLock(context.Background(), id, "successor-owner")
			}
			if phase == "provider-timeout" {
				service.client.Timeout = 100 * time.Millisecond
			}
			result := make(chan error, 1)
			go func() { _, err := service.refreshSession(ctx, session); result <- err }()
			waitRefreshSignal(t, entered)
			if phase != "during-save" {
				cancel()
			}
			switch phase {
			case "before-rotation":
				if rotated.Load() {
					t.Fatal("before-rotation fixture rotated too early")
				}
			case "logout":
				response := httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(response, logoutRequest(service, session))
				if response.Code != http.StatusNoContent {
					t.Fatalf("concurrent logout: %d", response.Code)
				}
			case "replacement":
				replacement := validSession(replacementID)
				replacement.LineageRoot = id
				if err := base.CommitLoginSession(context.Background(), id, replacement); err != nil {
					t.Fatal(err)
				}
			case "changed-session":
				changed := session
				changed.Nonce = "replacement-nonce"
				if err := base.SaveSession(context.Background(), changed); err != nil {
					t.Fatal(err)
				}

			}
			if phase != "provider-timeout" {
				unblock()
			}
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("refresh did not finish within its test dependency budget")
			}
			unblock()
			latest, readErr := base.GetSession(context.Background(), id)
			switch phase {
			case "logout", "replacement", "invalid-grant":
				if !errors.Is(err, ErrSessionNotFound) || !errors.Is(readErr, ErrSessionNotFound) {
					t.Fatalf("retired/rejected identity revived: refresh=%v read=%v", err, readErr)
				}
				if phase == "replacement" {
					if next, err := base.GetSession(context.Background(), replacementID); err != nil || next.ID != replacementID {
						t.Fatal("late refresh damaged replacement session")
					}
				}
			case "changed-session", "changed-owner":
				if !errors.Is(err, ErrRefreshConflict) || readErr != nil || latest.RefreshToken != session.RefreshToken {
					t.Fatalf("stale refresh overwrote newer facts: %v %v", err, readErr)
				}
				if phase == "changed-session" && latest.Nonce != "replacement-nonce" {
					t.Fatal("late refresh overwrote replacement nonce")
				}
				if phase == "changed-owner" {
					if ok, err := base.TryRefreshLock(context.Background(), id, "third-owner"); err != nil || ok {
						t.Fatal("late cleanup released the successor's lease")
					}
				}
			case "provider-timeout", "save-failure":
				if err == nil || errors.Is(err, ErrSessionNotFound) || readErr != nil || latest != session {
					t.Fatalf("temporary failure destroyed or falsely refreshed session: %v %v", err, readErr)
				}
			default:
				if err != nil || readErr != nil || latest.RefreshToken != "test-new-refresh" || latest.AccessToken != "test-new-access" {
					t.Fatalf("admitted rotation was lost after cancellation: refresh=%v read=%v", err, readErr)
				}
				if latest.ID != session.ID || latest.Subject != session.Subject || latest.Nonce != session.Nonce || latest.CSRFToken != session.CSRFToken || latest.CreatedAt != session.CreatedAt || latest.LineageRoot != session.LineageRoot {
					t.Fatal("refresh changed stable session identity")
				}
				if remaining := time.Until(latest.RefreshExpiresAt); remaining < 590*time.Second || remaining > 600*time.Second {
					t.Fatal("refresh lifetime was not taken from the IdP")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("refresh silently retried the IdP: %d calls", calls.Load())
			}
			if phase == "provider-timeout" && saveCalls.Load() != 0 {
				t.Fatal("timed-out provider response was persisted")
			}
		})
	}
}

func TestRefreshBrowserDisconnectPreservesRotatedCredential(t *testing.T) {
	runRefreshBrowserDisconnect(t, newMemoryStore())
}

func TestRealRedisRefreshBrowserDisconnectPreservesRotatedCredential(t *testing.T) {
	id := uuid.NewString()
	runRefreshBrowserDisconnectWithID(t, realRedisStore(t, id), id)
}

func runRefreshBrowserDisconnect(t *testing.T, store Store) {
	runRefreshBrowserDisconnectWithID(t, store, uuid.NewString())
}

func runRefreshBrowserDisconnectWithID(t *testing.T, store Store, id string) {
	t.Helper()
	var mu sync.Mutex
	current, rotations, rejected := "test-refresh", 0, 0
	rotated, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		if r.PostForm.Get("refresh_token") != current {
			rejected++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		rotations++
		current = fmt.Sprintf("test-refresh-%d", rotations)
		next, first := current, rotations == 1
		mu.Unlock()
		if first {
			close(rotated)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "test-new-access", RefreshToken: next, TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 600})
	}))
	session := validSession(id)
	session.AccessExpiresAt = time.Now().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	handler := service.Handler(nil)
	server := httptest.NewServer(handler)
	defer func() { unblock(); server.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/session", nil)
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: id})
	disconnected := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		disconnected <- err
	}()
	waitRefreshSignal(t, rotated)
	cancel()
	if err := <-disconnected; !errors.Is(err, context.Canceled) {
		t.Fatalf("browser did not actually disconnect: %v", err)
	}
	// Other requests arrive while the disconnected owner still holds its lease.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/session", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: id})
			response, err := server.Client().Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Errorf("concurrent session request lost identity: %d", response.StatusCode)
			}
		}()
	}
	unblock()
	wg.Wait()
	latest, err := store.GetSession(context.Background(), id)
	if err != nil || latest.RefreshToken != "test-refresh-1" {
		t.Fatalf("disconnected rotation not durably retained: %v", err)
	}
	mu.Lock()
	if rotations != 1 || rejected != 0 {
		t.Errorf("concurrent requests reused single-use predecessor: rotations=%d rejected=%d", rotations, rejected)
	}
	mu.Unlock()
	// Force the next ordinary access refresh; the protocol server accepts only
	// the previously saved successor, never its invalid single-use predecessor.
	latest.AccessExpiresAt = time.Now().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), latest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.refreshSession(context.Background(), latest); err != nil {
		t.Fatalf("next refresh could not use retained credential: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if rotations != 2 || rejected != 0 {
		t.Fatalf("next refresh reused predecessor: rotations=%d rejected=%d", rotations, rejected)
	}
}
