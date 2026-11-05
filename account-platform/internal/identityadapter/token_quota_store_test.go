package identityadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Quota tests use a stateful command model rather than scriptRetry. CI also
// requires the Lua5.1 CLI mode, which executes the production script unchanged.
// Command state/faults are controlled; neither mode is a real Redis server.
type quotaRiskStore struct {
	*identifierRiskStore
	denyQuotaCommand    string
	denyQuotaKey        string
	lostQuotaReply      bool
	overrideQuotaResult any
	luaCommand          []string
	luaCalls            int
}

func quotaRiskFixture(t *testing.T) (*RedisTokenAdmission, *quotaRiskStore) {
	t.Helper()
	store := &quotaRiskStore{identifierRiskStore: &identifierRiskStore{
		values: map[string]string{}, expires: map[string]time.Time{}, clock: time.Unix(10000, 0),
	}}
	if command := os.Getenv("PINVOU_TEST_AUTH_QUOTA_LUA_COMMAND"); command != "" {
		if json.Unmarshal([]byte(command), &store.luaCommand) != nil || len(store.luaCommand) == 0 || strings.TrimSpace(store.luaCommand[0]) == "" {
			t.Fatal("quota Lua command must be a nonempty JSON argument array")
		}
		t.Cleanup(func() { t.Logf("AUTH_QUOTA_ACTUAL_LUA executions=%d", store.luaCalls) })
	}
	a, err := NewRedisTokenAdmission("redis://127.0.0.1:1/0", base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("q", 32))))
	if err != nil {
		t.Fatal(err)
	}
	a.client.AddHook(store)
	t.Cleanup(func() { _ = a.client.Close() })
	return a, store
}

func (s *quotaRiskStore) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	other := s.identifierRiskStore.ProcessHook(next)
	return func(ctx context.Context, c redis.Cmder) error {
		a := c.Args()
		if c.Name() != "evalsha" && c.Name() != "eval" || len(a) < 3 || a[1] != admissionScript.Hash() && a[1] != admissionScriptSource {
			return other(ctx, c)
		}
		if err := ctx.Err(); err != nil {
			c.SetErr(err)
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		n, ok := a[2].(int)
		if !ok || n < 1 || len(a) != 3+n*3 {
			return errors.New("invalid quota script key/argument shape")
		}
		keys := make([]string, n)
		args := make([]int64, n*2)
		for i := range keys {
			keys[i], ok = a[3+i].(string)
			if !ok {
				return errors.New("invalid quota script key")
			}
		}
		for i := range args {
			number, ok := a[3+n+i].(int)
			if !ok || number < 1 {
				return errors.New("invalid quota script limit/window")
			}
			args[i] = int64(number)
		}
		s.scriptCalls++
		var retry int64
		var err error
		if len(s.luaCommand) != 0 {
			retry, err = s.executeLua(ctx, keys, args)
		} else {
			retry, err = s.referenceQuota(keys, args)
		}
		if err == nil && s.lostQuotaReply {
			s.lostQuotaReply = false
			err = errors.New("controlled lost quota script response")
		}
		if err != nil {
			c.SetErr(err)
			return err
		}
		if s.overrideQuotaResult != nil {
			c.(*redis.Cmd).SetVal(s.overrideQuotaResult)
			return nil
		}
		c.(*redis.Cmd).SetVal(retry)
		return nil
	}
}

func (s *quotaRiskStore) quotaCommand(command, key, value string, window int64) (string, error) {
	if s.denyQuotaCommand == command && (s.denyQuotaKey == "" || s.denyQuotaKey == key) {
		return "", errors.New("controlled quota command denial")
	}
	s.expireValue(key)
	switch command {
	case "GET":
		return s.values[key], nil
	case "TTL":
		return strconv.FormatInt(s.valueTTL(key), 10), nil
	case "SET":
		s.values[key] = value
		s.expires[key] = s.clock.Add(time.Duration(window) * time.Second)
		return "OK", nil
	case "INCR":
		var n int64
		var err error
		if s.values[key] != "" {
			n, err = strconv.ParseInt(s.values[key], 10, 64)
		}
		if err != nil || n == 1<<63-1 {
			return "", errors.New("invalid or overflowing quota count")
		}
		s.values[key] = strconv.FormatInt(n+1, 10)
		return s.values[key], nil
	default:
		return "", fmt.Errorf("unknown quota model command %s", command)
	}
}

// An independent desired-state model for default deterministic Go tests.
// The required CI Lua CLI mode instead runs the production script, allowing
// an old-script overlay to falsify this same consumer/state test suite.
func (s *quotaRiskStore) referenceQuota(keys []string, args []int64) (int64, error) {
	var retry int64
	for i, key := range keys {
		limit, window := args[i*2], args[i*2+1]
		value, err := s.quotaCommand("GET", key, "", 0)
		if err != nil {
			return 0, err
		}
		ttl := int64(-2)
		if value != "" {
			raw, err := s.quotaCommand("TTL", key, "", 0)
			if err != nil {
				return 0, err
			}
			ttl, _ = strconv.ParseInt(raw, 10, 64)
		}
		if ttl == -2 {
			if _, err := s.quotaCommand("SET", key, "1", window); err != nil {
				return 0, err
			}
			value, ttl = "1", window
		} else {
			if ttl == -1 {
				if _, err := s.quotaCommand("SET", key, value, window); err != nil {
					return 0, err
				}
				ttl = window
			}
			value, err = s.quotaCommand("INCR", key, "", 0)
			if err != nil {
				return 0, err
			}
		}
		count, _ := strconv.ParseInt(value, 10, 64)
		if count > limit {
			retry = max(retry, max(ttl, 1))
		}
	}
	return retry, nil
}

func quotaKey(a *RedisTokenAdmission, action, kind, value string) string {
	return "pinvou:auth-admission:" + action + ":" + kind + ":" + a.digest(value)
}
