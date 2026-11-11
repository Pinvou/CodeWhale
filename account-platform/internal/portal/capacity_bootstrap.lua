local index, inventory, prefix = KEYS[1], KEYS[2], ARGV[1]
expect(index, 'zset')
expect(inventory, 'hash')
need('HMGET', inventory, 'ready', 'cursor')
local state = redis.call('HMGET', inventory, 'ready', 'cursor')
if state[1] == '1' then return {1, 0} end
if state[1] then error('PORTAL_ADMISSION_STATE_INVALID') end
local cursor = state[2] or '0'
-- Production application ACLs do not gain global key enumeration. An
-- operations principal performs the initial inventory; consumers keep working.
if not redis.acl_check_cmd('SCAN', cursor, 'MATCH', prefix..'*', 'COUNT', '128') then return {-4, 1000} end
need('ZADD', index, '0', 'probe')
need('ZREM', index, 'probe')
need('HSET', inventory, 'cursor', '0')
need('HSET', inventory, 'ready', '1', 'cursor', '0')
need('PEXPIREAT', index, '1')
need('ZREVRANGE', index, '0', '0', 'WITHSCORES')
local page = redis.call('SCAN', cursor, 'MATCH', prefix..'*', 'COUNT', 128)
-- COUNT is a Redis hint. Refuse an oversized page instead of unbounded work
-- or advancing the cursor past unprocessed original objects.
if #page[2] > 512 then error('PORTAL_INVENTORY_PAGE_TOO_LARGE') end
for _, key in ipairs(page[2]) do need('PEXPIRETIME', key) end
local now = milliseconds()
local last = redis.call('ZREVRANGE', index, 0, 0, 'WITHSCORES')
local latest = tonumber(last[2]) or 0
if latest == math.huge then error('PORTAL_ADMISSION_STATE_INVALID') end
local deadlines = {}
for i, key in ipairs(page[2]) do
  deadlines[i] = redis.call('PEXPIRETIME', key)
  if deadlines[i] == -1 then error('PORTAL_TRANSIENT_TTL_REQUIRED') end
end
for i, key in ipairs(page[2]) do
  local deadline = deadlines[i]
  local id = string.sub(key, string.len(prefix)+1)
  if deadline > now then
    redis.call('ZADD', index, deadline, id)
    latest = math.max(latest, deadline)
    redis.call('PEXPIREAT', index, latest)
  else
    redis.call('ZREM', index, id)
  end
end
if latest > 0 then redis.call('PEXPIREAT', index, latest) end
-- A Lua/ACL/OOM error above leaves the old cursor: the same page is safe to
-- replay because ZADD is idempotent and no original payload/TTL is rewritten.
if page[1] == '0' then
  redis.call('HSET', inventory, 'ready', '1', 'cursor', '0')
  return {1, 0}
end
redis.call('HSET', inventory, 'cursor', page[1])
return {-3, 1000}
