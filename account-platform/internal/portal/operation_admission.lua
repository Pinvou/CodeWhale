local key = KEYS[1]
local rate, burst, fingerprint = tonumber(ARGV[1]), tonumber(ARGV[2]), ARGV[3]
local previous_kind = expect(key, 'hash')
need('HMGET', key, 'fingerprint', 'tokens', 'at')
need('HSET', key, 'fingerprint', fingerprint, 'tokens', '0', 'at', '0')
need('PEXPIRE', key, '1')
local now = milliseconds()
local saved = redis.call('HMGET', key, 'fingerprint', 'tokens', 'at')
local tokens = burst
if previous_kind == 'hash' and not saved[1] then error('PORTAL_ADMISSION_STATE_INVALID') end
if saved[1] then
  if saved[1] ~= fingerprint then error('PORTAL_ADMISSION_POLICY_MISMATCH') end
  local previous, at = tonumber(saved[2]), tonumber(saved[3])
  if not previous or not at or previous ~= previous or at ~= at or previous < 0 or previous > burst or at < 0 or at > 9007199254740991 then
    error('PORTAL_ADMISSION_STATE_INVALID')
  end
  tokens = math.min(burst, previous + math.max(0, now-at)*rate/1000)
end
local admitted, retry = 0, 0
if tokens >= 1 then
  tokens = tokens-1
  admitted = 1
else
  retry = math.max(1, math.ceil((1-tokens)*1000/rate))
end
-- One fixed hash per operation; rejected sources never create source/id keys.
redis.call('HSET', key, 'fingerprint', fingerprint, 'tokens', tostring(tokens), 'at', tostring(now))
redis.call('PEXPIRE', key, math.max(1000, math.ceil(burst*1000/rate)))
return {admitted, retry}
