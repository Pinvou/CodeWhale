local index, inventory = KEYS[1], KEYS[2]
local id, ttl, capacity = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3])
expect(index, 'zset')
expect(inventory, 'hash')
need('HGET', inventory, 'ready')
need('ZSCORE', index, id)
need('ZRANGEBYSCORE', index, '-inf', '+inf', 'LIMIT', '0', '256')
need('ZREVRANGE', index, '0', '0', 'WITHSCORES')
need('ZREM', index, id)
need('ZCOUNT', index, '-inf', '+inf')
need('ZADD', index, '0', id)
need('PEXPIREAT', index, '1')
if redis.call('HGET', inventory, 'ready') ~= '1' then return {-3, 1000} end
local now = milliseconds()
local existing = tonumber(redis.call('ZSCORE', index, id))
if existing and existing > now then return {1, 0} end
local expired = redis.call('ZRANGEBYSCORE', index, '-inf', now, 'LIMIT', 0, 256)
if #expired > 0 then redis.call('ZREM', index, unpack(expired)) end
if redis.call('ZCOUNT', index, '('..tostring(now), '+inf') >= capacity then
  local first = redis.call('ZRANGEBYSCORE', index, '('..tostring(now), '+inf', 'WITHSCORES', 'LIMIT', 0, 1)
  return {0, math.max(1, math.min(600000, tonumber(first[2])-now))}
end
local last = redis.call('ZREVRANGE', index, 0, 0, 'WITHSCORES')
local latest = tonumber(last[2]) or 0
if latest == math.huge then error('PORTAL_ADMISSION_STATE_INVALID') end
local deadline = now+ttl
redis.call('ZADD', index, deadline, id)
local result = redis.pcall('PEXPIREAT', index, math.max(latest,deadline))
if type(result)=='table' and result.err then
  redis.call('ZREM', index, id)
  return redis.error_reply('PORTAL_CAPACITY_RESERVATION_FAILED')
end
return {1, 0}
