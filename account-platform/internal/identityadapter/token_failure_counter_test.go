package identityadapter

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func failureCounterFixture(t *testing.T) (*RedisTokenAdmission, *identifierRiskStore) {
	t.Helper()
	store := &identifierRiskStore{values: map[string]string{}}
	a, err := NewRedisTokenAdmission("redis://127.0.0.1:1/0", base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("f", 32))))
	if err != nil {
		t.Fatal(err)
	}
	a.client.AddHook(store)
	t.Cleanup(func() { _ = a.client.Close() })
	return a, store
}

func TestRiskFailureFirstWriteAndCommandFailureRemainBounded(t *testing.T) {
	for _, denied := range []string{"expire", "set"} {
		t.Run(denied, func(t *testing.T) {
			a, store := failureCounterFixture(t)
			store.denyCommand = denied
			key := "pinvou:auth-failure:password:controlled"
			n, err := incrementFailure(context.Background(), a.client, key)
			if denied == "set" {
				if err == nil || store.values[key] != "" {
					t.Fatal("failed atomic SET created a failure count", n, err)
				}
				return
			}
			if err != nil || n != 1 || store.values[key] != "1" || store.valueTTL(key) != 900 {
				t.Fatal("first count depends on a separate EXPIRE or has no deadline", n, err)
			}
		})
	}
}

func TestRiskFailureLegacyStableLockRepairsOnceBeforeVerification(t *testing.T) {
	a, store := failureCounterFixture(t)
	ctx := context.Background()
	d := tokenRiskDimensions{action: "password", identifier: "legacy-user"}
	key := "pinvou:auth-failure:user:" + a.digest("stable-legacy")
	store.values["pinvou:auth-alias:"+a.digest(d.identifier)] = a.digest("stable-legacy")
	store.values[key] = "10" // The old crash state has no expiration.
	decision, err := a.Admit(ctx, d)
	if err != nil || decision.RetryAfter != 15*time.Minute || store.values[key] != "10" || store.valueTTL(key) != 900 {
		t.Fatal("already locked user did not acquire one finite recovery window", decision, err)
	}
	store.clock = store.clock.Add(3 * time.Minute)
	for range 3 {
		decision, err = a.Admit(ctx, d)
		if err != nil || decision.RetryAfter != 12*time.Minute || store.valueTTL(key) != 720 || store.values[key] != "10" {
			t.Fatal("lock observation renewed the window or erased the count", decision, err)
		}
	}
	store.clock = store.clock.Add(12 * time.Minute)
	decision, err = a.Admit(ctx, d)
	if err != nil || decision.RetryAfter != 0 || store.values[key] != "" {
		t.Fatal("repaired lock did not expire on a later admission", decision, err)
	}
	if _, err := a.Complete(ctx, d, false, ""); err != nil || store.values[key] != "1" || store.valueTTL(key) != 900 {
		t.Fatal("next failure did not start a fresh bounded window", err)
	}
}

func TestRiskFailureLegacyIdentifierReadAndWritesNeverRenewWindow(t *testing.T) {
	a, store := failureCounterFixture(t)
	d := tokenRiskDimensions{action: "password", identifier: "legacy-identifier"}
	key := "pinvou:auth-failure:password:" + a.digest(d.identifier)
	store.values[key] = "5"
	decision, err := a.Admit(context.Background(), d)
	if err != nil || decision.VerificationDelay != time.Second || store.values[key] != "5" || store.valueTTL(key) != 900 {
		t.Fatal("legacy identifier observation did not preserve delayed verification and count", decision, err)
	}
	store.clock = store.clock.Add(100 * time.Second)
	if _, err := a.Complete(context.Background(), d, false, ""); err != nil || store.values[key] != "6" || store.valueTTL(key) != 800 {
		t.Fatal("failure renewed an existing identifier window", err)
	}
	if _, err := a.Complete(context.Background(), d, true, "stable"); err != nil || store.values[key] != "6" || store.valueTTL(key) != 800 {
		t.Fatal("success changed risk history or its deadline", err)
	}
	decision, err = a.Admit(context.Background(), d)
	if err != nil || decision.VerificationDelay != 2*time.Second || store.valueTTL(key) != 800 {
		t.Fatal("next admission changed the progressive delay or renewed expiry", decision, err)
	}
}

func TestRiskFailureLegacyRepairFailureDoesNotAdmitOrEraseHistory(t *testing.T) {
	a, store := failureCounterFixture(t)
	d := tokenRiskDimensions{action: "password", identifier: "denied-repair"}
	key := "pinvou:auth-failure:user:" + a.digest("stable")
	store.values["pinvou:auth-alias:"+a.digest(d.identifier)] = a.digest("stable")
	store.values[key] = "10"
	store.denyCommand = "set"
	if _, err := a.Admit(context.Background(), d); err == nil || store.values[key] != "10" || store.valueTTL(key) != -1 {
		t.Fatal("failed repair admitted or changed the original risk count", err)
	}
	store.denyCommand = ""
	if locked, err := a.RecheckStableLock(context.Background(), d); err != nil || locked != 15*time.Minute || store.values[key] != "10" {
		t.Fatal("next healthy read could not recover the original locked state", locked, err)
	}
}

func TestRiskFailureUnknownWriteResultFailsClosedThenKeepsCommittedWindow(t *testing.T) {
	a, store := failureCounterFixture(t)
	store.unknownCounterReply = true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer upstream.Close()
	proxy, err := NewTokenProxy(upstream.URL, a)
	if err != nil {
		t.Fatal(err)
	}
	proxy.floor = 0
	r := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=password&username=reply-lost&password=synthetic"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	key := "pinvou:auth-failure:password:" + a.digest("reply-lost")
	if w.Code != http.StatusServiceUnavailable || store.values[key] != "1" || store.valueTTL(key) != 900 {
		t.Fatal("unknown counter result was admitted or left a permanent count", w.Code)
	}
	store.clock = store.clock.Add(time.Minute)
	if n, err := incrementFailure(context.Background(), a.client, key); err != nil || n != 2 || store.valueTTL(key) != 840 {
		t.Fatal("later failure reset a committed count/deadline after the lost response", n, err)
	}
}

func TestRiskFailureConcurrentObservationsAndIncrementsPreserveDeadline(t *testing.T) {
	a, store := failureCounterFixture(t)
	key := "pinvou:auth-failure:user:concurrent"
	store.values[key] = "10"
	if n, _, err := readFailureCounter(context.Background(), a.client, key); err != nil || n != 10 {
		t.Fatal(err)
	}
	store.clock = store.clock.Add(time.Minute)
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			if _, _, err := readFailureCounter(context.Background(), a.client, key); err != nil {
				t.Error(err)
			}
			if _, err := incrementFailure(context.Background(), a.client, key); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if store.values[key] != "30" || store.valueTTL(key) != 840 {
		t.Fatal("concurrent reads/writes lost increments or renewed the recovery window")
	}
}

func TestRiskFailureScriptUsesExistingProductionRedisPermissions(t *testing.T) {
	commands := regexp.MustCompile(`redis\.call\('([A-Z]+)'`).FindAllStringSubmatch(failureCounterLua, -1)
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
				t.Fatalf("%s does not allow script command %s", name, command[1])
			}
		}
		for _, command := range []string{"eval", "evalsha"} {
			if !strings.Contains(identity, " +"+command+" ") {
				t.Fatalf("%s does not allow %s", name, command)
			}
		}
	}
}
