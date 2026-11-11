package portal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/redis/go-redis/v9"
)

// These scenarios use two independent runtime clients, the published production
// ACL and an owned Redis 8.2 server. The admin is used only to arrange/inspect
// fixture state and perform the explicit operations inventory.
func capacityScenario(t *testing.T, limits AdmissionLimits) (*RedisStore, *RedisStore, *redis.Client) {
	t.Helper()
	resetOwnedPortalNamespace(t)
	t.Cleanup(func() { resetOwnedPortalNamespace(t) })
	admin := ownedPortalRedisAdmin(t)
	t.Cleanup(func() { admin.Close() })
	ops, err := NewRedisStore(osFixtureURL())
	if err != nil {
		t.Fatal(err)
	}
	if err = ops.InventoryTransientCapacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops.Close()
	makeStore := func() *RedisStore {
		s, e := NewRedisStoreWithAdmissionLimits(checkedPortalFixture(t), "", limits)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		// Open sequentially so the local VM NAT backlog is not the concurrency subject.
		held := make([]*redis.Conn, 0, 16)
		for i := 0; i < 16; i++ {
			c := s.transientClient.Conn()
			if e := c.Ping(context.Background()).Err(); e != nil {
				t.Fatal(e)
			}
			held = append(held, c)
		}
		for _, c := range held {
			c.Close()
		}
		return s
	}
	return makeStore(), makeStore(), admin
}

func quotaExpected(t *testing.T, err error) {
	t.Helper()
	var quota *PortalQuotaError
	if !errors.As(err, &quota) || quota.RetryAfter <= 0 {
		t.Fatalf("expected bounded quota refusal, got %v", err)
	}
}

