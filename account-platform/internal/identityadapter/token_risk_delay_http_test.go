package identityadapter

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/redis/go-redis/v9"
)

// Command storage only is controlled: actual RedisTokenAdmission, HTTP public
// router, metrics/recovery wrappers and shared30s server execute unchanged.
// This does not execute Redis/Lua or a real Keycloak user store.
type identifierRiskStore struct {
	mu                  sync.Mutex
	values              map[string]string
	scriptRetry         int64
	scriptCalls         int
	expires             map[string]time.Time
	clock               time.Time
	denyCommand         string
	unknownCounterReply bool
}

func (s *identifierRiskStore) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("test must not dial Redis")
	}
}
func (s *identifierRiskStore) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, c redis.Cmder) error {
		if err := ctx.Err(); err != nil {
			c.SetErr(err)
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.expires == nil {
			s.expires = make(map[string]time.Time)
		}
		a := c.Args()
		key := ""
		if len(a) > 1 {
			key, _ = a[1].(string)
		}
		if c.Name() == s.denyCommand {
			err := errors.New("controlled Redis command denial")
			c.SetErr(err)
			return err
		}
		s.expireValue(key)
		switch c.Name() {
		case "evalsha", "eval":
			script, _ := a[1].(string)
			switch script {
			case admissionScript.Hash(), admissionScriptSource:
				s.scriptCalls++
				c.(*redis.Cmd).SetVal(s.scriptRetry)
			case failureCounterScript.Hash(), failureCounterLua:
				return s.failureCounterCommand(c)
			default:
				err := errors.New("unrecognized Redis test script")
				c.SetErr(err)
				return err
			}
		case "get":
			v, ok := s.values[key]
			if !ok {
				c.SetErr(redis.Nil)
				return redis.Nil
			}
			c.(*redis.StringCmd).SetVal(v)
		case "incr":
			var n int64
			_, _ = fmt.Sscan(s.values[key], &n)
			n++
			s.values[key] = fmt.Sprint(n)
			c.(*redis.IntCmd).SetVal(n)
		case "expire":
			s.expires[key] = s.clock.Add(time.Duration(a[2].(int64)) * time.Second)
			c.(*redis.BoolCmd).SetVal(true)
		case "ttl":
			c.(*redis.DurationCmd).SetVal(time.Duration(s.valueTTL(key)) * time.Second)
		case "set":
			s.values[key] = fmt.Sprint(a[2])
			delete(s.expires, key)
			c.(*redis.StatusCmd).SetVal("OK")
		default:
			return errors.New("unexpected Redis test command " + c.Name())
		}
		return nil
	}
}

