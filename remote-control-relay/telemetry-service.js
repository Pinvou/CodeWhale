import crypto from "node:crypto";
import { isIP } from "node:net";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  IPv4,
  IPv6,
  loadVectorIndexFromFile,
  newWithVectorIndex,
  verifyFromFile,
} from "ip2region.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const TELEMETRY_PREFIX = "/pinvou3/telemetry";
const STATS_PREFIX = "/pinvou3/stats";
const MAX_JSON_BYTES = 256 * 1024;
const MAX_EVENTS_PER_BATCH = 200;
const BEHAVIOR_EVENT_NAMES = new Set([
  "task_started",
  "task_finished",
  "voice_started",
  "tool_call_completed",
  "tool_install_completed",
  "scene_triggered",
  "model_used",
]);
const RANGE_DAYS = new Map([
  ["today", 1],
  ["7", 7],
  ["30", 30],
]);
const ONLINE_WINDOW_MS = 5 * 60_000;
const ADMIN_SESSION_MS = 12 * 60 * 60_000;
const DAY_MS = 24 * 60 * 60_000;
const REGION_REFRESH_MS = DAY_MS;
const STATS_TIMEZONE_OFFSET_MS = 8 * 60 * 60_000;
const DEFAULT_EVENT_RETENTION_MS = 35 * DAY_MS;
const DEFAULT_MAX_EVENTS = 100_000;
const DEFAULT_MAX_DEVICES = 10_000;
const DEFAULT_EVENT_RATE_PER_MINUTE = 2_000;
const DEFAULT_GLOBAL_EVENT_RATE_PER_MINUTE = 20_000;
const RATE_LIMIT_WINDOW_MS = 60_000;
const MAX_RATE_LIMIT_BUCKETS = 20_000;
const MAX_CLIENT_FUTURE_SKEW_MS = 5 * 60_000;

function json(res, status, value, headers = {}) {
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
    "x-content-type-options": "nosniff",
    ...headers,
  });
  res.end(JSON.stringify(value));
}

function html(res, status, value) {
  res.writeHead(status, {
    "content-type": "text/html; charset=utf-8",
    "cache-control": "no-store",
    "content-security-policy": "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
    "x-content-type-options": "nosniff",
    "x-frame-options": "DENY",
    "referrer-policy": "no-referrer",
  });
  res.end(value);
}

function safeEqual(a, b) {
  const left = Buffer.from(String(a || ""));
  const right = Buffer.from(String(b || ""));
  return left.length === right.length && crypto.timingSafeEqual(left, right);
}

function randomId(prefix, bytes = 18) {
  return `${prefix}${crypto.randomBytes(bytes).toString("base64url")}`;
}

function boundedText(value, max = 128) {
  return String(value || "").trim().slice(0, max);
}

function boundedCount(value) {
  const n = Number(value);
  if (!Number.isFinite(n) || n < 0) return 0;
  return Math.min(Math.floor(n), Number.MAX_SAFE_INTEGER);
}

function boundedInteger(value, fallback, minimum, maximum) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return fallback;
  return Math.min(maximum, Math.max(minimum, Math.floor(parsed)));
}

class TelemetryHttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

function normalizedState(value) {
  return ["online", "idle", "active", "generating"].includes(value) ? value : "online";
}

function isLocalIp(value) {
  const ip = String(value || "").trim().toLowerCase();
  const version = isIP(ip);
  if (version === 4) {
    const parts = ip.split(".").map(Number);
    return parts[0] === 0
      || parts[0] === 10
      || parts[0] === 127
      || (parts[0] === 100 && parts[1] >= 64 && parts[1] <= 127)
      || (parts[0] === 169 && parts[1] === 254)
      || (parts[0] === 172 && parts[1] >= 16 && parts[1] <= 31)
      || (parts[0] === 192 && parts[1] === 168)
      || parts[0] >= 224;
  }
  if (version === 6) {
    return ip === "::" || ip === "::1" || ip.startsWith("fc") || ip.startsWith("fd")
      || /^fe[89ab]/.test(ip);
  }
  return true;
}

function regionPart(value) {
  const part = boundedText(value, 64);
  return part && part !== "0" ? part : "";
}

export function parseIp2Region(value) {
  const [country, province, city, , countryCode] = String(value || "").split("|");
  const normalized = {
    country: regionPart(country),
    province: regionPart(province),
    city: regionPart(city),
    country_code: regionPart(countryCode).toUpperCase(),
  };
  return normalized.country || normalized.province || normalized.city ? normalized : null;
}

export function formatDeviceRegion(region) {
  if (!region || typeof region !== "object") return "未知";
  const country = regionPart(region.country);
  const province = regionPart(region.province);
  const city = regionPart(region.city);
  const domestic = region.country_code === "CN" || country === "中国";
  const parts = domestic
    ? (province || city ? [province, city] : [country])
    : [country, province, city];
  const unique = parts.filter((part, index) => part && parts.indexOf(part) === index);
  return unique.slice(0, 3).join(" / ") || "未知";
}

export function createIpRegionResolver(options = {}) {
  const searchers = new Map();
  const sources = [
    [4, IPv4, options.ipv4Path],
    [6, IPv6, options.ipv6Path],
  ];
  for (const [versionNumber, version, path] of sources) {
    if (!path || !existsSync(path)) continue;
    try {
      verifyFromFile(path);
      const vectorIndex = loadVectorIndexFromFile(path);
      searchers.set(versionNumber, newWithVectorIndex(version, path, vectorIndex));
    } catch (error) {
      console.error(`[telemetry] failed to initialize IPv${versionNumber} region database:`, error);
    }
  }
  return {
    enabled: searchers.size > 0,
    async lookup(ip) {
      const version = isIP(ip);
      if (!version || isLocalIp(ip)) return null;
      const searcher = searchers.get(version);
      if (!searcher) return null;
      try {
        return parseIp2Region(await searcher.search(ip));
      } catch (error) {
        console.error(`[telemetry] failed to resolve IPv${version} region:`, error);
        return null;
      }
    },
  };
}