func TestRealRedisPortalSharedAdmissionAndCapacity(t *testing.T) {
	ctx := context.Background()
	t.Run("AllFourOperationsShareAtomicBurstAcrossClients", func(t *testing.T) {
		limits := AdmissionLimits{1, 8, 64, 64}
		a, b, admin := capacityScenario(t, limits)
		for _, operation := range []PortalOperation{LoginStartOperation, LoginCallbackOperation, HandoffCreateOperation, HandoffConsumeOperation} {
			var admitted, refused atomic.Int32
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					s := a
					if i%2 == 1 {
						s = b
					}
					e := s.AdmitOperation(ctx, operation)
					if e == nil {
						admitted.Add(1)
					} else {
						var q *PortalQuotaError
						if errors.As(e, &q) {
							refused.Add(1)
						} else {
							t.Error(e)
						}
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if admitted.Load() != 8 || refused.Load() != 24 {
				t.Fatalf("%s independent bursts leaked: admitted=%d refused=%d", operation, admitted.Load(), refused.Load())
			}
		}
		keys, e := admin.Keys(ctx, "portal:operation-admission:v1:*").Result()
		if e != nil || len(keys) != 4 {
			t.Fatalf("request-specific quota keys allocated: %d %v", len(keys), e)
		}
		// Prove refill without a wall-clock sleep, using the actual server's time.
		now, e := admin.Time(ctx).Result()
		if e != nil {
			t.Fatal(e)
		}
		if e = admin.HSet(ctx, "portal:operation-admission:v1:login-start", "at", now.Add(-2*time.Second).UnixMilli()).Err(); e != nil {
			t.Fatal(e)
		}
		if e = a.AdmitOperation(ctx, LoginStartOperation); e != nil {
			t.Fatal("next call did not recover", e)
		}
	})
	t.Run("PolicyMismatchAndCorruptionNeverGrantFreshBurst", func(t *testing.T) {
		a, _, admin := capacityScenario(t, AdmissionLimits{1, 2, 8, 8})
		if e := a.AdmitOperation(ctx, LoginCallbackOperation); e != nil {
			t.Fatal(e)
		}
		other, e := NewRedisStoreWithAdmissionLimits(checkedPortalFixture(t), "", AdmissionLimits{1, 3, 8, 8})
		if e != nil {
			t.Fatal(e)
		}
		defer other.Close()
		if e = other.AdmitOperation(ctx, LoginCallbackOperation); e == nil {
			t.Fatal("mixed replica policy admitted")
		}
		key := "portal:operation-admission:v1:login-callback"
		if e = admin.HDel(ctx, key, "fingerprint").Err(); e != nil {
			t.Fatal(e)
		}
		if e = a.AdmitOperation(ctx, LoginCallbackOperation); e == nil {
			t.Fatal("corrupt bucket granted a fresh burst")
		}
	})
	t.Run("FlowCapacityRetainsOriginalAndRecoversAfterSingleUse", func(t *testing.T) {
		a, b, admin := capacityScenario(t, AdmissionLimits{100, 200, 2, 2})
		flow := LoginFlow{Verifier: "original-proof", Nonce: "original-nonce", ReturnTo: "/", ExpiresAt: time.Now().Add(time.Minute)}
		if e := a.SaveFlow(ctx, "one", flow); e != nil {
			t.Fatal(e)
		}
		original, e := admin.Get(ctx, "portal:flow:one").Result()
		if e != nil {
			t.Fatal(e)
		}
		deadline, e := admin.PExpireTime(ctx, "portal:flow:one").Result()
		if e != nil {
			t.Fatal(e)
		}
		if e = b.SaveFlow(ctx, "two", flow); e != nil {
			t.Fatal(e)
		}
		quotaExpected(t, a.SaveFlow(ctx, "three", flow))
		if e = b.SaveFlow(ctx, "one", flow); e != nil {
			t.Fatal("identical replay failed", e)
		}
		changed := flow
		changed.Verifier = "different"
		if !errors.Is(a.SaveFlow(ctx, "one", changed), ErrTransientConflict) {
			t.Fatal("overwrite allowed")
		}
		after, _ := admin.Get(ctx, "portal:flow:one").Result()
		afterDeadline, _ := admin.PExpireTime(ctx, "portal:flow:one").Result()
		if after != original || afterDeadline != deadline {
			t.Fatal("quota/replay rewrote original or extended TTL")
		}
		got, e := b.TakeFlow(ctx, "one")
		if e != nil || got.Verifier != flow.Verifier {
			t.Fatal("original flow lost", e)
		}
		if _, e = a.TakeFlow(ctx, "one"); !errors.Is(e, ErrSessionNotFound) {
			t.Fatal("flow replayed", e)
		}
		if e = a.SaveFlow(ctx, "three", flow); e != nil {
			t.Fatal("released capacity did not recover", e)
		}
	})
	t.Run("ConcurrentCreatorsCannotOversubscribe", func(t *testing.T) {
		a, b, admin := capacityScenario(t, AdmissionLimits{100, 200, 4, 4})
		var accepted atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				s := a
				if i%2 == 1 {
					s = b
				}
				e := s.SaveFlow(ctx, fmt.Sprintf("flow-%d", i), LoginFlow{ExpiresAt: time.Now().Add(time.Minute)})
				if e == nil {
					accepted.Add(1)
				} else {
					var q *PortalQuotaError
					if !errors.As(e, &q) {
						t.Error(e)
					}
				}
			}(i)
		}
		close(start)
		wg.Wait()
		keys, e := admin.Keys(ctx, "portal:flow:*").Result()
		if e != nil || len(keys) != 4 || accepted.Load() != 4 {
			t.Fatalf("capacity exceeded %d/%d %v", accepted.Load(), len(keys), e)
		}
	})
	t.Run("HandoffReservationRetainsUnknownWriteAndExpires", func(t *testing.T) {
		a, b, admin := capacityScenario(t, AdmissionLimits{100, 200, 2, 1})
		if e := a.ReserveSessionHandoff(ctx, "accepted", time.Now().Add(time.Minute)); e != nil {
			t.Fatal(e)
		}
		quotaExpected(t, b.ReserveSessionHandoff(ctx, "other", time.Now().Add(time.Minute)))
		handoff := SessionHandoff{ExpiresAt: time.Now().Add(30 * time.Second)}
		if e := b.SaveSessionHandoff(ctx, "accepted", handoff); e != nil {
			t.Fatal("reserved fill refused at full capacity", e)
		}
		if e := a.ReleaseSessionHandoffReservation(ctx, "accepted"); e != nil {
			t.Fatal(e)
		}
		if _, e := admin.ZScore(ctx, transientIndex(handoffTransient), "accepted").Result(); e != nil {
			t.Fatal("unknown successful write was unaccounted", e)
		}
		quotaExpected(t, a.ReserveSessionHandoff(ctx, "other", time.Now().Add(time.Minute)))
		if _, e := a.TakeSessionHandoff(ctx, "accepted"); e != nil {
			t.Fatal(e)
		}
		if e := b.ReserveSessionHandoff(ctx, "abandoned", time.Now().Add(time.Minute)); e != nil {
			t.Fatal(e)
		}
		// Arrange elapsed reservation score, retaining actual Redis time authority.
		now, _ := admin.Time(ctx).Result()
		if e := admin.ZAdd(ctx, transientIndex(handoffTransient), redis.Z{Member: "abandoned", Score: float64(now.Add(-time.Second).UnixMilli())}).Err(); e != nil {
			t.Fatal(e)
		}
		if e := a.ReserveSessionHandoff(ctx, "next", time.Now().Add(time.Minute)); e != nil {
			t.Fatal("next reservation remained locked", e)
		}
		if e := a.ReleaseSessionHandoffReservation(ctx, "next"); e != nil {
			t.Fatal(e)
		}
		if e := b.ReserveSessionHandoff(ctx, "after-release", time.Now().Add(time.Minute)); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("LegacyInventoryPreservesNativeTTLAndAllowsConsumers", func(t *testing.T) {
		a, _, admin := capacityScenario(t, AdmissionLimits{100, 200, 1, 1})
		if e := admin.Del(ctx, transientInventory(flowTransient), transientInventory(handoffTransient)).Err(); e != nil {
			t.Fatal(e)
		}
		flow := LoginFlow{Verifier: "legacy", ExpiresAt: time.Now().Add(time.Minute)}
		body, _ := json.Marshal(flow)
		for _, id := range []string{"legacy-one", "legacy-two"} {
			if e := admin.Set(ctx, "portal:flow:"+id, body, time.Minute).Err(); e != nil {
				t.Fatal(e)
			}
		}
		deadline, _ := admin.PExpireTime(ctx, "portal:flow:legacy-two").Result()
		if e := a.SaveFlow(ctx, "new", flow); !errors.Is(e, ErrCapacityInventoryRequired) {
			t.Fatal("runtime implicitly scanned or admitted uninventoried data", e)
		}
		if _, _, e := a.transientClient.Scan(ctx, 0, "*", 1).Result(); e == nil {
			t.Fatal("runtime gained global enumeration")
		}
		if _, e := a.TakeFlow(ctx, "legacy-one"); e != nil {
			t.Fatal("inventory blocked original consumer", e)
		}
		ops, e := NewRedisStore(osFixtureURL())
		if e != nil {
			t.Fatal(e)
		}
		defer ops.Close()
		if e = ops.InventoryTransientCapacity(ctx); e != nil {
			t.Fatal(e)
		}
		after, _ := admin.PExpireTime(ctx, "portal:flow:legacy-two").Result()
		if after != deadline {
			t.Fatal("legacy TTL changed")
		}
		quotaExpected(t, a.SaveFlow(ctx, "new", flow))
		if _, e = a.TakeFlow(ctx, "legacy-two"); e != nil {
			t.Fatal(e)
		}
		if e = a.SaveFlow(ctx, "new", flow); e != nil {
			t.Fatal("post-inventory capacity did not recover", e)
		}
	})
	t.Run("PersistentLegacyObjectFailsInventoryWithoutDeletingEvidence", func(t *testing.T) {
		_, _, admin := capacityScenario(t, DefaultAdmissionLimits())
		admin.Del(ctx, transientInventory(flowTransient))
		if e := admin.Set(ctx, "portal:flow:legacy-persistent", "original", 0).Err(); e != nil {
			t.Fatal(e)
		}
		ops, e := NewRedisStore(osFixtureURL())
		if e != nil {
			t.Fatal(e)
		}
		defer ops.Close()
		if e = ops.InventoryTransientCapacity(ctx); e == nil {
			t.Fatal("persistent legacy object silently omitted")
		}
		value, e := admin.Get(ctx, "portal:flow:legacy-persistent").Result()
		if e != nil || value != "original" {
			t.Fatal("original evidence deleted", e)
		}
		if ready, _ := admin.HGet(ctx, transientInventory(flowTransient), "ready").Result(); ready == "1" {
			t.Fatal("failed inventory marked ready")
		}
	})
	t.Run("ACLFailurePrecedesOneUseAndCapacityMutations", func(t *testing.T) {
		a, _, admin := capacityScenario(t, DefaultAdmissionLimits())
		flow := LoginFlow{Verifier: "retain", ExpiresAt: time.Now().Add(time.Minute)}
		if e := a.SaveFlow(ctx, "accepted", flow); e != nil {
			t.Fatal(e)
		}
		if e := admin.Do(ctx, "ACL", "SETUSER", "portal", "-zrem").Err(); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if e := admin.Do(ctx, "ACL", "SETUSER", "portal", "+zrem").Err(); e != nil {
				t.Error(e)
			}
		}()
		if _, e := a.TakeFlow(ctx, "accepted"); e == nil {
			t.Fatal("consumer bypassed required ACL")
		}
		if e := a.SaveFlow(ctx, "new", flow); e == nil {
			t.Fatal("save bypassed required ACL")
		}
		if exists, _ := admin.Exists(ctx, "portal:flow:accepted").Result(); exists != 1 {
			t.Fatal("ACL failure consumed original")
		}
		if exists, _ := admin.Exists(ctx, "portal:flow:new").Result(); exists != 0 {
			t.Fatal("ACL failure partially wrote new object")
		}
	})

	t.Run("PublicRoutersRejectBeforeConsumingOriginalObjects", func(t *testing.T) {
		a, b, admin := capacityScenario(t, AdmissionLimits{1, 2, 8, 1})
		first := testService(t, a)
		second := testService(t, b)
		for i, path := range []string{"/auth/login", "/", "/auth/login?force_login=1"} {
			service := first
			if i%2 == 1 {
				service = second
			}
			w := httptest.NewRecorder()
			service.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			expected := 302
			if i == 2 {
				expected = 429
			}
			if w.Code != expected {
				t.Fatalf("public login %s returned %d", path, w.Code)
			}
		}
		if e := a.SaveFlow(ctx, "callback-original", LoginFlow{Verifier: "retained", ExpiresAt: time.Now().Add(time.Minute)}); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 2; i++ {
			if e := a.AdmitOperation(ctx, LoginCallbackOperation); e != nil {
				t.Fatal(e)
			}
		}
		r := httptest.NewRequest("GET", "/auth/callback?state=callback-original&code=controlled", nil)
		r.AddCookie(&http.Cookie{Name: second.flowCookieName(), Value: "callback-original"})
		w := httptest.NewRecorder()
		second.Handler(nil).ServeHTTP(w, r)
		if w.Code != 429 {
			t.Fatal("callback quota not enforced", w.Code)
		}
		if exists, _ := admin.Exists(ctx, "portal:flow:callback-original").Result(); exists != 1 {
			t.Fatal("callback quota consumed original")
		}
		handoffService, _ := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
		handoffService.store = b
		ticket := "real-redis-original-handoff-ticket"
		digest := sha256.Sum256([]byte(ticket))
		id := hex.EncodeToString(digest[:])
		if e := a.SaveSessionHandoff(ctx, id, SessionHandoff{Session: validSession("new-browser"), ExpiresAt: time.Now().Add(30 * time.Second)}); e != nil {
			t.Fatal(e)
		}
		calls := 0
		handoffService.client.Transport = admissionRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not call provider") })
		create := httptest.NewRequest("POST", "/portal-session-handoff", nil)
		create.Header.Set("Authorization", "Bearer controlled")
		w = httptest.NewRecorder()
		handoffService.Handler(nil).ServeHTTP(w, create)
		if w.Code != 429 || calls != 0 {
			t.Fatal("capacity checked after provider calls", w.Code, calls)
		}
		for i := 0; i < 2; i++ {
			if e := a.AdmitOperation(ctx, HandoffConsumeOperation); e != nil {
				t.Fatal(e)
			}
		}
		request := func() *http.Request {
			r := httptest.NewRequest("POST", "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+ticket+`"}`))
			r.Header.Set("Origin", handoffService.config.PublicURL.String())
			r.Header.Set("Content-Type", "application/json")
			return r
		}
		w = httptest.NewRecorder()
		handoffService.Handler(nil).ServeHTTP(w, request())
		if w.Code != 429 {
			t.Fatal("handoff quota not enforced", w.Code)
		}
		if exists, _ := admin.Exists(ctx, "portal:session-handoff:"+id).Result(); exists != 1 {
			t.Fatal("quota consumed original handoff")
		}
		if e := admin.Del(ctx, "portal:operation-admission:v1:handoff-consume").Err(); e != nil {
			t.Fatal(e)
		}
		for _, expected := range []int{200, 401} {
			w = httptest.NewRecorder()
			handoffService.Handler(nil).ServeHTTP(w, request())
			if w.Code != expected {
				t.Fatal("next admitted attempt or replay incorrect", w.Code, w.Body.String())
			}
		}
	})
	t.Run("UnknownSaveAndConsumptionRepliesAreNotAutomaticallyRetried", func(t *testing.T) {
		a, _, admin := capacityScenario(t, DefaultAdmissionLimits())
		options := *a.transientClient.Options()
		// go-redis normalizes disabled retries to zero in Options(); a fresh
		// NewClient needs the explicit -1 input again.
		if options.MaxRetries == 0 {
			options.MaxRetries = -1
		}
		a.transientClient.Close()
		var target atomic.Value
		target.Store("")
		var dropped atomic.Int32
		options.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, e := (&net.Dialer{}).DialContext(ctx, network, addr)
			if e != nil {
				return nil, e
			}
			return &dropPortalReplyConn{Conn: c, target: &target, dropped: &dropped}, nil
		}
		a.transientClient = redis.NewClient(&options)
		if e := a.transientClient.Ping(ctx).Err(); e != nil {
			t.Fatal(e)
		}
		target.Store("PORTAL_TRANSIENT_WRITE_FAILED")
		flow := LoginFlow{Verifier: "unknown-save-original", ExpiresAt: time.Now().Add(time.Minute)}
		if e := a.SaveFlow(ctx, "unknown", flow); e == nil {
			t.Fatal("lost successful write reply was transparently retried")
		}
		if dropped.Load() != 1 {
			t.Fatalf("expected one discarded SET script response, got %d", dropped.Load())
		}
		if exists, _ := admin.Exists(ctx, "portal:flow:unknown").Result(); exists != 1 {
			t.Fatal("successful unknown write lost")
		}
		if _, e := admin.ZScore(ctx, transientIndex(flowTransient), "unknown").Result(); e != nil {
			t.Fatal("unknown write not accounted", e)
		}
		target.Store("local value = redis.call('GETDEL'")
		_, e := a.TakeFlow(ctx, "unknown")
		if e == nil || errors.Is(e, ErrSessionNotFound) {
			t.Fatal("lost successful consumption reply misreported as missing", e)
		}
		if dropped.Load() != 2 {
			t.Fatal("did not drop actual GETDEL response")
		}
		target.Store("")
		if _, e = a.TakeFlow(ctx, "unknown"); !errors.Is(e, ErrSessionNotFound) {
			t.Fatal("following explicit attempt should see single-use consumption", e)
		}
		if e = a.SaveFlow(ctx, "after-unknown", flow); e != nil {
			t.Fatal("client did not recover", e)
		}
	})
	t.Run("BackgroundInventoryProgressesAndStops", func(t *testing.T) {
		_, _, admin := capacityScenario(t, DefaultAdmissionLimits())
		if e := admin.Del(ctx, transientInventory(flowTransient)).Err(); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 300; i++ {
			if e := admin.Set(ctx, fmt.Sprintf("portal:flow:legacy-%d", i), "original", time.Minute).Err(); e != nil {
				t.Fatal(e)
			}
		}
		ops, e := NewRedisStore(osFixtureURL())
		if e != nil {
			t.Fatal(e)
		}
		defer ops.Close()
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); ops.RunTransientInventory(workerCtx) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			state, e := ops.InspectTransientCapacity(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if state.FlowReady {
				if state.FlowActiveSlots != 300 {
					t.Fatal("incomplete background inventory", state)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("background inventory never progressed")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("worker did not drain")
		}
	})

	t.Run("PersonalProductionACLServesAllFourAdmissionAndCapacityOperations", func(t *testing.T) {
		a, b, admin := capacityScenario(t, DefaultAdmissionLimits())
		installOwnedPortalFixtureACL(t, admin, "redis-personal.acl.template")
		defer installOwnedPortalFixtureACL(t, admin, "redis-accounts.acl.template")
		for _, operation := range []PortalOperation{LoginStartOperation, LoginCallbackOperation, HandoffCreateOperation, HandoffConsumeOperation} {
			if e := a.AdmitOperation(ctx, operation); e != nil {
				t.Fatal("published personal actor cannot admit operation", operation, e)
			}
		}
		flow := LoginFlow{Verifier: "personal-original", ExpiresAt: time.Now().Add(time.Minute)}
		if e := a.SaveFlow(ctx, "personal-flow", flow); e != nil {
			t.Fatal(e)
		}
		got, e := b.TakeFlow(ctx, "personal-flow")
		if e != nil || got.Verifier != flow.Verifier {
			t.Fatal("personal flow consumption failed", e)
		}
		if e = a.ReserveSessionHandoff(ctx, "personal-handoff", time.Now().Add(time.Minute)); e != nil {
			t.Fatal(e)
		}
		if e = b.SaveSessionHandoff(ctx, "personal-handoff", SessionHandoff{Session: validSession("personal-browser"), ExpiresAt: time.Now().Add(30 * time.Second)}); e != nil {
			t.Fatal(e)
		}
		if e = a.ReleaseSessionHandoffReservation(ctx, "personal-handoff"); e != nil {
			t.Fatal(e)
		}
		if _, e = a.TakeSessionHandoff(ctx, "personal-handoff"); e != nil {
			t.Fatal(e)
		}
		if _, _, e = a.transientClient.Scan(ctx, 0, "*", 1).Result(); e == nil {
			t.Fatal("personal actor gained enumeration")
		}
		service := testService(t, a)
		w := httptest.NewRecorder()
		service.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 302 {
			t.Fatal("personal public root login failed", w.Code)
		}
	})
	t.Run("WrongTypeLegacyObjectsFailInventoryWithoutMutation", func(t *testing.T) {
		_, _, admin := capacityScenario(t, DefaultAdmissionLimits())
		ops, e := NewRedisStore(osFixtureURL())
		if e != nil {
			t.Fatal(e)
		}
		defer ops.Close()
		for _, kind := range []transientKind{flowTransient, handoffTransient} {
			for _, shape := range []string{"list", "hash"} {
				key := transientPrefix(kind) + "wrong-type"
				if e = admin.Del(ctx, transientInventory(kind)).Err(); e != nil {
					t.Fatal(e)
				}
				if shape == "list" {
					e = admin.LPush(ctx, key, "original").Err()
				} else {
					e = admin.HSet(ctx, key, "original", "evidence").Err()
				}
				if e != nil {
					t.Fatal(e)
				}
				if e = admin.PExpire(ctx, key, time.Minute).Err(); e != nil {
					t.Fatal(e)
				}
				deadline, e := admin.PExpireTime(ctx, key).Result()
				if e != nil {
					t.Fatal(e)
				}
				if e = ops.InventoryTransientCapacity(ctx); e == nil {
					t.Fatal("wrong-type original marked ready", kind, shape)
				}
				actual, e := admin.Type(ctx, key).Result()
				if e != nil || actual != shape {
					t.Fatal("original type mutated", e)
				}
				after, e := admin.PExpireTime(ctx, key).Result()
				if e != nil || after != deadline {
					t.Fatal("original TTL mutated", e)
				}
				if ready, _ := admin.HGet(ctx, transientInventory(kind), "ready").Result(); ready == "1" {
					t.Fatal("wrong type falsely marked ready")
				}
				if e = admin.Del(ctx, key).Err(); e != nil {
					t.Fatal(e)
				}
			}
		}
	})

	t.Run("PhysicalOOMPreservesAcceptedObjectsAndDeletionRecovery", func(t *testing.T) {
		a, _, admin := capacityScenario(t, DefaultAdmissionLimits())
		flow := LoginFlow{Verifier: "keep-through-oom", ExpiresAt: time.Now().Add(time.Minute)}
		if e := a.SaveFlow(ctx, "accepted", flow); e != nil {
			t.Fatal(e)
		}
		original, e := admin.ConfigGet(ctx, "maxmemory").Result()
		if e != nil {
			t.Fatal(e)
		}
		if e = admin.ConfigSet(ctx, "maxmemory", "1").Err(); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if e := admin.ConfigSet(ctx, "maxmemory", original["maxmemory"]).Err(); e != nil {
				t.Error(e)
			}
		}()
		if e = a.SaveFlow(ctx, "new", flow); e == nil || !strings.Contains(e.Error(), "OOM") {
			t.Fatal("physical OOM was not enforced", e)
		}
		if e = a.AdmitOperation(ctx, LoginCallbackOperation); e != nil {
			t.Fatal("bounded consumer admission failed during OOM", e)
		}
		got, e := a.TakeFlow(ctx, "accepted")
		if e != nil || got.Verifier != flow.Verifier {
			t.Fatal("accepted flow could not release memory", e)
		}
		if exists, _ := admin.Exists(ctx, "portal:flow:new").Result(); exists != 0 {
			t.Fatal("OOM write appeared")
		}
		if e = admin.ConfigSet(ctx, "maxmemory", original["maxmemory"]).Err(); e != nil {
			t.Fatal(e)
		}
		if e = a.SaveFlow(ctx, "next", flow); e != nil {
			t.Fatal("next call did not recover after memory restoration", e)
		}
	})
}

func osFixtureURL() string { return os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL") }

// The command is forwarded normally and its real server reply is read, then
// discarded once. This exercises unknown outcomes rather than pre-write errors.
type dropPortalReplyConn struct {
	net.Conn
	target  *atomic.Value
	dropped *atomic.Int32
	armed   bool
}

func (c *dropPortalReplyConn) Write(p []byte) (int, error) {
	v := c.target.Load().(string)
	if v != "" && bytes.Contains(p, []byte(v)) {
		c.armed = true
	}
	return c.Conn.Write(p)
}
func (c *dropPortalReplyConn) Read(p []byte) (int, error) {
	n, e := c.Conn.Read(p)
	if n > 0 && c.armed {
		c.armed = false
		c.dropped.Add(1)
		c.Conn.Close()
		return 0, io.ErrUnexpectedEOF
	}
	return n, e
}