// This controlled store models script results, not a Redis/Lua interpreter.
// Script identity is exact: unknown scripts fail rather than returning a fake
// successful admission. These tests exercise the Go producer/consumer and
// failures; Lua control flow and real Redis ACLs need separate execution.
func (s *identifierRiskStore) failureCounterCommand(c redis.Cmder) error {
	a := c.Args()
	if len(a) != 6 || a[2] != 1 {
		return errors.New("invalid failure-counter script invocation")
	}
	key, ok := a[3].(string)
	if !ok {
		return errors.New("invalid failure-counter key")
	}
	increment := a[4] == "1"
	window, ok := a[5].(int64)
	if !ok || window != 900 {
		return errors.New("invalid failure-counter window")
	}
	s.expireValue(key)
	value, exists := s.values[key]
	ttl := int64(0)
	if exists {
		ttl = s.valueTTL(key)
	}
	if exists && ttl < 0 || !exists && increment {
		if s.denyCommand == "set" {
			err := errors.New("controlled Redis SET denial")
			c.SetErr(err)
			return err
		}
		if !exists {
			value = "1"
		}
		s.values[key] = value
		s.expires[key] = s.clock.Add(time.Duration(window) * time.Second)
		ttl = window
	}
	if exists && increment {
		if s.denyCommand == "incr" {
			err := errors.New("controlled Redis INCR denial")
			c.SetErr(err)
			return err
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		value = strconv.FormatInt(n+1, 10)
		s.values[key] = value
	}
	if !exists && !increment {
		value = "0"
	}
	if increment && s.unknownCounterReply {
		s.unknownCounterReply = false
		err := errors.New("controlled lost counter response")
		c.SetErr(err)
		return err
	}
	c.(*redis.Cmd).SetVal([]any{value, ttl})
	return nil
}

func (s *identifierRiskStore) expireValue(key string) {
	if expires, ok := s.expires[key]; ok && !expires.After(s.clock) {
		delete(s.values, key)
		delete(s.expires, key)
	}
}

func (s *identifierRiskStore) valueTTL(key string) int64 {
	if _, ok := s.values[key]; !ok {
		return -2
	}
	expires, ok := s.expires[key]
	if !ok {
		return -1
	}
	return int64(expires.Sub(s.clock) / time.Second)
}
func (s *identifierRiskStore) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type identifierUnusedDirectory struct{ UserDirectory }

func identifierRiskHTTP(t *testing.T, failures int, status int, upstreamPause time.Duration) (*RedisTokenAdmission, *identifierRiskStore, *httptest.Server, *atomic.Int32) {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	store := &identifierRiskStore{values: map[string]string{}}
	a, err := NewRedisTokenAdmission("redis://127.0.0.1:1/0", base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("r", 32))))
	if err != nil {
		t.Fatal(err)
	}
	a.client.AddHook(store)
	t.Cleanup(func() { _ = a.client.Close() })
	for n := 0; n < failures; n++ {
		if _, err = a.Complete(context.Background(), tokenRiskDimensions{action: "password", identifier: "publicvictim"}, false, ""); err != nil {
			t.Fatal(err)
		}
	}
	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if upstreamPause > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(upstreamPause):
			}
		}
		w.WriteHeader(status)
		if status == 200 {
			_, _ = w.Write([]byte(`{"access_token":"fixture.eyJzdWIiOiJwdWJsaWMtdmljdGltIn0.signature"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}
	}))
	t.Cleanup(upstream.Close)
	proxy, err := NewTokenProxy(upstream.URL, a)
	if err != nil {
		t.Fatal(err)
	}
	proxy.floor = 0
	vault, err := NewPasswordVault([]byte(strings.Repeat("v", 32)), &identifierUnusedDirectory{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(vault, "public-fixture-token", proxy)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Environment: "test", ServiceName: "identity-service"}
	handler := httpserver.HandlerWithApplication(cfg, service.Handler(), nil)
	server := httptest.NewUnstartedServer(handler)
	server.Config = httpserver.NewApplicationServer(cfg, handler)
	server.Start()
	t.Cleanup(server.Close)
	return a, store, server, calls
}
func identifierRequest(t *testing.T, ctx context.Context, server *httptest.Server) (*http.Response, error) {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/realms/pinvou/protocol/openid-connect/token", strings.NewReader("grant_type=password&client_id=pinvou-desktop&username=PublicVictim&password=public-synthetic-correct"))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Device-ID", "public-victim-device")
	return (&http.Client{Timeout: 60 * time.Second}).Do(r)
}

func TestIdentifierRiskAllowsCorrectSixthCredentialAfterWait(t *testing.T) {
	a, store, server, calls := identifierRiskHTTP(t, 5, 200, 0)
	started := time.Now()
	response, err := identifierRequest(t, context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("correct sixth credential must be verified, got HTTP%d upstream=%d", response.StatusCode, calls.Load())
	}
	if time.Since(started) < 900*time.Millisecond {
		t.Fatal("required1s pre-verification wait omitted")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.values["pinvou:auth-failure:password:"+a.digest("publicvictim")] != "5" || store.scriptCalls != 1 {
		t.Fatal("success reset failures or admission double-counted")
	}
}
func TestIdentifierRiskWaitDoesNotAddFailedResponseDelay(t *testing.T) {
	_, _, server, calls := identifierRiskHTTP(t, 5, 401, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	response, err := identifierRequest(t, ctx, server)
	if err != nil {
		t.Fatal("pre-wait must not add old30s failed-response floor", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 || calls.Load() != 1 {
		t.Fatal(response.StatusCode, calls.Load())
	}
}
func TestIdentifierRiskCancellationDuringWaitDoesNotDispatch(t *testing.T) {
	a, store, server, calls := identifierRiskHTTP(t, 5, 200, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	response, err := identifierRequest(t, ctx, server)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("cancelled wait returned result")
	}
	time.Sleep(50 * time.Millisecond)
	store.mu.Lock()
	defer store.mu.Unlock()
	if calls.Load() != 0 || store.values["pinvou:auth-failure:password:"+a.digest("publicvictim")] != "5" {
		t.Fatal("cancel dispatched or counted credential failure")
	}
}
func TestIdentifierRiskStableLockActivatingDuringWaitStopsDispatch(t *testing.T) {
	a, store, server, calls := identifierRiskHTTP(t, 5, 200, 0)
	go func() {
		time.Sleep(100 * time.Millisecond)
		store.mu.Lock()
		store.values["pinvou:auth-alias:"+a.digest("publicvictim")] = "public-stable-digest"
		store.values["pinvou:auth-failure:user:public-stable-digest"] = "10"
		store.mu.Unlock()
	}()
	response, err := identifierRequest(t, context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 429 || calls.Load() != 0 {
		t.Fatal("stable ten-failure lock bypassed after wait", response.StatusCode, calls.Load())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.scriptCalls != 1 {
		t.Fatal("second quota admission")
	}
}
func TestIdentifierRiskThirtySecondWaitSurvivesSharedServer(t *testing.T) {
	_, _, server, calls := identifierRiskHTTP(t, 10, 200, 1100*time.Millisecond)
	started := time.Now()
	response, err := identifierRequest(t, context.Background(), server)
	if err != nil {
		t.Fatal("30s processing plus risk wait budget cut off", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 1 || time.Since(started) < 31*time.Second {
		t.Fatal("full30s wait or real shared route missing", response.StatusCode, calls.Load(), time.Since(started))
	}
}