function statsDay(timestamp) {
  return Math.floor((Number(timestamp) + STATS_TIMEZONE_OFFSET_MS) / DAY_MS);
}

function statsDayLabel(day) {
  return new Date(day * DAY_MS).toISOString().slice(0, 10);
}

export function buildStatsOverview(devices, events, now = Date.now()) {
  const today = statsDay(now);
  const trendStart = today - 29;
  const activeWeekStart = today - 6;
  const deviceById = new Map(devices.map((device) => [device.device_id, device]));

  const online = devices.filter((device) => now - Number(device.last_seen_at || 0) <= ONLINE_WINDOW_MS);
  const activeToday = devices.filter((device) => statsDay(device.last_active_at || 0) === today);
  const activeWeek = devices.filter((device) => statsDay(device.last_active_at || 0) >= activeWeekStart);
  const trend = new Map();
  for (let day = trendStart; day <= today; day += 1) {
    trend.set(day, { day: statsDayLabel(day), turns: 0, device_ids: new Set() });
  }

  for (const event of events) {
    if (!deviceById.has(event.device_id)) continue;
    const row = trend.get(statsDay(event.occurred_at));
    if (!row) continue;
    row.turns += 1;
    row.device_ids.add(event.device_id);
  }

  const versionMap = new Map();
  for (const device of activeWeek) {
    const version = device.app_version || "未知";
    versionMap.set(version, (versionMap.get(version) || 0) + 1);
  }

  return {
    generated_at: now,
    online_window_seconds: ONLINE_WINDOW_MS / 1000,
    counts: {
      online: online.length,
      active_today: activeToday.length,
      active_7d: activeWeek.length,
    },
    usage_trend: [...trend.values()].map((row) => ({
      day: row.day,
      active_devices: row.device_ids.size,
      turns: row.turns,
    })),
    active_versions: [...versionMap.entries()]
      .map(([version, count]) => ({ version, count }))
      .sort((a, b) => b.count - a.count || a.version.localeCompare(b.version)),
  };
}

export function buildDeviceList(devices, events, now = Date.now()) {
  const today = statsDay(now);
  const weekStart = today - 6;
  const activity = new Map();
  for (const event of events) {
    const eventDay = statsDay(event.occurred_at);
    if (eventDay < weekStart) continue;
    const row = activity.get(event.device_id) || { turnsToday: 0, turns7d: 0 };
    row.turns7d += 1;
    if (eventDay === today) row.turnsToday += 1;
    activity.set(event.device_id, row);
  }

  return devices
    .map((device) => {
      const recent = activity.get(device.device_id) || { turnsToday: 0, turns7d: 0 };
      const online = now - Number(device.last_seen_at || 0) <= ONLINE_WINDOW_MS;
      return {
        device_id: device.device_id,
        app_version: device.app_version,
        platform: device.platform,
        state: device.state,
        online,
        region: formatDeviceRegion(device.region),
        first_seen_at: device.first_seen_at,
        last_active_at: device.last_active_at,
        turns_today: recent.turnsToday,
        turns_7d: recent.turns7d,
      };
    })
    .sort((a, b) => Number(b.online) - Number(a.online)
      || Number(b.last_active_at || 0) - Number(a.last_active_at || 0));
}

function rangeDays(value) {
  return RANGE_DAYS.get(String(value || "30")) || 30;
}

function rangeWindow(range, now = Date.now()) {
  const days = rangeDays(range);
  const endDay = statsDay(now);
  return { range: days === 1 ? "today" : String(days), days, startDay: endDay - days + 1, endDay };
}

function dayRows(window) {
  const rows = [];
  for (let day = window.startDay; day <= window.endDay; day += 1) rows.push(day);
  return rows;
}

function eventIdentity(event) {
  return boundedText(event.identity_id || event.device_id, 128);
}

function activeIdentityByDay(devices, usageEvents, behaviorEvents, window) {
  const activeByDay = new Map(dayRows(window).map((day) => [day, new Set()]));
  for (const event of usageEvents) {
    const day = statsDay(event.occurred_at);
    const bucket = activeByDay.get(day);
    if (bucket && event.device_id) bucket.add(event.device_id);
  }
  for (const event of behaviorEvents) {
    const day = statsDay(event.occurred_at);
    const bucket = activeByDay.get(day);
    const identity = eventIdentity(event);
    if (bucket && identity) bucket.add(identity);
  }
  for (const device of devices) {
    const day = statsDay(device.last_active_at || 0);
    const bucket = activeByDay.get(day);
    if (bucket && device.device_id) bucket.add(device.device_id);
  }
  return activeByDay;
}

function filteredBehaviorEvents(events, window, eventName = null) {
  return events.filter((event) => {
    if (eventName && event.event_name !== eventName) return false;
    const day = statsDay(event.occurred_at);
    return day >= window.startDay && day <= window.endDay;
  });
}

function dailyAveragePerActive(events, activeByDay, window) {
  const countByDay = new Map(dayRows(window).map((day) => [day, 0]));
  for (const event of events) {
    const day = statsDay(event.occurred_at);
    if (countByDay.has(day)) countByDay.set(day, countByDay.get(day) + 1);
  }
  const values = dayRows(window).map((day) => {
    const dau = activeByDay.get(day)?.size || 0;
    return dau ? (countByDay.get(day) || 0) / dau : 0;
  });
  return values.length ? values.reduce((sum, value) => sum + value, 0) / values.length : 0;
}

function periodActiveCount(activeByDay) {
  const identities = new Set();
  for (const bucket of activeByDay.values()) {
    for (const identity of bucket) identities.add(identity);
  }
  return identities.size;
}

function rate(part, total) {
  return total ? part / total : 0;
}

function behaviorToolType(value) {
  const type = boundedText(value, 32);
  return ["skill", "mcp", "cli", "unknown"].includes(type) ? type : "unknown";
}

