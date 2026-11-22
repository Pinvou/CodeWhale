package identityadapter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const quotaSource = "192.0.2.1"
const quotaDevice = "quota-device"

func quotaHTTP(t *testing.T, a *RedisTokenAdmission) (func() *httptest.ResponseRecorder, *atomic.Int32) {
	request, calls := quotaHTTPRequests(t, a)
	return func() *httptest.ResponseRecorder { return request(quotaDevice) }, calls
}

func quotaHTTPRequests(t *testing.T, a *RedisTokenAdmission) (func(string) *httptest.ResponseRecorder, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"fixture.eyJzdWIiOiJxdW90YS1maXh0dXJlIn0.signature"}`))
	}))
	t.Cleanup(upstream.Close)
	proxy, err := NewTokenProxy(upstream.URL, a)
	if err != nil {
		t.Fatal(err)
	}
	proxy.floor = 0
	return func(device string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/realms/pinvou/protocol/openid-connect/token", strings.NewReader("grant_type=password&username=quota-user&password=synthetic"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Device-ID", device)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		return w
	}, calls
}

func TestTokenQuotaDeviceBoundaryAndHTTPConsumer(t *testing.T) {
	a, store := quotaRiskFixture(t)
	request, calls := quotaHTTP(t, a)
	key := quotaKey(a, "password", "device-minute", quotaDevice)
	for i := 1; i <= 11; i++ {
		w := request()
		if i <= 10 && w.Code != 200 || i == 11 && (w.Code != 429 || w.Header().Get("Retry-After") != "60") {
			t.Fatal("actual quota HTTP boundary", i, w.Code, w.Header().Get("Retry-After"))
		}
	}
	if calls.Load() != 10 || store.values[key] != "11" || store.valueTTL(key) != 60 {
		t.Fatal("rejected request cleared count, dispatched, or renewed deadline")
	}
	store.clock = store.clock.Add(time.Minute)
	if w := request(); w.Code != 200 || calls.Load() != 11 || store.values[key] != "1" {
		t.Fatal("later window failed to reopen after normal expiry", w.Code)
	}
}

func TestTokenQuotaSourceDay200And201KeepOriginalDeadline(t *testing.T) {
	a, store := quotaRiskFixture(t)
	key := quotaKey(a, "password", "source-day", "day-source")
	var firstDeadline time.Time
	for i := 1; i <= 201; i++ {
		store.clock = store.clock.Add(6 * time.Second)
		d := tokenRiskDimensions{action: "password", source: "day-source", device: fmt.Sprintf("device-%d", i), identifier: "day-user"}
		decision, err := a.Admit(context.Background(), d)
		if err != nil || i <= 200 && decision.RetryAfter != 0 || i == 201 && decision.RetryAfter <= 0 {
			t.Fatal("source day boundary changed", i, decision, err)
		}
		if i == 1 {
			firstDeadline = store.expires[key]
		}
		if !store.expires[key].Equal(firstDeadline) {
			t.Fatal("healthy daily quota deadline was renewed")
		}
	}
	if store.values[key] != "201" || store.valueTTL(key) != 85200 {
		t.Fatal("daily rejection lost original count or window")
	}
}

func TestTokenQuotaSourceMinute20And21(t *testing.T) {
	a, store := quotaRiskFixture(t)
	for i := 1; i <= 21; i++ {
		d := tokenRiskDimensions{action: "password", source: "one-source", device: fmt.Sprintf("rotated-device-%d", i), identifier: "minute-user"}
		decision, err := a.Admit(context.Background(), d)
		if err != nil || i <= 20 && decision.RetryAfter != 0 || i == 21 && decision.RetryAfter != time.Minute {
			t.Fatal("device rotation bypassed the source-minute quota", i, decision, err)
		}
	}
	if store.values[quotaKey(a, "password", "source-minute", "one-source")] != "21" {
		t.Fatal("source rejection cleared counted attempts")
	}
}

func TestTokenQuotaRefreshSessionAndActionBoundaries(t *testing.T) {
	a, store := quotaRiskFixture(t)
	var last tokenRiskDimensions
	for i := 1; i <= 31; i++ {
		last = tokenRiskDimensions{action: "refresh", source: fmt.Sprintf("source-%d", i), device: fmt.Sprintf("device-%d", i), session: "one-session", identifier: "refresh-session"}
		decision, err := a.Admit(context.Background(), last)
		if err != nil || i <= 30 && decision.RetryAfter != 0 || i == 31 && decision.RetryAfter != time.Minute {
			t.Fatal("refresh session boundary changed", i, decision, err)
		}
	}
	if store.values[quotaKey(a, "refresh", "session-minute", last.session)] != "31" {
		t.Fatal("session rejection cleared its own history")
	}
	password := tokenRiskDimensions{action: "password", source: last.source, device: last.device, identifier: "refresh-session"}
	if decision, err := a.Admit(context.Background(), password); err != nil || decision.RetryAfter != 0 {
		t.Fatal("refresh quota crossed into password action", decision, err)
	}
	last.session = "another-session"
	if decision, err := a.Admit(context.Background(), last); err != nil || decision.RetryAfter != 0 {
		t.Fatal("one session exhausted another session", decision, err)
	}
	provider := quotaKey(a, "refresh", "provider-minute", "keycloak")
	store.values[provider], store.expires[provider] = "1000", store.clock.Add(45*time.Second)
	if decision, err := a.Admit(context.Background(), last); err != nil || decision.RetryAfter != 45*time.Second || store.values[provider] != "1001" {
		t.Fatal("session rotation bypassed the provider quota", decision, err)
	}
}

func TestTokenQuotaLegacyCountersRepairOnceAndExpireThroughHTTP(t *testing.T) {
	a, store := quotaRiskFixture(t)
	request, calls := quotaHTTP(t, a)
	limits := []struct {
		kind, value string
		count       int
		window      time.Duration
	}{
		{"source-minute", quotaSource, 20, time.Minute}, {"source-day", quotaSource, 200, 24 * time.Hour},
		{"device-minute", quotaDevice, 10, time.Minute}, {"provider-minute", "keycloak", 1000, time.Minute},
	}
	for _, limit := range limits {
		store.values[quotaKey(a, "password", limit.kind, limit.value)] = fmt.Sprint(limit.count)
	}
	if w := request(); w.Code != 429 || w.Header().Get("Retry-After") != "86400" || calls.Load() != 0 {
		t.Fatal("legacy no-TTL quotas allowed upstream after limits", w.Code)
	}
	deadlines := make(map[string]time.Time)
	for _, limit := range limits {
		key := quotaKey(a, "password", limit.kind, limit.value)
		if store.values[key] != fmt.Sprint(limit.count+1) || store.valueTTL(key) != int64(limit.window/time.Second) {
			t.Fatal("legacy repair reset a quota count or used another dimension's window")
		}
		deadlines[key] = store.expires[key]
	}
	store.clock = store.clock.Add(5 * time.Second)
	if w := request(); w.Code != 429 || w.Header().Get("Retry-After") != "86395" || calls.Load() != 0 {
		t.Fatal("next request bypassed or renewed repaired quota", w.Code)
	}
	for _, limit := range limits {
		key := quotaKey(a, "password", limit.kind, limit.value)
		if !store.expires[key].Equal(deadlines[key]) || store.values[key] != fmt.Sprint(limit.count+2) {
			t.Fatal("rejection cleared count or extended a repaired deadline")
		}
	}
	store.clock = store.clock.Add(24 * time.Hour)
	if w := request(); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("later tick did not reopen expired repaired quota", w.Code)
	}
}

func TestTokenQuotaZeroTTLStillRejectsAndHealthyOtherDimensionSurvives(t *testing.T) {
	t.Run("zero-remaining-seconds", func(t *testing.T) {
		a, store := quotaRiskFixture(t)
		request, calls := quotaHTTP(t, a)
		key := quotaKey(a, "password", "device-minute", quotaDevice)
		store.values[key], store.expires[key] = "10", store.clock.Add(250*time.Millisecond)
		if w := request(); w.Code != 429 || w.Header().Get("Retry-After") != "1" || calls.Load() != 0 {
			t.Fatal("TTL0 over-limit quota was swallowed as allow", w.Code)
		}
		store.clock = store.clock.Add(250 * time.Millisecond)
		if w := request(); w.Code != 200 || store.values[key] != "1" {
			t.Fatal("expired subsecond quota did not reopen", w.Code)
		}
	})
	t.Run("healthy-daily-quota-after-legacy-device-expiry", func(t *testing.T) {
		a, store := quotaRiskFixture(t)
		request, calls := quotaHTTP(t, a)
		device, day := quotaKey(a, "password", "device-minute", quotaDevice), quotaKey(a, "password", "source-day", quotaSource)
		store.values[device] = "10"
		store.values[day], store.expires[day] = "200", store.clock.Add(120*time.Second)
		if w := request(); w.Code != 429 || w.Header().Get("Retry-After") != "120" {
			t.Fatal("healthy other quota lost to legacy repair", w.Code)
		}
		store.clock = store.clock.Add(61 * time.Second)
		if w := request(); w.Code != 429 || w.Header().Get("Retry-After") != "59" || store.values[device] != "1" || calls.Load() != 0 {
			t.Fatal("repaired quota expiry bypassed still-live daily quota", w.Code)
		}
	})
}

func TestTokenQuotaPartialCommandFailuresAndUnknownResponseFailClosed(t *testing.T) {
	for _, scenario := range []string{"deny-first-set", "deny-later-set", "deny-legacy-repair", "deny-incr", "lost-response"} {
		t.Run(scenario, func(t *testing.T) {
			a, store := quotaRiskFixture(t)
			request, calls := quotaHTTP(t, a)
			first, later := quotaKey(a, "password", "source-minute", quotaSource), quotaKey(a, "password", "source-day", quotaSource)
			switch scenario {
			case "deny-first-set":
				store.denyQuotaCommand, store.denyQuotaKey = "SET", first
			case "deny-later-set":
				store.denyQuotaCommand, store.denyQuotaKey = "SET", later
			case "deny-legacy-repair":
				store.values[later] = "200"
				store.denyQuotaCommand, store.denyQuotaKey = "SET", later
			case "deny-incr":
				store.values[later], store.expires[later] = "5", store.clock.Add(800*time.Second)
				store.denyQuotaCommand, store.denyQuotaKey = "INCR", later
			case "lost-response":
				store.lostQuotaReply = true
			}
			if w := request(); w.Code != 503 || calls.Load() != 0 {
				t.Fatal("failed/unknown quota script reached credential upstream", w.Code)
			}
			if scenario == "deny-first-set" {
				if store.values[first] != "" {
					t.Fatal("failed atomic first write left an unbounded quota")
				}
			} else if store.values[first] != "1" || store.valueTTL(first) != 60 {
				t.Fatal("partial failure erased earlier counted fact or its finite deadline")
			}
			firstDeadline := store.expires[first]
			store.clock = store.clock.Add(time.Second)
			store.denyQuotaCommand, store.denyQuotaKey = "", ""
			w := request()
			if scenario == "deny-legacy-repair" {
				if w.Code != 429 || store.values[later] != "201" || calls.Load() != 0 {
					t.Fatal("recovered legacy repair cleared count or bypassed quota", w.Code)
				}
			} else if w.Code != 200 || calls.Load() != 1 {
				t.Fatal("healthy retry failed to recover", w.Code)
			}
			if scenario != "deny-first-set" && (store.values[first] != "2" || !store.expires[first].Equal(firstDeadline)) {
				t.Fatal("retry reset earlier count or renewed existing window")
			}
			if scenario == "deny-incr" && (store.values[later] != "6" || store.valueTTL(later) != 799) {
				t.Fatal("retry erased pre-fault healthy count/deadline")
			}
		})
	}
}

func TestTokenQuotaDoesNotRequireSeparateExpirePermission(t *testing.T) {
	a, store := quotaRiskFixture(t)
	store.denyQuotaCommand = "EXPIRE"
	request, calls := quotaHTTP(t, a)
	if w := request(); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("first quota count still depends on a separate EXPIRE", w.Code)
	}
	if store.valueTTL(quotaKey(a, "password", "device-minute", quotaDevice)) != 60 {
		t.Fatal("first count was not atomically given a deadline")
	}
}

func TestTokenQuotaExpireFaultRecoveryRetainsSourceBoundaryThroughHTTP(t *testing.T) {
	a, store := quotaRiskFixture(t)
	request, calls := quotaHTTPRequests(t, a)
	store.denyQuotaCommand = "EXPIRE"
	first := request("first-device").Code
	store.denyQuotaCommand = ""
	for i := 2; i <= 21; i++ {
		w := request(fmt.Sprintf("rotated-device-%d", i))
		if i <= 20 && w.Code != 200 || i == 21 && (w.Code != 429 || w.Header().Get("Retry-After") != "60") {
			t.Fatal("source quota was bypassed after command recovery with other dimensions healthy", i, first, w.Code)
		}
	}
	key := quotaKey(a, "password", "source-minute", quotaSource)
	day := quotaKey(a, "password", "source-day", quotaSource)
	if first != 200 || calls.Load() != 20 || store.values[key] != "21" || store.valueTTL(key) != 60 || store.values[day] != "21" {
		t.Fatal("atomic creation/rejection lost counted facts or required EXPIRE")
	}
	store.clock = store.clock.Add(time.Minute)
	if w := request("later-device"); w.Code != 200 || store.values[key] != "1" || store.values[day] != "22" || calls.Load() != 21 {
		t.Fatal("later tick did not reopen only the expired source-minute window", w.Code)
	}
}

func TestTokenQuotaRejectsInvalidScriptResults(t *testing.T) {
	for _, value := range []any{int64(-1), int64(86401), int64(1<<63 - 1), "60", "not-a-number", []any{int64(0)}} {
		t.Run(fmt.Sprintf("%T-%v", value, value), func(t *testing.T) {
			a, store := quotaRiskFixture(t)
			store.overrideQuotaResult = value
			request, calls := quotaHTTP(t, a)
			if w := request(); w.Code != 503 || calls.Load() != 0 {
				t.Fatal("invalid/unbounded script result admitted upstream", w.Code)
			}
		})
	}
	a, store := quotaRiskFixture(t)
	store.overrideQuotaResult = int64(61)
	if _, err := a.Admit(context.Background(), tokenRiskDimensions{action: "refresh"}); err == nil {
		t.Fatal("reply exceeded the 60s maximum window of the actual included keys")
	}
}

func TestTokenQuotaConcurrentAdmissionsAndFailurePolicyIsolation(t *testing.T) {
	a, store := quotaRiskFixture(t)
	d := tokenRiskDimensions{action: "password", source: quotaSource, device: quotaDevice, identifier: "quota-user"}
	var accepted atomic.Int32
	var group sync.WaitGroup
	for range 24 {
		group.Go(func() {
			decision, err := a.Admit(context.Background(), d)
			if err != nil {
				t.Error(err)
			} else if decision.RetryAfter == 0 {
				accepted.Add(1)
			}
		})
	}
	group.Wait()
	if accepted.Load() != 10 || store.values[quotaKey(a, "password", "device-minute", quotaDevice)] != "24" {
		t.Fatal("concurrent admissions crossed the limit or cleared rejected counts", accepted.Load())
	}
	store.clock = store.clock.Add(time.Minute)
	failure := "pinvou:auth-failure:password:" + a.digest(d.identifier)
	store.values[failure], store.expires[failure] = "5", store.clock.Add(15*time.Minute)
	if decision, err := a.Admit(context.Background(), d); err != nil || decision.VerificationDelay != time.Second || store.values[failure] != "5" {
		t.Fatal("quota recovery changed five-failure pre-verification policy", decision, err)
	}
	stable := "pinvou:auth-failure:user:" + a.digest("stable-user")
	store.values["pinvou:auth-alias:"+a.digest(d.identifier)] = a.digest("stable-user")
	store.values[stable] = "10"
	if decision, err := a.Admit(context.Background(), d); err != nil || decision.RetryAfter != 15*time.Minute || store.values[stable] != "10" {
		t.Fatal("quota changes bypassed AUTH01 repair or stable-user lock", decision, err)
	}
}

func TestTokenQuotaScriptUsesExistingIdentityACL(t *testing.T) {
	commands := regexp.MustCompile(`redis\.call\('([A-Z]+)'`).FindAllStringSubmatch(admissionScriptSource, -1)
	for _, name := range []string{"redis-accounts.acl.template", "redis-personal.acl.template"} {
		data, err := os.ReadFile("../../deploy/production/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var identity string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "user identity ") {
				identity = " " + line + " "
			}
		}
		for _, command := range commands {
			if !strings.Contains(identity, " +"+strings.ToLower(command[1])+" ") {
				t.Fatalf("%s lacks %s", name, command[1])
			}
		}
	}
}
