package identityadapter

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Only Redis primitives, clock and command faults are modeled here. The quota
// algorithm is the unchanged production admissionScriptSource loaded below.
// Lua5.1 is a test-only CLI dependency installed by the mandatory CI step.
const quotaLuaCommands = `
assert(_VERSION == 'Lua 5.1', 'quota fixture requires Lua5.1')
local function expire(key)
  if expires[key] and expires[key] <= now then values[key], expires[key] = nil, nil end
end
redis = {call = function(command, key, ...)
  if command == denyCommand and (denyKey == '' or key == denyKey) then error('controlled command denial') end
  expire(key)
  local args = {...}
  if command == 'GET' then return values[key] or false end
  if command == 'TTL' then
    if not values[key] then return -2 end
    if not expires[key] then return -1 end
    return math.floor((expires[key] - now + 500) / 1000)
  end
  if command == 'SET' then
    if #args ~= 3 or args[2] ~= 'EX' or tonumber(args[3]) <= 0 then error('invalid SET EX') end
    values[key], expires[key] = tostring(args[1]), now + tonumber(args[3]) * 1000
    return 'OK'
  end
  if command == 'INCR' then
    local value = tonumber(values[key] or '0')
    -- This fixture's counters are small. Reject values outside Lua's exact
    -- integer range instead of approximating Redis's int64 implementation.
    if not value or value ~= math.floor(value) or math.abs(value) >= 9007199254740991 then error('invalid fixture integer') end
    value = value + 1
    values[key] = string.format('%.0f', value)
    return value
  end
  if command == 'EXPIRE' then
    if not values[key] then return 0 end
    expires[key] = now + tonumber(args[1]) * 1000
    return 1
  end
  error('unsupported Redis primitive')
end}
local loaded = assert(loadstring(productionSource))
local ok, result = pcall(loaded)
local function hex(value)
  return (value:gsub('.', function(byte) return string.format('%02x', string.byte(byte)) end))
end
-- Preserve preceding writes even when a later command caused a Lua error.
for key, value in pairs(values) do io.write('V\t', hex(key), '\t', hex(value), '\n') end
for key, deadline in pairs(expires) do io.write('E\t', hex(key), '\t', string.format('%.0f', deadline), '\n') end
if ok and type(result) == 'number' and result == math.floor(result) then
  io.write('RESULT\t', string.format('%.0f', result), '\n')
else
  io.write('ERROR\n')
end
`

func quotaLuaString(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for i := range len(value) {
		fmt.Fprintf(&out, "\\%03d", value[i])
	}
	out.WriteByte('"')
	return out.String()
}

type quotaLuaOutput struct{ buffer bytes.Buffer }

func (out *quotaLuaOutput) Write(p []byte) (int, error) {
	if out.buffer.Len()+len(p) > 1<<20 {
		return 0, errors.New("quota Lua output exceeded fixture bound")
	}
	return out.buffer.Write(p)
}