export function buildBehaviorOverview(devices, usageEvents, behaviorEvents, range = "30", now = Date.now()) {
  const window = rangeWindow(range, now);
  const activeByDay = activeIdentityByDay(devices, usageEvents, behaviorEvents, window);
  const activeTotal = periodActiveCount(activeByDay);
  const taskStarted = filteredBehaviorEvents(behaviorEvents, window, "task_started");
  const taskFinished = filteredBehaviorEvents(behaviorEvents, window, "task_finished");
  const voiceStarted = filteredBehaviorEvents(behaviorEvents, window, "voice_started");
  const taskUsers = new Set(taskStarted.map(eventIdentity).filter(Boolean));
  const voiceUsers = new Set(voiceStarted.map(eventIdentity).filter(Boolean));
  const taskStatus = { success: 0, interrupted: 0, failed: 0 };
  for (const event of taskFinished) {
    if (event.status in taskStatus) taskStatus[event.status] += 1;
  }
  return {
    generated_at: now,
    range: window.range,
    active_users: activeTotal,
    metrics: {
      task_started_count: taskStarted.length,
      task_started_users: taskUsers.size,
      task_started_per_active: dailyAveragePerActive(taskStarted, activeByDay, window),
      task_success_count: taskStatus.success,
      task_interrupted_count: taskStatus.interrupted,
      task_failed_count: taskStatus.failed,
      voice_penetration_rate: rate(voiceUsers.size, activeTotal),
      voice_started_users: voiceUsers.size,
      voice_started_count: voiceStarted.length,
      voice_starts_per_voice_user: rate(voiceStarted.length, voiceUsers.size),
    },
  };
}

function sortedRankingRows(groups, countKey, activeTotal, limit = 50) {
  return [...groups.values()]
    .map((row) => ({
      ...row,
      user_count: row.users.size,
      penetration_rate: rate(row.users.size, activeTotal),
      success_rate: row.total ? row.success_count / row.total : 0,
      [countKey]: row.total,
      users: undefined,
      total: undefined,
    }))
    .sort((a, b) => b[countKey] - a[countKey] || b.user_count - a.user_count
      || String(a.name || a.tool_name || a.scene_l2).localeCompare(String(b.name || b.tool_name || b.scene_l2), "zh-CN"))
    .slice(0, limit);
}

export function buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, range = "30", type = "call", now = Date.now()) {
  const window = rangeWindow(range, now);
  const activeTotal = periodActiveCount(activeIdentityByDay(devices, usageEvents, behaviorEvents, window));
  const eventName = type === "install" ? "tool_install_completed" : "tool_call_completed";
  const countKey = type === "install" ? "install_count" : "call_count";
  const groups = new Map();
  for (const event of filteredBehaviorEvents(behaviorEvents, window, eventName)) {
    if (eventName === "tool_install_completed" && event.success !== true) continue;
    const toolKey = boundedText(event.tool_key || event.tool_name, 128) || "unknown";
    const row = groups.get(toolKey) || {
      tool_key: toolKey,
      tool_name: boundedText(event.tool_name, 128) || toolKey,
      tool_type: behaviorToolType(event.tool_type),
      total: 0,
      success_count: 0,
      users: new Set(),
    };
    row.total += 1;
    if (event.success !== false) row.success_count += 1;
    const identity = eventIdentity(event);
    if (identity) row.users.add(identity);
    groups.set(toolKey, row);
  }
  return { generated_at: now, range: window.range, type, rows: sortedRankingRows(groups, countKey, activeTotal) };
}

export function buildBehaviorSceneRanking(devices, usageEvents, behaviorEvents, range = "30", now = Date.now()) {
  const window = rangeWindow(range, now);
  const activeTotal = periodActiveCount(activeIdentityByDay(devices, usageEvents, behaviorEvents, window));
  const groups = new Map();
  for (const event of filteredBehaviorEvents(behaviorEvents, window, "scene_triggered")) {
    const sceneL1 = boundedText(event.scene_l1, 64) || "unknown";
    const sceneL2 = boundedText(event.scene_l2, 96) || "unknown";
    const key = `${sceneL1}:${sceneL2}`;
    const row = groups.get(key) || {
      scene_l1: sceneL1,
      scene_l2: sceneL2,
      total: 0,
      success_count: 0,
      users: new Set(),
    };
    row.total += 1;
    row.success_count += 1;
    const identity = eventIdentity(event);
    if (identity) row.users.add(identity);
    groups.set(key, row);
  }
  return {
    generated_at: now,
    range: window.range,
    rows: sortedRankingRows(groups, "trigger_count", activeTotal),
  };
}

export function buildBehaviorModelRanking(devices, usageEvents, behaviorEvents, range = "30", now = Date.now()) {
  const window = rangeWindow(range, now);
  const activeTotal = periodActiveCount(activeIdentityByDay(devices, usageEvents, behaviorEvents, window));
  const groups = new Map();
  for (const event of filteredBehaviorEvents(behaviorEvents, window, "model_used")) {
    const modelId = boundedText(event.model_id || event.model_name, 128);
    if (!modelId) continue;
    const providerKey = boundedText(event.provider_key, 64) || "unknown";
    const key = `${providerKey}:${modelId}`;
    const row = groups.get(key) || {
      provider_key: providerKey,
      provider_name: boundedText(event.provider_name, 128) || providerKey,
      model_id: modelId,
      model_name: boundedText(event.model_name, 128) || modelId,
      total: 0,
      success_count: 0,
      users: new Set(),
    };
    row.total += 1;
    row.success_count += 1;
    const identity = eventIdentity(event);
    if (identity) row.users.add(identity);
    groups.set(key, row);
  }
  return {
    generated_at: now,
    range: window.range,
    rows: [...groups.values()]
      .map((row) => ({
        ...row,
        user_count: row.users.size,
        penetration_rate: rate(row.users.size, activeTotal),
        use_count: row.total,
        users: undefined,
        total: undefined,
        success_count: undefined,
      }))
      .sort((a, b) => b.user_count - a.user_count || b.use_count - a.use_count
        || String(a.model_name || a.model_id).localeCompare(String(b.model_name || b.model_id), "zh-CN"))
      .slice(0, 50),
  };
}

