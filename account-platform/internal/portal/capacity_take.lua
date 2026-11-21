expect(KEYS[1], 'string')
expect(KEYS[2], 'zset')
need('GETDEL', KEYS[1])
need('ZREM', KEYS[2], ARGV[1])
-- No automatic client retries: a lost response remains a storage error, never
-- a second GETDEL that misreports an already-consumed object as missing.
local value = redis.call('GETDEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
return value