func (s *quotaRiskStore) executeLua(ctx context.Context, keys []string, args []int64) (int64, error) {
	var input strings.Builder
	fmt.Fprintf(&input, "local now=%d\nlocal denyCommand=%s\nlocal denyKey=%s\nlocal productionSource=%s\n", s.clock.UnixMilli(), quotaLuaString(s.denyQuotaCommand), quotaLuaString(s.denyQuotaKey), quotaLuaString(admissionScriptSource))
	input.WriteString("local values={}\nlocal expires={}\nKEYS={}\nARGV={}\n")
	for key, value := range s.values {
		fmt.Fprintf(&input, "values[%s]=%s\n", quotaLuaString(key), quotaLuaString(value))
	}
	for key, deadline := range s.expires {
		fmt.Fprintf(&input, "expires[%s]=%d\n", quotaLuaString(key), deadline.UnixMilli())
	}
	for i, key := range keys {
		fmt.Fprintf(&input, "KEYS[%d]=%s\n", i+1, quotaLuaString(key))
	}
	for i, arg := range args {
		fmt.Fprintf(&input, "ARGV[%d]=%s\n", i+1, quotaLuaString(strconv.FormatInt(arg, 10)))
	}
	input.WriteString(quotaLuaCommands)
	if input.Len() > 1<<20 {
		return 0, errors.New("quota Lua input exceeded fixture bound")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, s.luaCommand[0], append(s.luaCommand[1:], "-")...)
	command.WaitDelay = time.Second
	// Lua5.1's LUA_INIT can execute user initialization before stdin. This
	// isolated command fixture must execute only the supplied test program.
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.EqualFold(name, "LUA_INIT") {
			command.Env = append(command.Env, value)
		}
	}
	command.Stdin = strings.NewReader(input.String())
	output := &quotaLuaOutput{}
	command.Stdout, command.Stderr = output, io.Discard
	if err := command.Run(); err != nil {
		return 0, fmt.Errorf("quota Lua CLI failed: %w", err)
	}
	values, expires := map[string]string{}, map[string]time.Time{}
	var result int64
	var complete, failed bool
	for _, line := range strings.Split(strings.TrimSuffix(output.buffer.String(), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if complete {
			return 0, errors.New("quota Lua emitted data after terminal status")
		}
		switch {
		case len(fields) == 3 && (fields[0] == "V" || fields[0] == "E"):
			key, err := hex.DecodeString(fields[1])
			if err != nil {
				return 0, errors.New("invalid quota Lua key encoding")
			}
			if fields[0] == "V" {
				value, err := hex.DecodeString(fields[2])
				if err != nil {
					return 0, errors.New("invalid quota Lua value encoding")
				}
				values[string(key)] = string(value)
			} else {
				deadline, err := strconv.ParseInt(fields[2], 10, 64)
				if err != nil {
					return 0, errors.New("invalid quota Lua deadline")
				}
				expires[string(key)] = time.UnixMilli(deadline)
			}
		case len(fields) == 2 && fields[0] == "RESULT":
			var err error
			result, err = strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, errors.New("invalid quota Lua result")
			}
			complete = true
		case len(fields) == 1 && fields[0] == "ERROR":
			complete, failed = true, true
		default:
			return 0, errors.New("invalid quota Lua command-fixture output")
		}
	}
	if !complete {
		return 0, errors.New("quota Lua omitted its terminal status")
	}
	s.luaCalls++
	s.values, s.expires = values, expires
	if failed {
		return 0, errors.New("controlled quota Lua command failure")
	}
	return result, nil
}

// The test executable stands in for the CLI only to inspect the actual child
// environment. This default regression requires neither Python nor Lua and
// does not claim coverage of production Lua control flow.
func TestQuotaLuaEnvironmentIsolation(t *testing.T) {
	const helper = "PINVOU_TEST_QUOTA_ENV_HELPER"
	const retained = "PINVOU_TEST_QUOTA_RETAINED_ENV"
	if os.Getenv(helper) == "1" {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			os.Exit(2)
		}
		for _, value := range os.Environ() {
			name, _, _ := strings.Cut(value, "=")
			if strings.EqualFold(name, "LUA_INIT") {
				fmt.Print("ERROR\n")
				os.Exit(0)
			}
		}
		if os.Getenv(retained) != "retained" {
			fmt.Print("ERROR\n")
			os.Exit(0)
		}
		fmt.Print("RESULT\t0\n")
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LUA_INIT", "Lua_Init"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(helper, "1")
			t.Setenv(retained, "retained")
			t.Setenv(name, "synthetic-initializer")
			store := &quotaRiskStore{
				identifierRiskStore: &identifierRiskStore{values: map[string]string{}, expires: map[string]time.Time{}, clock: time.Unix(10000, 0)},
				luaCommand:          []string{executable, "-test.run=^TestQuotaLuaEnvironmentIsolation$", "--"},
			}
			if _, err := store.executeLua(context.Background(), []string{"environment-probe"}, []int64{10, 60}); err != nil {
				t.Fatalf("child inherited Lua initialization or lost ordinary environment: %v", err)
			}
		})
	}
}