export function buildCommercialMock(range = "30", now = Date.now()) {
  const window = rangeWindow(range, now);
  const trend = dayRows(window).map((day, index) => ({
    day: statsDayLabel(day),
    revenue: 680 + index * 38 + (index % 5) * 42,
    paid_users: 108 + Math.floor(index * 1.7),
  }));
  return {
    generated_at: now,
    range: window.range,
    mock: true,
    metrics: {
      paid_penetration_rate: 0.082,
      paid_users: 128,
      arpu: 18.6,
      arppu: 226.8,
      mrr: 29030,
      new_paid_users: 21,
      renewal_rate: 0.74,
      trial_to_paid_rate: 0.16,
    },
    revenue_trend: trend,
    plan_distribution: [
      { plan: "Pro 月付", users: 74 },
      { plan: "Pro 年付", users: 39 },
      { plan: "Team", users: 15 },
    ],
    conversion_funnel: [
      { stage: "激活", count: 1420 },
      { stage: "试用", count: 318 },
      { stage: "付费", count: 128 },
    ],
  };
}

function readCookies(req) {
  const values = {};
  for (const part of String(req.headers.cookie || "").split(";")) {
    const index = part.indexOf("=");
    if (index <= 0) continue;
    values[part.slice(0, index).trim()] = decodeURIComponent(part.slice(index + 1).trim());
  }
  return values;
}

async function readJson(req) {
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > MAX_JSON_BYTES) throw new Error("payload_too_large");
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
  } catch {
    throw new Error("invalid_json");
  }
}

function loadJson(path, fallback) {
  if (!existsSync(path)) return fallback;
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (error) {
    console.error(`[telemetry] failed to read ${path}:`, error);
    return fallback;
  }
}

