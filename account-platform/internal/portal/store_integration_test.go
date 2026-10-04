package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func realRedisStore(t *testing.T, identifiers ...string) *RedisStore {
	t.Helper()
	dsn := os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("PINVOU_PORTAL_TEST_REDIS_URL is required for real Redis tests")
	}
	store, err := NewRedisStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Only keys whose random identifiers this test created are removed.
		for _, id := range identifiers {
			if err := store.client.Del(ctx, "portal:flow:"+id, "portal:session:"+id, "portal:refresh:"+id).Err(); err != nil {
				t.Error("owned Redis test-key cleanup failed")
			}
		}
	})
	return store
}

func TestRealRedisFlowConsumptionAndSessionTTL(t *testing.T) {
	ctx := context.Background()
	id := uuid.NewString()
	store := realRedisStore(t, id)
	flow := LoginFlow{Verifier: "test-verifier", Nonce: "test-nonce", ReturnTo: "/", ExpiresAt: time.Now().UTC().Add(time.Minute)}
	if err := store.SaveFlow(ctx, id, flow); err != nil {
		t.Fatal(err)
	}
	var taken atomic.Int32
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := store.TakeFlow(ctx, id)
			if err == nil {
				taken.Add(1)
				if result.Verifier != flow.Verifier || result.Nonce != flow.Nonce {
					t.Error("flow facts changed")
				}
			} else if !errors.Is(err, ErrSessionNotFound) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if taken.Load() != 1 {
		t.Fatalf("flow consumed %d times", taken.Load())
	}
	flow.ExpiresAt = time.Now().Add(-time.Second)
	if !errors.Is(store.SaveFlow(ctx, id, flow), ErrSessionNotFound) {
		t.Fatal("expired flow was saved")
	}
	session := validSession(id)
	session.RefreshExpiresAt = time.Now().UTC().Add(150 * time.Millisecond)
	if err := store.SaveSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	ttl, err := store.client.PTTL(ctx, "portal:session:"+id).Result()
	if err != nil || ttl <= 0 || ttl > 150*time.Millisecond {
		t.Fatalf("invalid session TTL %s %v", ttl, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err = store.GetSession(ctx, id)
		if errors.Is(err, ErrSessionNotFound) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("expired session survived %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRealRedisRefreshLeaseHandoffFencesStaleOwnerAndLogout(t *testing.T) {
	ctx := context.Background()
	id := uuid.NewString()
	store := realRedisStore(t, id)
	previous := validSession(id)
	if err := store.SaveSession(ctx, previous); err != nil {
		t.Fatal(err)
	}
	if locked, err := store.TryRefreshLock(ctx, id, "old-owner"); err != nil || !locked {
		t.Fatalf("first lock %t %v", locked, err)
	}
	if locked, err := store.TryRefreshLock(ctx, id, "new-owner"); err != nil || locked {
		t.Fatalf("concurrent lock %t %v", locked, err)
	}
	// Accelerate the actual Redis lease expiry in this isolated fault fixture.
	if err := store.client.PExpire(ctx, "portal:refresh:"+id, 20*time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		locked, err := store.TryRefreshLock(ctx, id, "new-owner")
		if err != nil {
			t.Fatal(err)
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired lease did not hand off")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := store.ReleaseRefreshLock(ctx, id, "old-owner"); err != nil {
		t.Fatal(err)
	}
	owner, err := store.client.Get(ctx, "portal:refresh:"+id).Result()
	if err != nil || owner != "new-owner" {
		t.Fatalf("stale release deleted current lock %q %v", owner, err)
	}
	updated := previous
	updated.AccessToken, updated.RefreshToken = "test-new-access", "test-new-refresh"
	if !errors.Is(store.SaveRefreshedSession(ctx, previous, updated, "old-owner"), ErrRefreshConflict) {
		t.Fatal("stale owner overwrote session")
	}
	if err := store.DeleteRefreshedSession(ctx, previous, "old-owner"); !errors.Is(err, ErrRefreshConflict) {
		t.Fatalf("stale failure classification: %v", err)
	}
	if err := store.SaveRefreshedSession(ctx, previous, updated, "new-owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRefreshedSession(ctx, previous, "new-owner"); !errors.Is(err, ErrRefreshConflict) {
		t.Fatalf("old facts failure classification: %v", err)
	}
	actual, err := store.GetSession(ctx, id)
	if err != nil || actual.AccessToken != updated.AccessToken {
		t.Fatal("stale failure deleted newer token facts")
	}
	if err := store.DeleteSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(store.SaveRefreshedSession(ctx, updated, previous, "new-owner"), ErrSessionNotFound) {
		t.Fatal("refresh resurrected logged-out session")
	}
}

func TestRealRedisLogoutDuringHTTPRefreshCannotResurrectSession(t *testing.T) {
	for _, providerFails := range []bool{false, true} {
		name := "exchange-succeeds"
		if providerFails {
			name = "exchange-fails"
		}
		t.Run(name, func(t *testing.T) {
			id := uuid.NewString()
			store := realRedisStore(t, id)
			started, finish := make(chan struct{}), make(chan struct{})
			var once sync.Once
			service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-finish
				if providerFails {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(TokenResponse{AccessToken: "test-new-access", RefreshToken: "test-new-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 3600})
			}))
			release := func() { once.Do(func() { close(finish) }) }
			t.Cleanup(release)
			session := validSession(id)
			session.AccessExpiresAt = time.Now().UTC().Add(-time.Minute)
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: id})
			finished := make(chan int, 1)
			go func() {
				response := httptest.NewRecorder()
				service.Handler(nil).ServeHTTP(response, request)
				finished <- response.Code
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("refresh did not start")
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, logoutRequest(service, session))
			if response.Code != http.StatusNoContent {
				t.Fatalf("logout during refresh failed %d %s", response.Code, response.Body.String())
			}
			release()
			select {
			case code := <-finished:
				if code != http.StatusUnauthorized {
					t.Fatalf("logged-out refresh accepted: %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("refresh did not finish")
			}
			if _, err := store.GetSession(context.Background(), id); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("logged-out session survived %v", err)
			}
		})
	}
}

func TestRealRedisConcurrentSessionHTTPRequestsRefreshExactlyOnce(t *testing.T) {
	id := uuid.NewString()
	store := realRedisStore(t, id)
	var calls atomic.Int32
	service, _ := oidcTestService(t, store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		r.ParseForm()
		if r.PostForm.Get("refresh_token") != "test-refresh" {
			t.Error("wrong original refresh fact")
		}
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(TokenResponse{AccessToken: "test-new-access", RefreshToken: "test-new-refresh", TokenType: "Bearer", ExpiresIn: 60, RefreshExpiresIn: 3600})
	}))
	session := validSession(id)
	session.AccessExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler(nil))
	defer server.Close()
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, err := http.NewRequest(http.MethodGet, server.URL+"/api/session", nil)
			if err != nil {
				t.Error(err)
				return
			}
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: id})
			response, err := server.Client().Do(request)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			var result struct {
				Authenticated bool   `json:"authenticated"`
				CSRFToken     string `json:"csrf_token"`
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil || response.StatusCode != http.StatusOK || !result.Authenticated || result.CSRFToken != strings.Repeat("c", 32) {
				t.Errorf("session response status %d %+v %v", response.StatusCode, result, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh rotation stampede: %d", calls.Load())
	}
	latest, err := store.GetSession(context.Background(), id)
	if err != nil || latest.RefreshToken != "test-new-refresh" {
		t.Fatalf("refresh not durably rotated %+v %v", latest, err)
	}
}

func TestRealRedisTransportInterruptionReturns503WithoutLosingSession(t *testing.T) {
	id := uuid.NewString()
	base := realRedisStore(t, id)
	session := validSession(id)
	if err := base.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	options, err := redis.ParseURL(os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	destination := options.Addr
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var stopped bool
	var connections []net.Conn
	var acceptWG, pumpWG sync.WaitGroup
	acceptWG.Add(1)
	go func() {
		defer acceptWG.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			upstream, err := net.DialTimeout("tcp", destination, 200*time.Millisecond)
			if err != nil {
				client.Close()
				continue
			}
			mu.Lock()
			if stopped {
				mu.Unlock()
				client.Close()
				upstream.Close()
				return
			}
			connections = append(connections, client, upstream)
			mu.Unlock()
			pumpWG.Add(2)
			go func() { defer pumpWG.Done(); io.Copy(upstream, client); upstream.Close() }()
			go func() { defer pumpWG.Done(); io.Copy(client, upstream); client.Close() }()
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			listener.Close()
			mu.Lock()
			stopped = true
			for _, connection := range connections {
				connection.Close()
			}
			mu.Unlock()
			acceptWG.Wait()
			pumpWG.Wait()
		})
	}
	t.Cleanup(stop)
	options.Addr = listener.Addr().String()
	options.MaxRetries = -1
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	options.ContextTimeoutEnabled = true
	proxyStore := &RedisStore{client: redis.NewClient(options)}
	t.Cleanup(func() { proxyStore.Close() })
	if _, err := proxyStore.GetSession(context.Background(), id); err != nil {
		t.Fatalf("real Redis proxy did not first serve the session: %v", err)
	}
	service := testService(t, proxyStore)
	server := httptest.NewServer(service.Handler(nil))
	defer server.Close()
	stop() // Interrupt this test's TCP connections, never the shared Redis server.
	for _, route := range []string{"/api/session", "/api/logout", "/api/token/v1/me/balance", "/api/account/v1/me"} {
		method := http.MethodGet
		if route == "/api/logout" {
			method = http.MethodPost
		}
		request, err := http.NewRequest(method, server.URL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: id})
		request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
		request.Header.Set("Origin", "https://portal.example")
		request.Header.Set("X-CSRF-Token", session.CSRFToken)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Set-Cookie") != "" {
			t.Fatalf("Redis transport interruption reported false expiry/CSRF failure: %s %d", route, response.StatusCode)
		}
	}
	callback, err := http.NewRequest(http.MethodGet, server.URL+"/auth/callback?state="+id+"&code=test-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	callback.AddCookie(&http.Cookie{Name: service.flowCookieName(), Value: id})
	callbackResponse, err := server.Client().Do(callback)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, callbackResponse.Body)
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != http.StatusServiceUnavailable || callbackResponse.Header.Get("Set-Cookie") != "" {
		t.Fatalf("interrupted Redis callback masqueraded as expired state: %d", callbackResponse.StatusCode)
	}
	latest, err := base.GetSession(context.Background(), id)
	if err != nil || latest.AccessToken != session.AccessToken {
		t.Fatal("transport failure destroyed real Redis session")
	}
}
