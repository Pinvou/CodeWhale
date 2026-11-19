local function need(...)
  if not redis.acl_check_cmd(...) then
    error('PORTAL_ADMISSION_COMMANDS_REQUIRED')
  end
end
local function kind(key)
  need('TYPE', key)
  local result = redis.call('TYPE', key)
  if type(result) == 'table' then return result.ok end
  return result
end
local function expect(key, expected)
  local actual = kind(key)
  if actual ~= 'none' and actual ~= expected then
    error('PORTAL_ADMISSION_STATE_INVALID')
  end
  return actual
end
local function milliseconds()
  need('TIME')
  local now = redis.call('TIME')
  return tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000)
end
