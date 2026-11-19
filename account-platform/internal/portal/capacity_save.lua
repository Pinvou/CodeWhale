local data, index, inventory = KEYS[1], KEYS[2], KEYS[3]
local id, body, ttl, capacity = ARGV[1], ARGV[2], tonumber(ARGV[3]), tonumber(ARGV[4])
expect(data, 'string')
expect(index, 'zset')
expect(inventory, 'hash')
need('HGET', inventory, 'ready')
need('GET', data)
need('ZRANGEBYSCORE', index, '-inf', '+inf', 'LIMIT', '0', '256')
need('ZREM', index, id)
need('ZCOUNT', index, '-inf', '+inf')
need('ZADD', index, '0', id)
need('ZSCORE', index, id)
need('ZREVRANGE', index, '0', '0', 'WITHSCORES')
need('PEXPIREAT', index, '1')
if redis.call('HGET', inventory, 'ready') ~= '1' then return {-3, 1000} end
local existing = redis.call('GET', data)
if existing then
  if existing == body then return {1, 0} end
  return {-2, 0}
end
local now = milliseconds()
local deadline = now+ttl
need('SET', data, body, 'PXAT', deadline, 'NX')
local expired = redis.call('ZRANGEBYSCORE', index, '-inf', now, 'LIMIT', 0, 256)
if #expired > 0 then redis.call('ZREM', index, unpack(expired)) end
local reserved = tonumber(redis.call('ZSCORE', index, id))
if (not reserved or reserved <= now) and redis.call('ZCOUNT', index, '('..tostring(now), '+inf') >= capacity then
  local first = redis.call('ZRANGEBYSCORE', index, '('..tostring(now), '+inf', 'WITHSCORES', 'LIMIT', 0, 1)
  return {0, math.max(1, math.min(600000, tonumber(first[2])-now))}
end
local last = redis.call('ZREVRANGE', index, 0, 0, 'WITHSCORES')
local latest = tonumber(last[2]) or 0
if latest == math.huge then error('PORTAL_ADMISSION_STATE_INVALID') end
local retention = math.max(deadline, latest)
-- Reserve capacity before writing a payload. Redis Lua errors do not roll
-- back writes: explicitly undo only this new reservation if SET fails.
redis.call('ZADD', index, deadline, id)
local retained = redis.pcall('PEXPIREAT', index, retention)
if type(retained) == 'table' and retained.err then
  redis.call('ZREM', index, id)
  return redis.error_reply('PORTAL_CAPACITY_RESERVATION_FAILED')
end
local saved = redis.pcall('SET', data, body, 'PXAT', deadline, 'NX')
if not saved or (type(saved) == 'table' and saved.err) then
  redis.call('ZREM', index, id)
  return redis.error_reply('PORTAL_TRANSIENT_WRITE_FAILED')
end
return {1, 0}