function atomicWriteJson(path, value) {
  const temporary = `${path}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  renameSync(temporary, path);
}

function atomicWriteJsonLines(path, values) {
  const temporary = `${path}.tmp`;
  const content = values.length
    ? `${values.map((value) => JSON.stringify(value)).join("\n")}\n`
    : "";
  writeFileSync(temporary, content, { mode: 0o600 });
  renameSync(temporary, path);
}

export class TelemetryStore {
  constructor(dataDir, pepper, options = {}) {
    this.dataDir = dataDir;
    this.pepper = pepper;
    this.devicesPath = join(dataDir, "devices.json");
    this.eventsPath = join(dataDir, "usage-events.jsonl");
    this.behaviorEventsPath = join(dataDir, "behavior-events.jsonl");
    mkdirSync(dataDir, { recursive: true, mode: 0o700 });
    this.devices = new Map();
    this.devicesByHardwareHash = new Map();
    this.events = [];
    this.eventIds = new Set();
    this.behaviorEvents = [];
    this.behaviorEventIds = new Set();
    this.saveTimer = null;
    this.lastEventCompactionAt = 0;
    this.eventRateBuckets = new Map();
    this.globalEventRateBucket = null;
    this.maxDevices = boundedInteger(options.maxDevices, DEFAULT_MAX_DEVICES, 1, 1_000_000);
    this.maxEvents = boundedInteger(options.maxEvents, DEFAULT_MAX_EVENTS, 100, 5_000_000);
    this.eventCompactionTarget = Math.max(1, Math.floor(this.maxEvents * 0.9));
    this.eventRetentionMs = boundedInteger(
      options.eventRetentionMs,
      DEFAULT_EVENT_RETENTION_MS,
      30 * DAY_MS,
      366 * DAY_MS,
    );
    this.eventRatePerMinute = boundedInteger(
      options.eventRatePerMinute,
      DEFAULT_EVENT_RATE_PER_MINUTE,
      MAX_EVENTS_PER_BATCH,
      100_000,
    );
    this.globalEventRatePerMinute = boundedInteger(
      options.globalEventRatePerMinute,
      DEFAULT_GLOBAL_EVENT_RATE_PER_MINUTE,
      MAX_EVENTS_PER_BATCH,
      1_000_000,
    );

    const saved = loadJson(this.devicesPath, { devices: [] });
    for (const device of Array.isArray(saved.devices) ? saved.devices : []) {
      if (!device?.device_id || !device?.hardware_hash || !device?.device_token) continue;
      const normalized = {
        ...device,
        turns: 0,
        input_tokens: 0,
        output_tokens: 0,
        errors: 0,
      };
      this.devices.set(device.device_id, normalized);
      this.devicesByHardwareHash.set(device.hardware_hash, normalized);
    }
    this.loadEvents();
    this.loadBehaviorEvents();
  }

  loadEvents() {
    if (!existsSync(this.eventsPath)) return;
    const lines = readFileSync(this.eventsPath, "utf8").split("\n");
    let rewriteNeeded = false;
    for (const line of lines) {
      if (!line.trim()) continue;
      try {
        const event = JSON.parse(line);
        if (!event.event_id || this.eventIds.has(event.event_id)) {
          rewriteNeeded = true;
          continue;
        }
        const device = this.devices.get(event.device_id);
        if (!device) {
          rewriteNeeded = true;
          continue;
        }
        this.eventIds.add(event.event_id);
        this.events.push(event);
      } catch (error) {
        rewriteNeeded = true;
        console.error("[telemetry] skipped malformed usage event:", error);
      }
    }
    this.compactEvents(Date.now(), rewriteNeeded);
    this.rebuildUsageCounters();
  }

  loadBehaviorEvents() {
    if (!existsSync(this.behaviorEventsPath)) return;
    const lines = readFileSync(this.behaviorEventsPath, "utf8").split("\n");
    let rewriteNeeded = false;
    for (const line of lines) {
      if (!line.trim()) continue;
      try {
        const event = JSON.parse(line);
        if (!event.event_id || this.behaviorEventIds.has(event.event_id)
          || !BEHAVIOR_EVENT_NAMES.has(event.event_name)) {
          rewriteNeeded = true;
          continue;
        }
        this.behaviorEventIds.add(event.event_id);
        this.behaviorEvents.push(event);
      } catch (error) {
        rewriteNeeded = true;
        console.error("[telemetry] skipped malformed behavior event:", error);
      }
    }
    this.compactBehaviorEvents(Date.now(), rewriteNeeded);
  }

  hardwareHash(claim) {
    return crypto.createHmac("sha256", this.pepper).update(claim).digest("hex");
  }

  registrationSecretHash(secret) {
    return crypto.createHmac("sha256", this.pepper)
      .update(`registration-secret:${secret}`)
      .digest("hex");
  }

  saveSoon() {
    if (this.saveTimer) return;
    this.saveTimer = setTimeout(() => {
      this.saveTimer = null;
      this.saveNow();
    }, 1000);
    this.saveTimer.unref?.();
  }

  saveNow() {
    atomicWriteJson(this.devicesPath, { version: 2, devices: [...this.devices.values()] });
  }

  updateRegion(device, region, now = Date.now()) {
    if (region !== undefined) {
      if (region) device.region = region;
      device.region_updated_at = now;
    }
  }

  regionNeedsRefresh(device, now = Date.now()) {
    return now - Number(device.region_updated_at || 0) >= REGION_REFRESH_MS;
  }

  register(input, region) {
    const claim = boundedText(input.hardware_claim, 512);
    if (claim.length < 8) throw new Error("invalid_hardware_claim");
    const registrationSecret = boundedText(input.registration_secret, 256);
    if (registrationSecret.length < 32) throw new Error("invalid_registration_secret");
    const hardwareHash = this.hardwareHash(claim);
    const registrationSecretHash = this.registrationSecretHash(registrationSecret);
    let device = this.devicesByHardwareHash.get(hardwareHash);
    const now = Date.now();
    if (!device) {
      if (this.devices.size >= this.maxDevices) {
        throw new TelemetryHttpError(503, "device_capacity_reached");
      }
      device = {
        device_id: randomId("dev_", 15),
        device_token: randomId("dt_", 24),
        hardware_hash: hardwareHash,
        registration_secret_hash: registrationSecretHash,
        identity_quality: boundedText(input.identity_quality, 32) || "installation_only",
        hardware_source: boundedText(input.hardware_source, 48) || "unknown",
        first_seen_at: now,
        last_seen_at: now,
        last_active_at: null,
        app_version: boundedText(input.app_version, 32),
        platform: boundedText(input.platform, 32),
        arch: boundedText(input.arch, 32),
        state: "online",
        region: region || null,
        region_updated_at: now,
        turns: 0,
        input_tokens: 0,
        output_tokens: 0,
        errors: 0,
      };
      this.devices.set(device.device_id, device);
      this.devicesByHardwareHash.set(device.hardware_hash, device);
      this.saveNow();
    } else {
      if (!device.registration_secret_hash
        || !safeEqual(registrationSecretHash, device.registration_secret_hash)) {
        throw new TelemetryHttpError(409, "device_already_registered");
      }
      device.last_seen_at = now;
      device.app_version = boundedText(input.app_version, 32) || device.app_version;
      device.platform = boundedText(input.platform, 32) || device.platform;
      device.arch = boundedText(input.arch, 32) || device.arch;
      this.updateRegion(device, region, now);
      this.saveSoon();
    }
    return { device_id: device.device_id, device_token: device.device_token };
  }

  authenticate(deviceId, token) {
    const device = this.devices.get(boundedText(deviceId, 96));
    if (!device || !safeEqual(token, device.device_token)) return null;
    return device;
  }

  heartbeat(device, input, region) {
    const now = Date.now();
    device.last_seen_at = now;
    device.state = normalizedState(input.state);
    device.app_version = boundedText(input.app_version, 32) || device.app_version;
    device.platform = boundedText(input.platform, 32) || device.platform;
    device.arch = boundedText(input.arch, 32) || device.arch;
    const activity = Number(input.last_activity_at);
    if (Number.isFinite(activity) && activity > 0 && activity <= now + 60_000) {
      device.last_active_at = Math.max(Number(device.last_active_at || 0), activity);
    }
    this.updateRegion(device, region, now);
    this.bindRegistrationSecret(device, input.registration_secret);
    this.saveSoon();
  }

  bindRegistrationSecret(device, secret) {
    if (device.registration_secret_hash) return;
    const registrationSecret = boundedText(secret, 256);
    if (registrationSecret.length < 32) return;
    device.registration_secret_hash = this.registrationSecretHash(registrationSecret);
  }

  applyUsage(device, event) {
    device.turns += 1;
    device.input_tokens += event.input_tokens;
    device.output_tokens += event.output_tokens;
    if (!event.success) device.errors += 1;
    device.last_active_at = Math.max(Number(device.last_active_at || 0), event.occurred_at);
  }

  rebuildUsageCounters() {
    for (const device of this.devices.values()) {
      device.turns = 0;
      device.input_tokens = 0;
      device.output_tokens = 0;
      device.errors = 0;
    }
    for (const event of this.events) {
      const device = this.devices.get(event.device_id);
      if (device) this.applyUsage(device, event);
    }
  }

  compactEvents(now = Date.now(), force = false) {
    const cutoff = now - this.eventRetentionMs;
    let retained = this.events.filter((event) => Number(event.received_at || event.occurred_at) >= cutoff);
    if (retained.length > this.maxEvents) retained = retained.slice(-this.eventCompactionTarget);
    const changed = force || retained.length !== this.events.length;
    this.events = retained;
    this.eventIds = new Set(retained.map((event) => event.event_id));
    this.lastEventCompactionAt = now;
    if (changed) atomicWriteJsonLines(this.eventsPath, retained);
    return changed;
  }

  compactExpiredEventsIfDue(now = Date.now()) {
    if (now - this.lastEventCompactionAt < 60 * 60_000) return false;
    const usageChanged = this.compactEvents(now);
    this.compactBehaviorEvents(now);
    return usageChanged;
  }

  compactBehaviorEvents(now = Date.now(), force = false) {
    const cutoff = now - this.eventRetentionMs;
    let retained = this.behaviorEvents.filter((event) => Number(event.received_at || event.occurred_at) >= cutoff);
    if (retained.length > this.maxEvents) retained = retained.slice(-this.eventCompactionTarget);
    const changed = force || retained.length !== this.behaviorEvents.length;
    this.behaviorEvents = retained;
    this.behaviorEventIds = new Set(retained.map((event) => event.event_id));
    if (changed) atomicWriteJsonLines(this.behaviorEventsPath, retained);
    return changed;
  }

  eventRateLimited(deviceId, count, now = Date.now()) {
    const bucket = this.eventRateBuckets.get(deviceId);
    let deviceLimited;
    if (!bucket || now - bucket.started_at >= RATE_LIMIT_WINDOW_MS) {
      this.eventRateBuckets.set(deviceId, { started_at: now, count });
      deviceLimited = count > this.eventRatePerMinute;
    } else {
      bucket.count += count;
      deviceLimited = bucket.count > this.eventRatePerMinute;
    }
    if (deviceLimited) return true;
    if (!this.globalEventRateBucket
      || now - this.globalEventRateBucket.started_at >= RATE_LIMIT_WINDOW_MS) {
      this.globalEventRateBucket = { started_at: now, count };
    } else {
      this.globalEventRateBucket.count += count;
    }
    return this.globalEventRateBucket.count > this.globalEventRatePerMinute;
  }

  appendEvents(device, inputEvents) {
    const accepted = [];
    const duplicates = [];
    const now = Date.now();
    if (this.compactExpiredEventsIfDue(now)) this.rebuildUsageCounters();
    if (this.eventRateLimited(device.device_id, inputEvents.length, now)) {
      throw new TelemetryHttpError(429, "event_rate_limited");
    }
    const batchEventIds = new Set();
    for (const raw of inputEvents.slice(0, MAX_EVENTS_PER_BATCH)) {
      const eventId = boundedText(raw?.event_id, 96);
      if (eventId.length < 12) continue;
      if (this.eventIds.has(eventId) || batchEventIds.has(eventId)) {
        duplicates.push(eventId);
        continue;
      }
      batchEventIds.add(eventId);
      let occurredAt = Number(raw.occurred_at);
      if (!Number.isFinite(occurredAt) || occurredAt <= 0 || occurredAt > now + 60_000) occurredAt = now;
      const event = {
        event_id: eventId,
        device_id: device.device_id,
        occurred_at: occurredAt,
        received_at: now,
        input_tokens: boundedCount(raw.input_tokens),
        output_tokens: boundedCount(raw.output_tokens),
        success: raw.success !== false,
      };
      accepted.push(event);
    }
    if (accepted.length) {
      appendFileSync(this.eventsPath, accepted.map((event) => JSON.stringify(event)).join("\n") + "\n", { mode: 0o600 });
      for (const event of accepted) {
        this.eventIds.add(event.event_id);
        this.events.push(event);
        this.applyUsage(device, event);
      }
      if (this.events.length > this.maxEvents) {
        this.compactEvents(now, true);
        this.rebuildUsageCounters();
      }
      device.last_seen_at = now;
      this.saveSoon();
    }
    return { accepted: accepted.map((event) => event.event_id), duplicates };
  }

  normalizedOccurredAt(value, now = Date.now()) {
    const occurredAt = Number(value);
    if (!Number.isFinite(occurredAt) || occurredAt <= 0) return now;
    if (occurredAt > now + MAX_CLIENT_FUTURE_SKEW_MS) return now;
    if (occurredAt < now - this.eventRetentionMs) return now;
    return occurredAt;
  }

  appendBehaviorEvents(device, inputEvents) {
    const accepted = [];
    const duplicates = [];
    const rejected = [];
    const now = Date.now();
    this.compactExpiredEventsIfDue(now);
    if (this.eventRateLimited(device.device_id, inputEvents.length, now)) {
      throw new TelemetryHttpError(429, "event_rate_limited");
    }
    const batchEventIds = new Set();
    for (const raw of inputEvents.slice(0, MAX_EVENTS_PER_BATCH)) {
      const eventId = boundedText(raw?.event_id, 96);
      const eventName = boundedText(raw?.event_name, 64);
      if (eventId.length < 12 || !BEHAVIOR_EVENT_NAMES.has(eventName)) {
        rejected.push(eventId || eventName || "invalid_event");
        continue;
      }
      if (this.behaviorEventIds.has(eventId) || batchEventIds.has(eventId)) {
        duplicates.push(eventId);
        continue;
      }
      const identityType = boundedText(raw.identity_type, 16) || "device";
      const identityId = boundedText(raw.identity_id, 128) || device.device_id;
      if (identityType !== "device" || identityId !== device.device_id) {
        rejected.push(eventId);
        continue;
      }
      batchEventIds.add(eventId);
      const event = {
        event_id: eventId,
        event_name: eventName,
        occurred_at: this.normalizedOccurredAt(raw.occurred_at, now),
        received_at: now,
        app_version: boundedText(raw.app_version, 48) || boundedText(device.app_version, 48),
        platform: boundedText(raw.platform, 32) || boundedText(device.platform, 32),
        identity_id: device.device_id,
        identity_type: "device",
        session_id: boundedText(raw.session_id, 128),
        turn_id: boundedText(raw.turn_id, 128),
        input_type: boundedText(raw.input_type, 32),
        status: ["success", "failed", "interrupted"].includes(raw.status) ? raw.status : undefined,
        stage: boundedText(raw.stage, 32),
        tool_key: boundedText(raw.tool_key, 128),
        tool_name: boundedText(raw.tool_name, 128),
        tool_type: behaviorToolType(raw.tool_type),
        success: raw.success !== false,
        scene_l1: boundedText(raw.scene_l1, 64),
        scene_l2: boundedText(raw.scene_l2, 96),
        provider_key: boundedText(raw.provider_key, 64),
        provider_name: boundedText(raw.provider_name, 128),
        model_id: boundedText(raw.model_id, 128),
        model_name: boundedText(raw.model_name, 128),
      };
      accepted.push(event);
    }
    if (accepted.length) {
      appendFileSync(this.behaviorEventsPath, accepted.map((event) => JSON.stringify(event)).join("\n") + "\n", { mode: 0o600 });
      for (const event of accepted) {
        this.behaviorEventIds.add(event.event_id);
        this.behaviorEvents.push(event);
      }
      if (this.behaviorEvents.length > this.maxEvents) this.compactBehaviorEvents(now, true);
      device.last_seen_at = now;
      this.saveSoon();
    }
    return { accepted: accepted.map((event) => event.event_id), duplicates, rejected };
  }

  overview() {
    if (this.compactExpiredEventsIfDue()) this.rebuildUsageCounters();
    return buildStatsOverview([...this.devices.values()], this.events);
  }

  deviceList() {
    if (this.compactExpiredEventsIfDue()) this.rebuildUsageCounters();
    return buildDeviceList([...this.devices.values()], this.events);
  }

  behaviorOverview(range) {
    if (this.compactExpiredEventsIfDue()) this.rebuildUsageCounters();
    return buildBehaviorOverview([...this.devices.values()], this.events, this.behaviorEvents, range);
  }

  behaviorTools(range, type) {
    if (this.compactExpiredEventsIfDue()) this.rebuildUsageCounters();
    return buildBehaviorToolRanking([...this.devices.values()], this.events, this.behaviorEvents, range, type);
  }

  behaviorScenes(range) {
    if (this.compactExpiredEventsIfDue()) this.rebuildUsageCounters();
    return buildBehaviorSceneRanking([...this.devices.values()], this.events, this.behaviorEvents, range);
  }

  behaviorModels(range) {
    if (this.compactExpiredEventsIfDue()) this.rebuildUsageCounters();
    return buildBehaviorModelRanking([...this.devices.values()], this.events, this.behaviorEvents, range);
  }

  commercialOverview(range) {
    return buildCommercialMock(range);
  }
}

export function createTelemetryService(options = {}) {
  const enrollmentToken = String(process.env.PINVOU_TELEMETRY_ENROLLMENT_TOKEN || "");
  const devicePepper = String(process.env.PINVOU_TELEMETRY_DEVICE_PEPPER || "");
  const adminPassword = String(process.env.PINVOU_STATS_ADMIN_PASSWORD || "");
  const dataDir = process.env.PINVOU_TELEMETRY_DATA_DIR || "/var/lib/pinvou-telemetry";
  const enabled = enrollmentToken.length >= 24 && devicePepper.length >= 24 && adminPassword.length >= 12;
  const store = enabled ? new TelemetryStore(dataDir, devicePepper, {
    maxDevices: options.maxDevices ?? process.env.PINVOU_TELEMETRY_MAX_DEVICES,
    maxEvents: options.maxEvents ?? process.env.PINVOU_TELEMETRY_MAX_EVENTS,
    eventRetentionMs: options.eventRetentionMs
      ?? (Number(process.env.PINVOU_TELEMETRY_EVENT_RETENTION_DAYS) * DAY_MS || undefined),
    eventRatePerMinute: options.eventRatePerMinute
      ?? process.env.PINVOU_TELEMETRY_EVENT_RATE_PER_MINUTE,
    globalEventRatePerMinute: options.globalEventRatePerMinute
      ?? process.env.PINVOU_TELEMETRY_EVENT_RATE_GLOBAL_PER_MINUTE,
  }) : null;
  const regionResolver = options.regionResolver || createIpRegionResolver({
    ipv4Path: process.env.PINVOU_TELEMETRY_IP_DB_V4 || join(dataDir, "ip2region_v4.xdb"),
    ipv6Path: process.env.PINVOU_TELEMETRY_IP_DB_V6 || join(dataDir, "ip2region_v6.xdb"),
  });
  const statsPage = options.statsPage || join(__dirname, "web", "stats.html");
  const adminSessions = new Map();
  const loginAttempts = new Map();
  const registrationAttempts = new Map();
  const globalRegistrationAttempts = new Map();
  const registrationRatePerIp = boundedInteger(
    options.registrationRatePerIp ?? process.env.PINVOU_TELEMETRY_REGISTER_RATE_PER_IP,
    10,
    1,
    1_000,
  );
  const registrationRateGlobal = boundedInteger(
    options.registrationRateGlobal ?? process.env.PINVOU_TELEMETRY_REGISTER_RATE_GLOBAL,
    100,
    1,
    100_000,
  );

  function bearer(req) {
    const value = String(req.headers.authorization || "");
    return value.startsWith("Bearer ") ? value.slice(7) : "";
  }

  function adminAuthorized(req) {
    const token = readCookies(req).pinvou_stats_session;
    const expiresAt = token ? adminSessions.get(token) : null;
    if (!expiresAt || expiresAt <= Date.now()) {
      if (token) adminSessions.delete(token);
      return false;
    }
    return true;
  }

  function rateLimited(buckets, key, limit) {
    const now = Date.now();
    const bucket = buckets.get(key);
    if (!bucket || now - bucket.started_at >= 60_000) {
      if (!bucket && buckets.size >= MAX_RATE_LIMIT_BUCKETS) {
        for (const [bucketKey, value] of buckets) {
          if (now - value.started_at >= RATE_LIMIT_WINDOW_MS) buckets.delete(bucketKey);
        }
        while (buckets.size >= MAX_RATE_LIMIT_BUCKETS) {
          buckets.delete(buckets.keys().next().value);
        }
      }
      buckets.set(key, { started_at: now, count: 1 });
      return false;
    }
    bucket.count += 1;
    return bucket.count > limit;
  }

  async function handleHttp(req, res, routePath, context = {}) {
    const isTelemetry = routePath === TELEMETRY_PREFIX || routePath.startsWith(`${TELEMETRY_PREFIX}/`);
    const isStats = routePath === STATS_PREFIX || routePath.startsWith(`${STATS_PREFIX}/`);
    if (!isTelemetry && !isStats) return false;
    if (!enabled) {
      json(res, 503, { error: "telemetry_not_configured" });
      return true;
    }

    try {
      const requestIp = boundedText(context.clientIp || req.socket?.remoteAddress || "unknown", 96);
      if (req.method === "GET" && routePath === `${TELEMETRY_PREFIX}/healthz`) {
        json(res, 200, { ok: true });
        return true;
      }
      if (req.method === "POST" && routePath === `${TELEMETRY_PREFIX}/v1/register`) {
        if (rateLimited(registrationAttempts, requestIp, registrationRatePerIp)
          || rateLimited(globalRegistrationAttempts, "global", registrationRateGlobal)) {
          json(res, 429, { error: "rate_limited" });
          return true;
        }
        const input = await readJson(req);
        if (!safeEqual(input.enrollment_token, enrollmentToken)) {
          json(res, 401, { error: "invalid_enrollment_token" });
          return true;
        }
        const region = await regionResolver.lookup(requestIp);
        json(res, 200, store.register(input, region));
        return true;
      }
      if (req.method === "POST" && routePath === `${TELEMETRY_PREFIX}/v1/heartbeat`) {
        const input = await readJson(req);
        const device = store.authenticate(input.device_id, bearer(req));
        if (!device) {
          json(res, 401, { error: "invalid_device_token" });
          return true;
        }
        const region = store.regionNeedsRefresh(device)
          ? await regionResolver.lookup(requestIp)
          : undefined;
        store.heartbeat(device, input, region);
        json(res, 200, { ok: true, server_time: Date.now() });
        return true;
      }
      if (req.method === "POST" && routePath === `${TELEMETRY_PREFIX}/v1/events`) {
        const input = await readJson(req);
        const device = store.authenticate(input.device_id, bearer(req));
        if (!device) {
          json(res, 401, { error: "invalid_device_token" });
          return true;
        }
        const events = Array.isArray(input.events) ? input.events : [];
        if (events.length > MAX_EVENTS_PER_BATCH) {
          json(res, 413, { error: "too_many_events" });
          return true;
        }
        json(res, 200, store.appendEvents(device, events));
        return true;
      }
      if (req.method === "POST" && routePath === `${STATS_PREFIX}/api/behavior/events`) {
        const input = await readJson(req);
        const device = store.authenticate(input.device_id, bearer(req));
        if (!device) {
          json(res, 401, { error: "invalid_device_token" });
          return true;
        }
        const events = Array.isArray(input.events) ? input.events : [];
        if (events.length > MAX_EVENTS_PER_BATCH) {
          json(res, 413, { error: "too_many_events" });
          return true;
        }
        json(res, 200, store.appendBehaviorEvents(device, events));
        return true;
      }
      if (req.method === "GET" && (routePath === STATS_PREFIX || routePath === `${STATS_PREFIX}/`)) {
        html(res, 200, readFileSync(statsPage, "utf8"));
        return true;
      }
      if (req.method === "POST" && routePath === `${STATS_PREFIX}/api/login`) {
        if (rateLimited(loginAttempts, requestIp, 30)) {
          json(res, 429, { error: "rate_limited" });
          return true;
        }
        const input = await readJson(req);
        if (!safeEqual(input.password, adminPassword)) {
          json(res, 401, { error: "invalid_credentials" });
          return true;
        }
        const session = randomId("as_", 32);
        adminSessions.set(session, Date.now() + ADMIN_SESSION_MS);
        json(res, 200, { ok: true }, {
          "set-cookie": `pinvou_stats_session=${encodeURIComponent(session)}; Path=${STATS_PREFIX}; Max-Age=${ADMIN_SESSION_MS / 1000}; HttpOnly; Secure; SameSite=Strict`,
        });
        return true;
      }
      if (req.method === "POST" && routePath === `${STATS_PREFIX}/api/logout`) {
        const session = readCookies(req).pinvou_stats_session;
        if (session) adminSessions.delete(session);
        json(res, 200, { ok: true }, {
          "set-cookie": `pinvou_stats_session=; Path=${STATS_PREFIX}; Max-Age=0; HttpOnly; Secure; SameSite=Strict`,
        });
        return true;
      }
      if (routePath.startsWith(`${STATS_PREFIX}/api/`) && !adminAuthorized(req)) {
        json(res, 401, { error: "unauthorized" });
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/overview`) {
        json(res, 200, store.overview());
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/devices`) {
        json(res, 200, { devices: store.deviceList() });
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/behavior/overview`) {
        json(res, 200, store.behaviorOverview(new URL(req.url, "http://localhost").searchParams.get("range")));
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/behavior/tools`) {
        const params = new URL(req.url, "http://localhost").searchParams;
        json(res, 200, store.behaviorTools(params.get("range"), params.get("type")));
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/behavior/scenes`) {
        json(res, 200, store.behaviorScenes(new URL(req.url, "http://localhost").searchParams.get("range")));
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/behavior/models`) {
        json(res, 200, store.behaviorModels(new URL(req.url, "http://localhost").searchParams.get("range")));
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/commercial/overview`) {
        json(res, 200, store.commercialOverview(new URL(req.url, "http://localhost").searchParams.get("range")));
        return true;
      }
      json(res, 404, { error: "not_found" });
      return true;
    } catch (error) {
      const code = Number(error?.status)
        || (error?.message === "payload_too_large" ? 413 : 400);
      if (!(error instanceof TelemetryHttpError)) {
        console.error("[telemetry] request failed:", error);
      }
      json(res, code, { error: error?.message || "bad_request" });
      return true;
    }
  }

  if (!enabled) {
    console.warn("[telemetry] disabled: configure enrollment token, device pepper and admin password");
  } else {
    console.log(`[telemetry] ready data_dir=${dataDir} region_lookup=${regionResolver.enabled ? "enabled" : "disabled"}`);
  }
  return { enabled, handleHttp };
}
