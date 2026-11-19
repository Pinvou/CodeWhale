expect(KEYS[1], 'string')
expect(KEYS[2], 'zset')
need('EXISTS', KEYS[1])
need('ZREM', KEYS[2], ARGV[1])
-- A lost SET reply may have left a real object. Never delete or unaccount it.
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('ZREM', KEYS[2], ARGV[1])
end
return 1
