package identityadapter

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const failureCounterWindow = 15 * time.Minute

// SET with EX creates the count and its deadline in one Redis command. Lua
// errors do not roll back earlier commands, so INCR followed by EXPIRE would
// still leave a permanent key when the latter command is denied or fails.
// Legacy permanent counts have no recoverable original deadline. Preserve the
// count and attach one bounded window on first observation, including reads of
// users already locked before verification. Never renew an existing deadline.
const failureCounterLua = `
local value = redis.call('GET', KEYS[1])
local window = tonumber(ARGV[2])
local function fresh()
  if ARGV[1] == '0' then return {'0', 0} end
  redis.call('SET', KEYS[1], '1', 'EX', window)
  return {'1', window}
end
if not value then return fresh() end
local ttl = redis.call('TTL', KEYS[1])
if ttl == -2 then return fresh() end
if ttl == -1 then
  redis.call('SET', KEYS[1], value, 'EX', window)
  ttl = window
end
if ARGV[1] == '1' then
  redis.call('INCR', KEYS[1])
  value = redis.call('GET', KEYS[1])
end
return {value, ttl}`

var failureCounterScript = redis.NewScript(failureCounterLua)

func readFailureCounter(ctx context.Context, client *redis.Client, key string) (int64, time.Duration, error) {
	return failureCounter(ctx, client, key, false)
}

func failureCounter(ctx context.Context, client *redis.Client, key string, increment bool) (int64, time.Duration, error) {
	mode := "0"
	if increment {
		mode = "1"
	}
	values, err := failureCounterScript.Run(ctx, client, []string{key}, mode, int64(failureCounterWindow/time.Second)).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	if len(values) != 2 || values[0] < 0 || values[1] < 0 {
		return 0, 0, errors.New("authentication failure counter result is invalid")
	}
	return values[0], time.Duration(values[1]) * time.Second, nil
}
