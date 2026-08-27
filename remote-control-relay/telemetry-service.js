import crypto from "node:crypto";
import { isIP } from "node:net";
import {
  appendFileSync,
  chmodSync,
  existsSync,
  lstatSync,
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
import {
  BEHAVIOR_EVENT_NAMES,
  BEHAVIOR_STORED_FIELDS,
  validateBehaviorEventShape,
} from "./behavior-telemetry-schema.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const TELEMETRY_PREFIX = "/pinvou3/telemetry";
const STATS_PREFIX = "/pinvou3/stats";
const MAX_JSON_BYTES = 512 * 1024;
const MAX_USAGE_EVENTS_PER_BATCH = 200;
const MAX_BEHAVIOR_EVENTS_PER_BATCH = 50;
const LEGACY_BEHAVIOR_EVENT_NAMES = new Set([
  "task_started",
  "task_finished",
  "voice_started",
  "tool_call_completed",
  "tool_install_completed",
  "scene_triggered",
  "model_used",
]);
const STORED_BEHAVIOR_EVENT_NAMES = new Set([
  ...BEHAVIOR_EVENT_NAMES,
  ...LEGACY_BEHAVIOR_EVENT_NAMES,
]);
const TELEMETRY_ENVIRONMENTS = new Set(["production", "staging", "development", "test"]);
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
const DELETION_RESULT_RETENTION_MS = 30 * DAY_MS;

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

function telemetryEnvironment(value) {
  const environment = boundedText(value, 32).toLowerCase();
  return TELEMETRY_ENVIRONMENTS.has(environment) ? environment : "production";
}

function clockSkewBucket(occurredAt, receivedAt) {
  if (!Number.isSafeInteger(occurredAt) || !Number.isSafeInteger(receivedAt)) return "unknown";
  if (occurredAt > receivedAt + MAX_CLIENT_FUTURE_SKEW_MS) return "future_5m_plus";
  if (occurredAt < receivedAt - 60 * 60_000) return "past_1h_plus";
  return "normal";
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

function isCoreUserTask(event, eventName) {
  return event?.event_name === eventName
    && event.initiator_type === "user"
    && ["assistant", "code", "team"].includes(event.mode);
}

function activeIdentityByDay(_devices, _usageEvents, behaviorEvents, window) {
  const activeByDay = new Map(dayRows(window).map((day) => [day, new Set()]));
  for (const event of behaviorEvents) {
    if (!isCoreUserTask(event, "task_submitted")) continue;
    const day = statsDay(event.occurred_at);
    const bucket = activeByDay.get(day);
    const identity = eventIdentity(event);
    if (bucket && identity) bucket.add(identity);
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

function perActiveInstallationDay(events, activeByDay) {
  const activeInstallationDays = [...activeByDay.values()]
    .reduce((sum, identities) => sum + identities.size, 0);
  return {
    active_installation_days: activeInstallationDays,
    value: rate(events.length, activeInstallationDays),
  };
}

function uniqueAttempts(events, eventName) {
  const attempts = new Map();
  for (const event of events) {
    if (event.event_name !== eventName) continue;
    const key = boundedText(event.attempt_id || event.turn_id || event.event_id, 128);
    if (key && !attempts.has(key)) attempts.set(key, event);
  }
  return [...attempts.values()];
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

function successfulActiveIdentities(behaviorEvents, window) {
  const windowEvents = filteredBehaviorEvents(behaviorEvents, window);
  return new Set(windowEvents
    .filter((event) => isCoreUserTask(event, "task_finished") && event.status === "success")
    .map(eventIdentity)
    .filter(Boolean));
}

function intersectionSize(values, eligible) {
  let count = 0;
  for (const value of values) if (eligible.has(value)) count += 1;
  return count;
}

const EXPLICIT_VALUE_WINDOW_MS = 30 * 60_000;
const MIN_RELIABLE_SAMPLE = 30;
const MIN_PRIVACY_GROUP_SUBJECTS = 20;

function reliableRate(part, total) {
  return Number(total) >= MIN_RELIABLE_SAMPLE ? rate(part, total) : null;
}

const deployedRegistryPath = join(
  __dirname,
  "docs",
  "schemas",
  "behavior-telemetry-registries.json",
);
const behaviorRegistries = JSON.parse(readFileSync(
  existsSync(deployedRegistryPath)
    ? deployedRegistryPath
    : join(__dirname, "..", "docs", "schemas", "behavior-telemetry-registries.json"),
  "utf8",
));
const CONTROLLED_TOOL_KEYS = new Set(behaviorRegistries.tools);
const CONTROLLED_PROVIDER_KEYS = new Set(behaviorRegistries.providers);
const CONTROLLED_MODEL_KEYS = new Set(behaviorRegistries.models);

function controlledKey(value, registry, fallback = "unknown") {
  const key = boundedText(value, 128).toLowerCase();
  return registry.has(key) ? key : fallback;
}

function legacyToolKey(value) {
  const name = boundedText(value, 128).toLowerCase();
  if (["bash", "exec_shell", "task_shell_start"].includes(name)) return "shell";
  if (["read_file"].includes(name)) return "read_file";
  if (["write_file", "apply_patch"].includes(name)) return "edit_file";
  if (["glob", "rg", "search_files", "search"].includes(name)) return "search_files";
  if (name.startsWith("mcp_") || name.includes("-mcp")) return "mcp_custom";
  return "custom";
}

function legacyProviderKey(value) {
  const name = boundedText(value, 128).toLowerCase();
  if (name.includes("grok") || name.includes("xai")) return "xai";
  if (name.includes("ollama") || name.includes("vllm") || name === "local") return "local";
  for (const key of CONTROLLED_PROVIDER_KEYS) {
    if (!['custom', 'unknown'].includes(key) && name.includes(key)) return key;
  }
  return name ? "custom" : "unknown";
}

function legacyModelKey(value) {
  const name = boundedText(value, 128).toLowerCase().replaceAll("-", "_");
  const checks = [
    ["claude_opus", ["claude", "opus"]], ["claude_sonnet", ["claude", "sonnet"]],
    ["claude_haiku", ["claude", "haiku"]], ["deepseek_reasoner", ["deepseek", "reason"]],
    ["deepseek_chat", ["deepseek", "chat"]], ["gpt_5", ["gpt_5"]], ["gpt_4", ["gpt_4"]],
  ];
  for (const [key, parts] of checks) if (parts.every(part => name.includes(part))) return key;
  for (const key of CONTROLLED_MODEL_KEYS) {
    if (!['custom', 'unknown'].includes(key) && name.includes(key)) return key;
  }
  return name ? "custom" : "unknown";
}

function sanitizeStoredBehaviorEvent(raw) {
  const event = {};
  for (const [field, value] of Object.entries(raw || {})) {
    if (BEHAVIOR_STORED_FIELDS.has(field)) event[field] = value;
  }
  if (raw?.tool_key !== undefined || raw?.tool_name !== undefined) {
    event.tool_key = raw.tool_key !== undefined
      ? controlledKey(raw.tool_key, CONTROLLED_TOOL_KEYS, "custom")
      : legacyToolKey(raw.tool_name);
  }
  if (raw?.provider_key !== undefined || raw?.provider_name !== undefined) {
    event.provider_key = raw.provider_key !== undefined
      ? controlledKey(raw.provider_key, CONTROLLED_PROVIDER_KEYS, "custom")
      : legacyProviderKey(raw.provider_name);
  }
  if (raw?.model_key !== undefined || raw?.model_id !== undefined || raw?.model_name !== undefined) {
    event.model_key = raw.model_key !== undefined
      ? controlledKey(raw.model_key, CONTROLLED_MODEL_KEYS, "custom")
      : legacyModelKey(raw.model_id || raw.model_name);
  }
  if (raw?.from_model_key !== undefined) {
    event.from_model_key = controlledKey(raw.from_model_key, CONTROLLED_MODEL_KEYS, "custom");
  }
  if (raw?.to_model_key !== undefined) {
    event.to_model_key = controlledKey(raw.to_model_key, CONTROLLED_MODEL_KEYS, "custom");
  }
  return event;
}

function percentile(values, quantile) {
  const sorted = values.map(Number).filter(Number.isFinite).sort((left, right) => left - right);
  if (!sorted.length) return null;
  return sorted[Math.max(0, Math.ceil(sorted.length * quantile) - 1)];
}

function sampleConfidence(sampleSize) {
  return Number(sampleSize) >= MIN_RELIABLE_SAMPLE ? "sufficient" : "low";
}

function contributionLimit(eventName) {
  if (String(eventName).startsWith("tool_call_")) return 500;
  return 100;
}

function clipDailySubjectContributions(events) {
  const counts = new Map();
  const subjects = new Set();
  const clippedSubjects = new Set();
  const retained = [];
  for (const event of events) {
    const identity = eventIdentity(event);
    if (!identity) {
      retained.push(event);
      continue;
    }
    subjects.add(identity);
    const key = `${identity}:${statsDay(event.occurred_at)}:${event.event_name}`;
    const count = counts.get(key) || 0;
    if (count >= contributionLimit(event.event_name)) {
      clippedSubjects.add(identity);
      continue;
    }
    counts.set(key, count + 1);
    retained.push(event);
  }
  return {
    events: retained,
    subject_count: subjects.size,
    clipped_subject_count: clippedSubjects.size,
    clipped_subject_rate: rate(clippedSubjects.size, subjects.size),
  };
}

function retentionMetric(usageEvents, behaviorEvents, horizon, now) {
  const today = statsDay(now);
  const cohorts = new Map();
  for (const event of behaviorEvents) {
    if (event.event_name !== "analytics_cohort_started") continue;
    const identity = eventIdentity(event);
    const day = statsDay(event.occurred_at);
    if (!identity || day > today - horizon) continue;
    if (!cohorts.has(identity) || day < cohorts.get(identity)) cohorts.set(identity, day);
  }
  const activity = new Map();
  const addActivity = (identity, occurredAt) => {
    if (!identity) return;
    const bucket = activity.get(identity) || new Set();
    bucket.add(statsDay(occurredAt));
    activity.set(identity, bucket);
  };
  for (const event of behaviorEvents) {
    if (event.event_name !== "task_finished"
      || event.status !== "success"
      || event.initiator_type !== "user"
      || !["assistant", "code", "team"].includes(event.mode)) continue;
    addActivity(eventIdentity(event), event.occurred_at);
  }
  let retained = 0;
  for (const [identity, cohortDay] of cohorts) {
    if (activity.get(identity)?.has(cohortDay + horizon)) retained += 1;
  }
  return {
    horizon_days: horizon,
    eligible_installations: cohorts.size,
    retained_installations: retained,
    retention_rate: reliableRate(retained, cohorts.size),
    confidence: sampleConfidence(cohorts.size),
  };
}

function buildVersionQuality(windowEvents) {
  const groups = new Map();
  for (const event of windowEvents) {
    if (!["task_submitted", "task_admitted", "task_first_output", "task_finished"].includes(event.event_name)) continue;
    const version = boundedText(event.app_version, 64) || "unknown";
    const platform = boundedText(event.platform, 32) || "unknown";
    const arch = boundedText(event.arch, 32) || "unknown";
    const key = `${version}:${platform}:${arch}`;
    const row = groups.get(key) || {
      app_version: version,
      platform,
      arch,
      submitted: new Set(),
      admitted: new Set(),
      finished: new Set(),
      success: new Set(),
      cancelled: new Set(),
      first_output_ms: [],
      total_duration_ms: [],
      errors: new Map(),
      identities: new Set(),
    };
    const attempt = boundedText(event.attempt_id, 128);
    const identity = eventIdentity(event);
    if (identity) row.identities.add(identity);
    if (attempt) {
      if (event.event_name === "task_submitted") row.submitted.add(attempt);
      if (event.event_name === "task_admitted") row.admitted.add(attempt);
      if (event.event_name === "task_first_output") {
        row.first_output_ms.push(event.duration_ms);
      }
      if (event.event_name === "task_finished") {
        row.finished.add(attempt);
        row.total_duration_ms.push(event.duration_ms);
        if (event.status === "success") row.success.add(attempt);
        if (event.status === "cancelled") row.cancelled.add(attempt);
        if (event.error_category) {
          row.errors.set(event.error_category, (row.errors.get(event.error_category) || 0) + 1);
        }
      }
    }
    groups.set(key, row);
  }
  return [...groups.values()].filter((row) => row.identities.size >= MIN_PRIVACY_GROUP_SUBJECTS).map((row) => ({
    app_version: row.app_version,
    platform: row.platform,
    arch: row.arch,
    sample_size: row.submitted.size,
    installation_count: row.identities.size,
    confidence: sampleConfidence(row.submitted.size),
    admission_rate: reliableRate(row.admitted.size, row.submitted.size),
    success_rate: reliableRate(row.success.size, row.finished.size),
    cancellation_rate: reliableRate(row.cancelled.size, row.finished.size),
    first_output_p50_ms: percentile(row.first_output_ms, 0.5),
    first_output_p90_ms: percentile(row.first_output_ms, 0.9),
    total_duration_p50_ms: percentile(row.total_duration_ms, 0.5),
    total_duration_p90_ms: percentile(row.total_duration_ms, 0.9),
    error_categories: Object.fromEntries([...row.errors].sort((left, right) => right[1] - left[1])),
  })).sort((left, right) => right.sample_size - left.sample_size
    || left.app_version.localeCompare(right.app_version));
}

function buildVoiceFunnel(windowEvents, taskFinished) {
  const operations = eventName => new Set(windowEvents
    .filter(event => event.event_name === eventName)
    .map(event => event.operation_id).filter(Boolean));
  const entries = operations("voice_entry_clicked");
  const permissions = new Set(windowEvents
    .filter(event => event.event_name === "voice_permission_result" && event.voice_permission_status === "granted")
    .map(event => event.operation_id).filter(Boolean));
  const started = operations("voice_started");
  const recognized = operations("voice_recognition_completed");
  const submittedByOperation = new Map();
  for (const event of windowEvents) {
    if (event.event_name === "voice_message_submitted" && event.operation_id
      && !submittedByOperation.has(event.operation_id)) submittedByOperation.set(event.operation_id, event);
  }
  const submittedEvents = [...submittedByOperation.values()];
  const submittedAttempts = new Set(submittedEvents.map(event => event.attempt_id).filter(Boolean));
  const successfulAttempts = new Set(taskFinished
    .filter(event => event.status === "success")
    .map(event => event.attempt_id).filter(attempt => submittedAttempts.has(attempt)));
  const failed = operations("voice_recognition_failed");
  const cancelled = operations("voice_cancelled");
  return {
    entry_clicked: entries.size,
    permission_granted: permissions.size,
    started: started.size,
    recognition_completed: recognized.size,
    message_submitted: submittedEvents.length,
    task_succeeded: successfulAttempts.size,
    recognition_failed: failed.size,
    cancelled: cancelled.size,
    entry_to_submit_rate: reliableRate(submittedEvents.length, entries.size),
    submit_to_success_rate: reliableRate(successfulAttempts.size, submittedEvents.length),
    confidence: sampleConfidence(entries.size),
  };
}

function currentBeijingWeek(now) {
  const today = statsDay(now);
  const weekday = new Date(today * DAY_MS).getUTCDay();
  const startDay = today - ((weekday + 6) % 7);
  return { start: startDay * DAY_MS - STATS_TIMEZONE_OFFSET_MS, end: now };
}

export function buildWeeklyExplicitValueMetric(behaviorEvents, now = Date.now()) {
  const week = currentBeijingWeek(now);
  const latestFeedback = new Map();
  for (const event of behaviorEvents) {
    if (event.event_name !== "response_feedback_changed" || Number(event.occurred_at) > now) continue;
    const key = boundedText(event.feedback_id || `${event.session_id || ""}:${event.turn_id || ""}`, 256);
    const current = latestFeedback.get(key);
    if (key && (!current || Number(event.occurred_at) >= Number(current.occurred_at))) {
      latestFeedback.set(key, event);
    }
  }
  const valueSignals = behaviorEvents.filter((event) => {
    if (Number(event.occurred_at) > now) return false;
    if (["response_copied", "artifact_exported"].includes(event.event_name)) return true;
    if (event.event_name !== "response_feedback_changed") return false;
    const key = boundedText(event.feedback_id || `${event.session_id || ""}:${event.turn_id || ""}`, 256);
    return latestFeedback.get(key) === event
      && event.feedback_action === "set"
      && event.rating === "positive";
  });
  const successfulTasks = uniqueAttempts(behaviorEvents.filter((event) => (
    event.event_name === "task_finished"
      && event.status === "success"
      && event.initiator_type === "user"
      && ["assistant", "code", "team"].includes(event.mode)
      && Number(event.occurred_at) >= week.start
      && Number(event.occurred_at) <= week.end
  )), "task_finished");
  const weeklySuccessfulInstallations = new Set(successfulTasks.map(eventIdentity).filter(Boolean));
  const valuedInstallations = new Set();
  const feedbackInstallations = new Set();
  const copiedInstallations = new Set();
  const exportedInstallations = new Set();
  const negativeFeedbackInstallations = new Set();
  for (const task of successfulTasks) {
    const identity = eventIdentity(task);
    if (!identity || !task.turn_id) continue;
    const completedAt = Number(task.occurred_at);
    const matchedSignals = valueSignals.filter((signal) => (
      eventIdentity(signal) === identity
        && signal.turn_id === task.turn_id
        && Number(signal.occurred_at) >= completedAt
        && Number(signal.occurred_at) <= completedAt + EXPLICIT_VALUE_WINDOW_MS
    ));
    if (matchedSignals.length) valuedInstallations.add(identity);
    if (matchedSignals.some(signal => signal.event_name === "response_feedback_changed")) feedbackInstallations.add(identity);
    if (matchedSignals.some(signal => signal.event_name === "response_copied")) copiedInstallations.add(identity);
    if (matchedSignals.some(signal => signal.event_name === "artifact_exported")) exportedInstallations.add(identity);
    const negativeFeedback = [...latestFeedback.values()].some(signal => (
      signal.feedback_action === "set"
        && signal.rating === "negative"
        && eventIdentity(signal) === identity
        && signal.turn_id === task.turn_id
        && Number(signal.occurred_at) >= completedAt
        && Number(signal.occurred_at) <= completedAt + EXPLICIT_VALUE_WINDOW_MS
    ));
    if (negativeFeedback) negativeFeedbackInstallations.add(identity);
  }
  return {
    wevta_installations: valuedInstallations.size,
    weekly_successful_task_installations: weeklySuccessfulInstallations.size,
    explicit_value_sample_size: weeklySuccessfulInstallations.size,
    explicit_value_confidence: sampleConfidence(weeklySuccessfulInstallations.size),
    wevta_rate: reliableRate(valuedInstallations.size, weeklySuccessfulInstallations.size),
    positive_feedback_installations: feedbackInstallations.size,
    response_copied_installations: copiedInstallations.size,
    artifact_exported_installations: exportedInstallations.size,
    negative_feedback_installations: negativeFeedbackInstallations.size,
    negative_feedback_rate: reliableRate(negativeFeedbackInstallations.size, weeklySuccessfulInstallations.size),
  };
}

function buildActivationFunnel(behaviorEvents, window, now) {
  const cohorts = new Map();
  for (const event of filteredBehaviorEvents(behaviorEvents, window, "analytics_cohort_started")) {
    const identity = eventIdentity(event);
    if (!identity) continue;
    const previous = cohorts.get(identity);
    if (!previous || Number(event.occurred_at) < Number(previous.occurred_at)) cohorts.set(identity, event);
  }
  const matured = [...cohorts.entries()]
    .filter(([, event]) => Number(event.occurred_at) <= now - DAY_MS);
  const eventsByIdentity = new Map();
  for (const event of behaviorEvents) {
    const identity = eventIdentity(event);
    if (!identity) continue;
    const entries = eventsByIdentity.get(identity) || [];
    entries.push(event);
    eventsByIdentity.set(identity, entries);
  }
  const stages = {
    environment_ready: new Set(),
    task_submitted: new Set(),
    task_admitted: new Set(),
    task_first_output: new Set(),
    task_succeeded: new Set(),
  };
  const firstValueDurations = [];
  for (const [identity, cohort] of matured) {
    const startedAt = Number(cohort.occurred_at);
    const deadline = startedAt + DAY_MS;
    const events = (eventsByIdentity.get(identity) || [])
      .filter(event => Number(event.occurred_at) >= startedAt && Number(event.occurred_at) <= deadline)
      .sort((left, right) => Number(left.occurred_at) - Number(right.occurred_at));
    if (events.some(event => event.event_name === "task_environment_ready")) stages.environment_ready.add(identity);
    if (events.some(event => event.event_name === "task_submitted")) stages.task_submitted.add(identity);
    if (events.some(event => event.event_name === "task_admitted")) stages.task_admitted.add(identity);
    if (events.some(event => event.event_name === "task_first_output")) stages.task_first_output.add(identity);
    const success = events.find(event => event.event_name === "task_finished"
      && event.status === "success"
      && event.initiator_type === "user"
      && ["assistant", "code", "team"].includes(event.mode));
    if (success) {
      stages.task_succeeded.add(identity);
      firstValueDurations.push(Number(success.occurred_at) - startedAt);
    }
  }
  return {
    cohort_installations: cohorts.size,
    matured_installations: matured.length,
    pending_maturity_installations: cohorts.size - matured.length,
    environment_ready_installations: stages.environment_ready.size,
    task_submitted_installations: stages.task_submitted.size,
    task_admitted_installations: stages.task_admitted.size,
    task_first_output_installations: stages.task_first_output.size,
    task_succeeded_installations: stages.task_succeeded.size,
    activation_rate_24h: reliableRate(stages.task_succeeded.size, matured.length),
    first_value_p50_ms: percentile(firstValueDurations, 0.5),
    first_value_p75_ms: percentile(firstValueDurations, 0.75),
    first_value_p90_ms: percentile(firstValueDurations, 0.9),
    confidence: sampleConfidence(matured.length),
  };
}

function behaviorToolType(value) {
  const type = boundedText(value, 32);
  return ["skill", "mcp", "cli", "builtin", "custom", "unknown"].includes(type) ? type : "unknown";
}

export function buildBehaviorOverview(devices, usageEvents, behaviorEvents, range = "30", now = Date.now(), pipelineEvents = []) {
  const window = rangeWindow(range, now);
  const activeByDay = activeIdentityByDay(devices, usageEvents, behaviorEvents, window);
  const activeTotal = periodActiveCount(activeByDay);
  const contributionClipping = clipDailySubjectContributions(
    filteredBehaviorEvents(behaviorEvents, window),
  );
  const windowEvents = contributionClipping.events;
  const taskSubmitted = uniqueAttempts(windowEvents, "task_submitted");
  const activeTaskSubmitted = taskSubmitted.filter((event) => isCoreUserTask(event, "task_submitted"));
  const taskAdmitted = uniqueAttempts(windowEvents, "task_admitted");
  const taskRejected = uniqueAttempts(windowEvents, "task_rejected");
  const taskFinished = uniqueAttempts(windowEvents, "task_finished");
  const cohortStarted = filteredBehaviorEvents(behaviorEvents, window, "analytics_cohort_started");
  const maturedCohorts = new Map();
  for (const event of cohortStarted) {
    const identity = eventIdentity(event);
    if (!identity || Number(event.occurred_at) > now - DAY_MS) continue;
    const prior = maturedCohorts.get(identity);
    if (!prior || event.occurred_at < prior.occurred_at) maturedCohorts.set(identity, event);
  }
  const voiceStarted = filteredBehaviorEvents(behaviorEvents, window, "voice_started");
  const taskInstallations = new Set(activeTaskSubmitted.map(eventIdentity).filter(Boolean));
  const voiceUsers = new Set(voiceStarted.map(eventIdentity).filter(Boolean));
  const taskStatus = { success: 0, interrupted: 0, failed: 0, cancelled: 0 };
  for (const event of taskFinished) {
    if (event.status in taskStatus) taskStatus[event.status] += 1;
  }
  const successfulActiveInstallations = successfulActiveIdentities(behaviorEvents, window);
  const successfulWithinWindowSubmissions = intersectionSize(
    successfulActiveInstallations,
    taskInstallations,
  );
  const voiceAdopters = intersectionSize(voiceUsers, successfulActiveInstallations);
  const firstValueWithin24h = new Set();
  for (const event of taskFinished) {
    if (event.status !== "success" || event.initiator_type !== "user"
      || !["assistant", "code", "team"].includes(event.mode)) continue;
    const identity = eventIdentity(event);
    const cohort = maturedCohorts.get(identity);
    if (cohort && event.occurred_at >= cohort.occurred_at
      && event.occurred_at <= cohort.occurred_at + DAY_MS) firstValueWithin24h.add(identity);
  }
  const maturedSubmissionIds = new Set(taskSubmitted
    .filter((event) => Number(event.occurred_at) <= now - DAY_MS)
    .map((event) => event.attempt_id)
    .filter(Boolean));
  const maturedAdmittedForResolution = taskAdmitted.filter((event) => maturedSubmissionIds.has(event.attempt_id));
  const maturedRejected = taskRejected.filter((event) => maturedSubmissionIds.has(event.attempt_id));
  const resolvedMatured = maturedAdmittedForResolution.length + maturedRejected.length;
  const finishedAttemptIds = new Set(taskFinished.map((event) => event.attempt_id));
  const maturedAdmitted = taskAdmitted.filter((event) => Number(event.occurred_at) <= now - DAY_MS);
  const admittedTerminalCount = maturedAdmitted.filter((event) => finishedAttemptIds.has(event.attempt_id)).length;
  const compensatedCount = [...taskRejected, ...taskFinished]
    .filter((event) => event.stage === "shutdown").length;
  const activeDayMetric = perActiveInstallationDay(activeTaskSubmitted, activeByDay);
  const toolCallStarted = uniqueAttempts(windowEvents.map((event) => ({
    ...event,
    attempt_id: event.operation_id,
  })), "tool_call_started");
  const toolCallCompleted = uniqueAttempts(windowEvents.map((event) => ({
    ...event,
    attempt_id: event.operation_id,
  })), "tool_call_completed");
  const toolInstallStarted = uniqueAttempts(windowEvents.map((event) => ({
    ...event,
    attempt_id: event.operation_id,
  })), "tool_install_started");
  const toolInstallCompleted = uniqueAttempts(windowEvents.map((event) => ({
    ...event,
    attempt_id: event.operation_id,
  })), "tool_install_completed");
  const pipeline = pipelineEvents.filter((entry) => {
    const day = statsDay(entry.occurred_at);
    return day >= window.startDay && day <= window.endDay;
  }).reduce((totals, entry) => {
    for (const key of ["accepted", "duplicate", "rejected", "retryable"]) {
      totals[key] += Number(entry[key] || 0);
    }
    return totals;
  }, { accepted: 0, duplicate: 0, rejected: 0, retryable: 0 });
  const pipelineTotal = pipeline.accepted + pipeline.duplicate + pipeline.rejected + pipeline.retryable;
  const explicitValue = buildWeeklyExplicitValueMetric(behaviorEvents, now);
  const taskFirstOutput = uniqueAttempts(windowEvents, "task_first_output");
  const taskErrors = Object.fromEntries([...taskFinished.concat(taskRejected).reduce((groups, event) => {
    if (event.error_category) groups.set(event.error_category, (groups.get(event.error_category) || 0) + 1);
    return groups;
  }, new Map())].sort((left, right) => right[1] - left[1]));
  const retention = {
    d1: retentionMetric(usageEvents, behaviorEvents, 1, now),
    d7: retentionMetric(usageEvents, behaviorEvents, 7, now),
    d30: retentionMetric(usageEvents, behaviorEvents, 30, now),
  };
  const voiceFunnel = buildVoiceFunnel(windowEvents, taskFinished);
  const activationFunnel = buildActivationFunnel(behaviorEvents, window, now);
  const versionQuality = buildVersionQuality(windowEvents);
  const latestReceivedAt = Math.max(0, ...behaviorEvents.map(event => Number(event.received_at || event.occurred_at) || 0));
  const activeCollectionPresent = devices.some(device => device.behavior_collection?.status === "active");
  const dataStatus = behaviorEvents.length === 0
    ? (activeCollectionPresent ? "no_data" : "tracking_not_enabled")
    : (activeCollectionPresent && latestReceivedAt < now - DAY_MS ? "delayed" : "ready");
  const qualityWarnings = [];
  if (taskSubmitted.length < MIN_RELIABLE_SAMPLE) qualityWarnings.push("task_sample_low");
  if (maturedAdmitted.length && admittedTerminalCount / maturedAdmitted.length < 0.98) qualityWarnings.push("task_terminal_incomplete");
  if (pipelineTotal && pipeline.rejected / pipelineTotal > 0.01) qualityWarnings.push("pipeline_rejection_high");
  if (windowEvents.some(event => event.clock_skew_bucket && event.clock_skew_bucket !== "normal")) qualityWarnings.push("clock_skew_present");
  if (contributionClipping.clipped_subject_count) qualityWarnings.push("subject_contribution_clipped");
  return {
    generated_at: now,
    range: window.range,
    subject: "installation",
    timezone: "Asia/Shanghai",
    metric_version: "behavior-v1.2",
    metric_version_started_at: Date.parse("2026-08-27T00:00:00+08:00"),
    data_status: dataStatus,
    latest_event_received_at: latestReceivedAt || null,
    active_installations: activeTotal,
    metrics: {
      task_submitted_count: taskSubmitted.length,
      task_admitted_count: taskAdmitted.length,
      task_rejected_count: taskRejected.length,
      task_finished_count: taskFinished.length,
      task_active_installations: taskInstallations.size,
      task_submitted_per_active_installation_day: activeDayMetric.value,
      active_installation_days: activeDayMetric.active_installation_days,
      cohort_matured_installations: maturedCohorts.size,
      first_technical_value_24h_installations: firstValueWithin24h.size,
      first_technical_value_24h_rate: reliableRate(firstValueWithin24h.size, maturedCohorts.size),
      wsta_installations: successfulActiveInstallations.size,
      wsta_rate: successfulWithinWindowSubmissions === successfulActiveInstallations.size
        ? reliableRate(successfulActiveInstallations.size, activeTotal)
        : null,
      ...explicitValue,
      task_admission_rate_24h: reliableRate(maturedAdmittedForResolution.length, maturedSubmissionIds.size),
      task_rejection_rate_24h: reliableRate(maturedRejected.length, maturedSubmissionIds.size),
      task_submission_resolution_rate_24h: reliableRate(resolvedMatured, maturedSubmissionIds.size),
      task_terminal_completeness_rate: reliableRate(admittedTerminalCount, maturedAdmitted.length),
      task_terminal_matured_count: maturedAdmitted.length,
      task_terminal_missing_count: maturedAdmitted.length - admittedTerminalCount,
      task_terminal_pending_within_24h_count: taskAdmitted.length - maturedAdmitted.length,
      task_compensation_rate: reliableRate(compensatedCount, taskFinished.length + taskRejected.length),
      task_success_count: taskStatus.success,
      task_interrupted_count: taskStatus.interrupted,
      task_failed_count: taskStatus.failed,
      task_cancelled_count: taskStatus.cancelled,
      task_submission_success_rate: reliableRate(taskStatus.success, taskSubmitted.length),
      admitted_task_success_rate: reliableRate(taskStatus.success, taskFinished.length),
      task_first_output_rate: reliableRate(taskFirstOutput.length, taskAdmitted.length),
      task_cancel_rate: reliableRate(taskStatus.cancelled, taskFinished.length),
      task_first_output_p50_ms: percentile(taskFirstOutput.map(event => event.duration_ms), 0.5),
      task_first_output_p90_ms: percentile(taskFirstOutput.map(event => event.duration_ms), 0.9),
      task_total_duration_p50_ms: percentile(taskFinished.map(event => event.duration_ms), 0.5),
      task_total_duration_p90_ms: percentile(taskFinished.map(event => event.duration_ms), 0.9),
      tool_call_started_count: toolCallStarted.length,
      tool_call_completed_count: toolCallCompleted.length,
      tool_call_completion_rate: reliableRate(toolCallCompleted.length, toolCallStarted.length),
      tool_call_success_rate: reliableRate(
        toolCallCompleted.filter((event) => event.success === true).length,
        toolCallCompleted.length,
      ),
      tool_install_started_count: toolInstallStarted.length,
      tool_install_completed_count: toolInstallCompleted.length,
      tool_install_completion_rate: reliableRate(toolInstallCompleted.length, toolInstallStarted.length),
      voice_penetration_rate: reliableRate(voiceAdopters, successfulActiveInstallations.size),
      voice_started_installations: voiceAdopters,
      voice_observed_installations: voiceUsers.size,
      voice_started_count: voiceStarted.length,
      voice_starts_per_installation: rate(voiceStarted.length, voiceUsers.size),
      pipeline_accepted_count: pipeline.accepted,
      pipeline_duplicate_rate: reliableRate(pipeline.duplicate, pipelineTotal),
      pipeline_rejection_rate: reliableRate(pipeline.rejected, pipelineTotal),
      pipeline_retryable_rate: reliableRate(pipeline.retryable, pipelineTotal),
    },
    retention,
    funnels: { activation: activationFunnel, voice: voiceFunnel },
    version_quality: versionQuality,
    error_categories: taskErrors,
    data_quality: {
      minimum_reliable_sample: MIN_RELIABLE_SAMPLE,
      minimum_group_subjects: MIN_PRIVACY_GROUP_SUBJECTS,
      task_sample_size: taskSubmitted.length,
      confidence: sampleConfidence(taskSubmitted.length),
      contribution_clipping: {
        subject_count: contributionClipping.subject_count,
        clipped_subject_count: contributionClipping.clipped_subject_count,
        clipped_subject_rate: contributionClipping.clipped_subject_rate,
        task_limit_per_event_day: 100,
        tool_call_limit_per_day: 500,
      },
      warnings: qualityWarnings,
    },
  };
}

function sortedRankingRows(groups, countKey, successfulActive, limit = 50, minimumSubjects = MIN_PRIVACY_GROUP_SUBJECTS) {
  return [...groups.values()]
    .filter((row) => row.users.size >= minimumSubjects)
    .map((row) => {
      const adopterCount = intersectionSize(row.users, successfulActive);
      return {
        ...row,
        user_count: adopterCount,
        observed_user_count: row.users.size,
        penetration_rate: reliableRate(adopterCount, successfulActive.size),
        success_rate: reliableRate(row.success_count, row.total),
        penetration_sample_size: successfulActive.size,
        success_sample_size: row.total,
        penetration_confidence: sampleConfidence(successfulActive.size),
        success_confidence: sampleConfidence(row.total),
        [countKey]: row.total,
        users: undefined,
        total: undefined,
      };
    })
    .sort((a, b) => b[countKey] - a[countKey] || b.user_count - a.user_count
      || String(a.name || a.tool_name || a.scene_l2).localeCompare(String(b.name || b.tool_name || b.scene_l2), "zh-CN"))
    .slice(0, limit);
}

export function buildBehaviorToolRanking(devices, usageEvents, behaviorEvents, range = "30", type = "call", now = Date.now(), options = {}) {
  const window = rangeWindow(range, now);
  const minimumSubjects = options.minimumGroupSubjects ?? MIN_PRIVACY_GROUP_SUBJECTS;
  const successfulActive = successfulActiveIdentities(behaviorEvents, window);
  const eventName = type === "install" ? "tool_install_completed" : "tool_call_completed";
  const countKey = type === "install" ? "install_count" : "call_count";
  const groups = new Map();
  const clippedWindowEvents = clipDailySubjectContributions(
    filteredBehaviorEvents(behaviorEvents, window),
  ).events;
  for (const event of clippedWindowEvents.filter(event => event.event_name === eventName)) {
    if (eventName === "tool_install_completed" && event.success !== true) continue;
    const toolKey = controlledKey(event.tool_key, CONTROLLED_TOOL_KEYS);
    const row = groups.get(toolKey) || {
      tool_key: toolKey,
      tool_name: toolKey,
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
  const funnelGroups = new Map();
  const stageNames = new Map([
    ["tool_impression", "impressions"],
    ["tool_detail_opened", "details"],
    ["tool_install_started", "install_started"],
    ["tool_install_completed", "install_succeeded"],
    ["tool_configuration_started", "configuration_started"],
    ["tool_configuration_finished", "configuration_succeeded"],
    ["tool_call_completed", "call_succeeded"],
  ]);
  for (const event of clippedWindowEvents.filter(event => event.event_name.startsWith("tool_"))) {
    const stage = stageNames.get(event.event_name);
    if (!stage) continue;
    const toolKey = controlledKey(event.tool_key, CONTROLLED_TOOL_KEYS);
    const row = funnelGroups.get(toolKey) || {
      tool_key: toolKey,
      tool_name: toolKey,
      tool_type: behaviorToolType(event.tool_type),
      impressions: new Set(),
      details: new Set(),
      install_started: new Set(),
      install_succeeded: new Set(),
      configuration_started: new Set(),
      configuration_succeeded: new Set(),
      call_succeeded: new Set(),
      failed: new Set(),
      cancelled: new Set(),
      configuration_completed_events: [],
      successful_call_events: [],
      users: new Set(),
    };
    const stageId = boundedText(event.operation_id || event.event_id
      || `${event.view_id || "view"}:${toolKey}`, 256);
    const identity = eventIdentity(event);
    if (identity) row.users.add(identity);
    const successfulTerminal = !["tool_install_completed", "tool_configuration_finished", "tool_call_completed"].includes(event.event_name)
      || event.success === true;
    if (stageId && successfulTerminal) row[stage].add(stageId);
    if (event.event_name === "tool_configuration_finished" && event.success === true) {
      row.configuration_completed_events.push({ identity: eventIdentity(event), occurred_at: Number(event.occurred_at) });
    }
    if (event.event_name === "tool_call_completed" && event.success === true) {
      row.successful_call_events.push({ identity: eventIdentity(event), occurred_at: Number(event.occurred_at) });
    }
    if (stageId && event.success === false) {
      row.failed.add(stageId);
      if (event.error_category === "user_cancelled") row.cancelled.add(stageId);
    }
    funnelGroups.set(toolKey, row);
  }
  const funnelRows = [...funnelGroups.values()].filter(row => row.users.size >= minimumSubjects).map(row => {
    const configurationToCall = row.configuration_completed_events.flatMap(configuration => {
      const call = row.successful_call_events
        .filter(candidate => candidate.identity && candidate.identity === configuration.identity
          && candidate.occurred_at >= configuration.occurred_at)
        .sort((left, right) => left.occurred_at - right.occurred_at)[0];
      return call ? [call.occurred_at - configuration.occurred_at] : [];
    });
    return {
      tool_key: row.tool_key,
      tool_name: row.tool_name,
      tool_type: row.tool_type,
      user_count: row.users.size,
      impression_count: row.impressions.size,
      detail_count: row.details.size,
      install_started_count: row.install_started.size,
      install_succeeded_count: row.install_succeeded.size,
      configuration_started_count: row.configuration_started.size,
      configuration_succeeded_count: row.configuration_succeeded.size,
      call_succeeded_count: row.call_succeeded.size,
      failed_count: row.failed.size,
      cancelled_count: row.cancelled.size,
      impression_to_install_rate: reliableRate(row.install_started.size, row.impressions.size),
      install_success_rate: reliableRate(row.install_succeeded.size, row.install_started.size),
      install_to_configuration_rate: reliableRate(row.configuration_started.size, row.install_succeeded.size),
      configuration_success_rate: reliableRate(row.configuration_succeeded.size, row.configuration_started.size),
      configuration_to_call_rate: reliableRate(row.call_succeeded.size, row.configuration_succeeded.size),
      configuration_to_call_p50_ms: percentile(configurationToCall, 0.5),
      confidence: sampleConfidence(row.impressions.size),
    };
  }).sort((left, right) => right.impression_count - left.impression_count
    || right.install_started_count - left.install_started_count
    || left.tool_key.localeCompare(right.tool_key, "zh-CN"));
  return {
    generated_at: now,
    range: window.range,
    type,
    minimum_group_subjects: minimumSubjects,
    rows: sortedRankingRows(groups, countKey, successfulActive, 50, minimumSubjects),
    funnel_rows: funnelRows,
    suppressed_group_count: [...funnelGroups.values()]
      .filter(row => row.users.size < minimumSubjects).length,
  };
}

export function buildBehaviorSceneRanking(devices, usageEvents, behaviorEvents, range = "30", now = Date.now(), options = {}) {
  const window = rangeWindow(range, now);
  const minimumSubjects = options.minimumGroupSubjects ?? MIN_PRIVACY_GROUP_SUBJECTS;
  const successfulActive = successfulActiveIdentities(behaviorEvents, window);
  const groups = new Map();
  const windowEvents = clipDailySubjectContributions(
    filteredBehaviorEvents(behaviorEvents, window),
  ).events;
  const finishedByAttempt = new Map(uniqueAttempts(windowEvents, "task_finished")
    .map(event => [event.attempt_id, event]));
  const latestFeedbackByTurn = new Map();
  for (const event of windowEvents) {
    if (event.event_name !== "response_feedback_changed" || !event.turn_id) continue;
    const previous = latestFeedbackByTurn.get(event.turn_id);
    if (!previous || Number(event.occurred_at) >= Number(previous.occurred_at)) {
      latestFeedbackByTurn.set(event.turn_id, event);
    }
  }
  for (const event of windowEvents
    .filter((entry) => ["scene_impression", "scene_selected", "scene_submitted", "scene_triggered"].includes(entry.event_name))) {
    const sceneL1 = boundedText(event.scene_l1, 64) || "unknown";
    const sceneL2 = boundedText(event.scene_l2, 96) || "unknown";
    const key = `${sceneL1}:${sceneL2}`;
    const row = groups.get(key) || {
      scene_l1: sceneL1,
      scene_l2: sceneL2,
      total: 0,
      success_count: 0,
      users: new Set(),
      impressions: new Set(),
      selections: new Set(),
      submissions: new Set(),
      task_successes: new Set(),
      valued_tasks: new Set(),
      negative_feedback: new Set(),
      durations: [],
    };
    const identity = eventIdentity(event);
    const eventKey = boundedText(event.operation_id || event.attempt_id || event.event_id, 256);
    if (event.event_name === "scene_impression" && eventKey) row.impressions.add(eventKey);
    if (event.event_name === "scene_selected" && eventKey) row.selections.add(eventKey);
    if (["scene_submitted", "scene_triggered"].includes(event.event_name) && eventKey) {
      row.submissions.add(eventKey);
      const finished = finishedByAttempt.get(event.attempt_id);
      if (finished) {
        row.durations.push(finished.duration_ms);
        if (finished.status === "success") row.task_successes.add(event.attempt_id);
        const feedback = latestFeedbackByTurn.get(finished.turn_id);
        if (feedback?.feedback_action === "set" && feedback.rating === "negative") {
          row.negative_feedback.add(event.attempt_id);
        }
        const hasValue = windowEvents.some(signal => (
          eventIdentity(signal) === identity
            && signal.turn_id === finished.turn_id
            && ["response_copied", "artifact_exported"].includes(signal.event_name)
            && Number(signal.occurred_at) >= Number(finished.occurred_at)
            && Number(signal.occurred_at) <= Number(finished.occurred_at) + EXPLICIT_VALUE_WINDOW_MS
        )) || (feedback?.feedback_action === "set"
          && feedback.rating === "positive"
          && eventIdentity(feedback) === identity
          && Number(feedback.occurred_at) >= Number(finished.occurred_at)
          && Number(feedback.occurred_at) <= Number(finished.occurred_at) + EXPLICIT_VALUE_WINDOW_MS);
        if (finished.status === "success" && hasValue) row.valued_tasks.add(event.attempt_id);
      }
    }
    row.total = row.submissions.size;
    row.success_count = row.task_successes.size;
    if (identity) row.users.add(identity);
    groups.set(key, row);
  }
  return {
    generated_at: now,
    range: window.range,
    minimum_group_subjects: minimumSubjects,
    rows: [...groups.values()].filter(row => row.users.size >= minimumSubjects).map(row => {
      const adopterCount = intersectionSize(row.users, successfulActive);
      return {
        scene_l1: row.scene_l1,
        scene_l2: row.scene_l2,
        impression_count: row.impressions.size,
        selection_count: row.selections.size,
        trigger_count: row.submissions.size,
        user_count: adopterCount,
        observed_user_count: row.users.size,
        penetration_rate: reliableRate(adopterCount, successfulActive.size),
        penetration_sample_size: successfulActive.size,
        selection_rate: reliableRate(row.selections.size, row.impressions.size),
        submission_rate: reliableRate(row.submissions.size, row.selections.size),
        task_success_rate: reliableRate(row.task_successes.size, row.submissions.size),
        explicit_value_rate: reliableRate(row.valued_tasks.size, row.task_successes.size),
        negative_feedback_rate: reliableRate(row.negative_feedback.size, row.submissions.size),
        task_duration_p50_ms: percentile(row.durations, 0.5),
        confidence: sampleConfidence(row.impressions.size),
      };
    }).sort((left, right) => right.trigger_count - left.trigger_count
      || right.impression_count - left.impression_count
      || left.scene_l2.localeCompare(right.scene_l2, "zh-CN")),
    suppressed_group_count: [...groups.values()]
      .filter(row => row.users.size < minimumSubjects).length,
  };
}

export function buildBehaviorModelRanking(devices, usageEvents, behaviorEvents, range = "30", now = Date.now(), options = {}) {
  const window = rangeWindow(range, now);
  const minimumSubjects = options.minimumGroupSubjects ?? MIN_PRIVACY_GROUP_SUBJECTS;
  const successfulActive = successfulActiveIdentities(behaviorEvents, window);
  const groups = new Map();
  const windowEvents = clipDailySubjectContributions(
    filteredBehaviorEvents(behaviorEvents, window),
  ).events;
  const finishedByAttempt = new Map(uniqueAttempts(windowEvents, "task_finished")
    .map(event => [event.attempt_id, event]));
  for (const event of windowEvents.filter(event => event.event_name === "model_used")) {
    const modelId = controlledKey(event.model_key, CONTROLLED_MODEL_KEYS);
    const providerKey = controlledKey(event.provider_key, CONTROLLED_PROVIDER_KEYS);
    const key = `${providerKey}:${modelId}`;
    const row = groups.get(key) || {
      provider_key: providerKey,
      model_key: modelId,
      total: 0,
      finished: new Set(),
      success: new Set(),
      durations: [],
      selection_sources: new Map(),
      selection_scopes: new Map(),
      explicit_selection_count: 0,
      users: new Set(),
    };
    row.total += 1;
    const selectionSource = boundedText(event.selection_source, 32) || "unknown";
    row.selection_sources.set(selectionSource, (row.selection_sources.get(selectionSource) || 0) + 1);
    const attemptId = boundedText(event.attempt_id, 128);
    const finished = finishedByAttempt.get(attemptId);
    if (attemptId && finished) {
      row.finished.add(attemptId);
      if (finished.status === "success") row.success.add(attemptId);
      if (Number.isFinite(Number(finished.duration_ms))) row.durations.push(Number(finished.duration_ms));
    }
    const identity = eventIdentity(event);
    if (identity) row.users.add(identity);
    groups.set(key, row);
  }
  for (const event of windowEvents.filter(event => event.event_name === "model_selected")) {
    const modelId = controlledKey(event.model_key, CONTROLLED_MODEL_KEYS);
    const providerKey = controlledKey(event.provider_key, CONTROLLED_PROVIDER_KEYS);
    const key = `${providerKey}:${modelId}`;
    const row = groups.get(key) || {
      provider_key: providerKey,
      model_key: modelId,
      total: 0,
      finished: new Set(),
      success: new Set(),
      durations: [],
      selection_sources: new Map(),
      selection_scopes: new Map(),
      explicit_selection_count: 0,
      users: new Set(),
    };
    row.explicit_selection_count += 1;
    const scope = boundedText(event.selection_scope, 32) || "unknown";
    row.selection_scopes.set(scope, (row.selection_scopes.get(scope) || 0) + 1);
    const identity = eventIdentity(event);
    if (identity) row.users.add(identity);
    groups.set(key, row);
  }
  return {
    generated_at: now,
    range: window.range,
    minimum_group_subjects: minimumSubjects,
    rows: [...groups.values()]
      .filter((row) => row.users.size >= minimumSubjects)
      .map((row) => ({
        provider_key: row.provider_key,
        model_key: row.model_key,
        user_count: intersectionSize(row.users, successfulActive),
        observed_user_count: row.users.size,
        penetration_rate: reliableRate(intersectionSize(row.users, successfulActive), successfulActive.size),
        penetration_sample_size: successfulActive.size,
        use_count: row.total,
        task_terminal_count: row.finished.size,
        task_success_count: row.success.size,
        task_success_rate: reliableRate(row.success.size, row.finished.size),
        task_duration_p50_ms: percentile(row.durations, 0.5),
        task_duration_p90_ms: percentile(row.durations, 0.9),
        selection_sources: Object.fromEntries([...row.selection_sources]
          .sort((left, right) => right[1] - left[1] || left[0].localeCompare(right[0]))),
        explicit_selection_count: row.explicit_selection_count,
        selection_scopes: Object.fromEntries([...row.selection_scopes]
          .sort((left, right) => right[1] - left[1] || left[0].localeCompare(right[0]))),
        confidence: sampleConfidence(row.finished.size),
      }))
      .sort((a, b) => b.user_count - a.user_count || b.use_count - a.use_count
        || String(a.model_key).localeCompare(String(b.model_key), "zh-CN"))
      .slice(0, 50),
    suppressed_group_count: [...groups.values()]
      .filter(row => row.users.size < minimumSubjects).length,
  };
}

export function buildCommercialMock(range = "30", now = Date.now()) {
  const window = rangeWindow(range, now);
  return {
    generated_at: now,
    range: window.range,
    available: false,
    reason: "authoritative_commercial_source_not_connected",
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
  const temporary = `${path}.${process.pid}.${crypto.randomBytes(6).toString("hex")}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600, flag: "wx" });
  renameSync(temporary, path);
  enforcePrivateFile(path);
}

function atomicWriteJsonLines(path, values) {
  const temporary = `${path}.${process.pid}.${crypto.randomBytes(6).toString("hex")}.tmp`;
  const content = values.length
    ? `${values.map((value) => JSON.stringify(value)).join("\n")}\n`
    : "";
  writeFileSync(temporary, content, { mode: 0o600, flag: "wx" });
  renameSync(temporary, path);
  enforcePrivateFile(path);
}

function enforcePrivateFile(path) {
  const metadata = lstatSync(path);
  if (metadata.isSymbolicLink() || !metadata.isFile()) {
    throw new Error(`unsafe telemetry storage file: ${path}`);
  }
  chmodSync(path, 0o600);
}

export class TelemetryStore {
  constructor(dataDir, pepper, options = {}) {
    this.dataDir = dataDir;
    this.pepper = pepper;
    this.devicesPath = join(dataDir, "devices.json");
    this.eventsPath = join(dataDir, "usage-events.jsonl");
    this.behaviorEventsPath = join(dataDir, "behavior-events.jsonl");
    this.behaviorPipelinePath = join(dataDir, "behavior-pipeline.jsonl");
    this.deletionTombstonesPath = join(dataDir, "behavior-deletion-tombstones.json");
    mkdirSync(dataDir, { recursive: true, mode: 0o700 });
    const dataDirectory = lstatSync(dataDir);
    if (dataDirectory.isSymbolicLink() || !dataDirectory.isDirectory()) {
      throw new Error(`unsafe telemetry data directory: ${dataDir}`);
    }
    chmodSync(dataDir, 0o700);
    this.devices = new Map();
    this.devicesByHardwareHash = new Map();
    this.events = [];
    this.eventIds = new Set();
    this.behaviorEvents = [];
    this.behaviorEventIds = new Set();
    this.behaviorPipelineEvents = [];
    this.deletionTombstones = [];
    this.saveTimer = null;
    this.lastEventCompactionAt = 0;
    this.eventRateBuckets = new Map();
    this.globalEventRateBucket = null;
    this.environment = telemetryEnvironment(options.environment);
    this.maxDevices = boundedInteger(options.maxDevices, DEFAULT_MAX_DEVICES, 1, 1_000_000);
    this.maxEvents = boundedInteger(options.maxEvents, DEFAULT_MAX_EVENTS, 100, 5_000_000);
    this.eventCompactionTarget = Math.max(1, Math.floor(this.maxEvents * 0.9));
    this.eventRetentionMs = boundedInteger(
      options.eventRetentionMs,
      DEFAULT_EVENT_RETENTION_MS,
      DAY_MS,
      DEFAULT_EVENT_RETENTION_MS,
    );
    this.eventRatePerMinute = boundedInteger(
      options.eventRatePerMinute,
      DEFAULT_EVENT_RATE_PER_MINUTE,
      MAX_USAGE_EVENTS_PER_BATCH,
      100_000,
    );
    this.globalEventRatePerMinute = boundedInteger(
      options.globalEventRatePerMinute,
      DEFAULT_GLOBAL_EVENT_RATE_PER_MINUTE,
      MAX_USAGE_EVENTS_PER_BATCH,
      1_000_000,
    );

    this.loadDeletionTombstones();
    const deletedSubjects = new Set(this.deletionTombstones.map((record) => record.subject_hash));
    const saved = loadJson(this.devicesPath, { devices: [] });
    let restoredDeletedDevice = false;
    let restoredExpiredDevice = false;
    const deviceCutoff = Date.now() - this.eventRetentionMs;
    for (const device of Array.isArray(saved.devices) ? saved.devices : []) {
      if (!device?.device_id || !device?.hardware_hash || !device?.device_token) continue;
      if (deletedSubjects.has(this.deletionSubjectHash(device.device_id))) {
        restoredDeletedDevice = true;
        continue;
      }
      if (this.deviceRetentionStartedAt(device) < deviceCutoff) {
        restoredExpiredDevice = true;
        this.deletionTombstones.push({
          subject_hash: this.deletionSubjectHash(device.device_id),
          token_hash: this.deletionTokenHash(device.device_token),
        });
        continue;
      }
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
    this.loadBehaviorPipelineEvents();
    const recoveredDeletion = this.deletionTombstones.some((record) => (
      record.deletion_request_id && record.status !== "completed"
    ));
    if (recoveredDeletion) {
      const completedAt = Date.now();
      for (const record of this.deletionTombstones) {
        if (record.deletion_request_id && record.status !== "completed") {
          record.status = "completed";
          record.completed_at = completedAt;
        }
      }
      this.saveDeletionTombstones();
    }
    if (restoredDeletedDevice || restoredExpiredDevice) this.saveNow();
    if (restoredExpiredDevice) this.saveDeletionTombstones();
    this.enforcePrivateStoragePermissions();
  }

  deletionSubjectHash(deviceId) {
    return crypto.createHmac("sha256", this.pepper)
      .update(`deletion-subject:${boundedText(deviceId, 96)}`)
      .digest("hex");
  }

  deletionTokenHash(token) {
    return crypto.createHmac("sha256", this.pepper)
      .update(`deletion-token:${boundedText(token, 256)}`)
      .digest("hex");
  }

  loadDeletionTombstones() {
    const saved = loadJson(this.deletionTombstonesPath, { records: [] });
    this.deletionTombstones = (Array.isArray(saved.records) ? saved.records : [])
      .filter((record) => record?.subject_hash);
    if (existsSync(this.deletionTombstonesPath)
      && this.deletionTombstones.length !== (saved.records || []).length) {
      this.saveDeletionTombstones();
    }
  }

  saveDeletionTombstones() {
    atomicWriteJson(this.deletionTombstonesPath, {
      version: 2,
      records: this.deletionTombstones,
    });
  }

  enforcePrivateStoragePermissions() {
    chmodSync(this.dataDir, 0o700);
    for (const path of [
      this.devicesPath,
      this.eventsPath,
      this.behaviorEventsPath,
      this.behaviorPipelinePath,
      this.deletionTombstonesPath,
    ]) {
      if (existsSync(path)) enforcePrivateFile(path);
    }
  }

  subjectWasDeleted(deviceId) {
    const subjectHash = this.deletionSubjectHash(deviceId);
    return this.deletionTombstones.some((record) => record.subject_hash === subjectHash);
  }

  credentialWasRemoved(token) {
    const tokenHash = this.deletionTokenHash(token);
    return this.deletionTombstones.some((record) => (
      record.token_hash && safeEqual(record.token_hash, tokenHash)
    ));
  }

  publicDeletionResponse(record) {
    return {
      request_id: record.request_id,
      deletion_request_id: record.deletion_request_id,
      status: record.status,
      accepted_at: record.accepted_at,
      completed_at: record.completed_at || null,
    };
  }

  priorDeletionResponse(requestId, token, now = Date.now()) {
    const tokenHash = this.deletionTokenHash(token);
    const record = this.deletionTombstones.find((candidate) => (
        candidate.request_id === boundedText(requestId, 128)
        && safeEqual(candidate.token_hash, tokenHash)
        && Number(candidate.result_expires_at) > now
    ));
    return record ? this.publicDeletionResponse(record) : null;
  }

  deletionStatus(deletionRequestId, token, now = Date.now()) {
    const tokenHash = this.deletionTokenHash(token);
    const record = this.deletionTombstones.find((candidate) => (
      candidate.deletion_request_id === boundedText(deletionRequestId, 128)
        && safeEqual(candidate.token_hash, tokenHash)
    ));
    if (!record) throw new TelemetryHttpError(404, "deletion_request_not_found");
    if (Number(record.result_expires_at) <= now) {
      throw new TelemetryHttpError(410, "deletion_result_expired");
    }
    return this.publicDeletionResponse(record);
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
        if (!device || this.subjectWasDeleted(event.device_id)) {
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
        const raw = JSON.parse(line);
        const event = sanitizeStoredBehaviorEvent(raw);
        if (!event.event_id || this.behaviorEventIds.has(event.event_id)
          || this.subjectWasDeleted(event.device_id)
          || !STORED_BEHAVIOR_EVENT_NAMES.has(event.event_name)) {
          rewriteNeeded = true;
          continue;
        }
        if (JSON.stringify(event) !== JSON.stringify(raw)) rewriteNeeded = true;
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

  deviceRetentionStartedAt(device) {
    return Number(device?.first_seen_at || device?.last_seen_at || 0);
  }

  regionNeedsRefresh(device, now = Date.now()) {
    return now - Number(device.region_updated_at || 0) >= REGION_REFRESH_MS;
  }

  register(input, region) {
    const claim = boundedText(input.hardware_claim, 512);
    if (claim.length < 8) throw new Error("invalid_hardware_claim");
    const registrationSecret = boundedText(input.registration_secret, 256);
    if (registrationSecret.length < 32) throw new Error("invalid_registration_secret");
    const now = Date.now();
    this.compactExpiredData(now);
    const hardwareHash = this.hardwareHash(claim);
    const registrationSecretHash = this.registrationSecretHash(registrationSecret);
    let device = this.devicesByHardwareHash.get(hardwareHash);
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
    if (this.deviceRetentionStartedAt(device) < Date.now() - this.eventRetentionMs) {
      this.compactExpiredData(Date.now());
      return null;
    }
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
    return this.compactExpiredData(now);
  }

  compactExpiredData(now = Date.now()) {
    const usageChanged = this.compactEvents(now);
    const behaviorChanged = this.compactBehaviorEvents(now);
    const pipelineChanged = this.compactBehaviorPipelineEvents(now);
    const deletionLedgerChanged = this.compactDeletionTombstones(now);
    const cutoff = now - this.eventRetentionMs;
    let devicesChanged = false;
    const expiredDeviceIds = new Set();
    for (const [deviceId, device] of this.devices) {
      if (this.deviceRetentionStartedAt(device) >= cutoff) continue;
      const subjectHash = this.deletionSubjectHash(deviceId);
      if (!this.deletionTombstones.some((record) => record.subject_hash === subjectHash)) {
        this.deletionTombstones.push({
          subject_hash: subjectHash,
          token_hash: this.deletionTokenHash(device.device_token),
        });
      }
      this.devices.delete(deviceId);
      this.devicesByHardwareHash.delete(device.hardware_hash);
      this.eventRateBuckets.delete(deviceId);
      expiredDeviceIds.add(deviceId);
      devicesChanged = true;
    }
    if (devicesChanged) {
      this.events = this.events.filter((event) => !expiredDeviceIds.has(event.device_id));
      this.eventIds = new Set(this.events.map((event) => event.event_id));
      this.behaviorEvents = this.behaviorEvents
        .filter((event) => !expiredDeviceIds.has(event.device_id));
      this.behaviorEventIds = new Set(this.behaviorEvents.map((event) => event.event_id));
      atomicWriteJsonLines(this.eventsPath, this.events);
      atomicWriteJsonLines(this.behaviorEventsPath, this.behaviorEvents);
      this.saveNow();
      this.saveDeletionTombstones();
    }
    if (usageChanged || devicesChanged) this.rebuildUsageCounters();
    this.lastEventCompactionAt = now;
    return usageChanged || behaviorChanged || pipelineChanged || deletionLedgerChanged || devicesChanged;
  }

  compactDeletionTombstones(now = Date.now()) {
    const bySubject = new Map();
    for (const record of this.deletionTombstones) {
      const current = bySubject.get(record.subject_hash);
      if (Number(record.result_expires_at) > now) {
        bySubject.set(record.subject_hash, record);
      } else if (!current || Number(current.result_expires_at) <= now) {
        bySubject.set(record.subject_hash, {
          subject_hash: record.subject_hash,
          ...(record.token_hash ? { token_hash: record.token_hash } : {}),
        });
      }
    }
    const compacted = [...bySubject.values()];
    if (JSON.stringify(compacted) === JSON.stringify(this.deletionTombstones)) return false;
    this.deletionTombstones = compacted;
    this.saveDeletionTombstones();
    return true;
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
    for (const raw of inputEvents.slice(0, MAX_USAGE_EVENTS_PER_BATCH)) {
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
      if (existsSync(this.eventsPath)) enforcePrivateFile(this.eventsPath);
      appendFileSync(this.eventsPath, accepted.map((event) => JSON.stringify(event)).join("\n") + "\n", { mode: 0o600 });
      enforcePrivateFile(this.eventsPath);
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

  appendBehaviorEvents(device, inputEvents, batchId = null) {
    if (inputEvents.length > MAX_BEHAVIOR_EVENTS_PER_BATCH) {
      throw new TelemetryHttpError(413, "too_many_events");
    }
    const now = Date.now();
    const response = {
      batch_id: boundedText(batchId, 96) || null,
      results: [],
    };
    this.compactExpiredEventsIfDue(now);
    if (this.eventRateLimited(device.device_id, inputEvents.length, now)) {
      response.results = inputEvents.map((raw) => ({
        event_id: boundedText(raw?.event_id, 96),
        status: "retryable",
        reason_code: "rate_limited",
      }));
      this.recordBehaviorPipelineResponse(response, now);
      return response;
    }

    const collection = device.behavior_collection;
    const accepted = [];
    const acceptedResultIndexes = [];
    const batchEventIds = new Set();
    for (const raw of inputEvents.slice(0, MAX_BEHAVIOR_EVENTS_PER_BATCH)) {
      const eventId = boundedText(raw?.event_id, 96);
      if (raw?.account_subject_id !== undefined) {
        response.results.push({
          event_id: eventId,
          status: "rejected",
          reason_code: "invalid_identity_claim",
        });
        continue;
      }
      const validation = validateBehaviorEventShape(raw, {
        now,
        retentionMs: this.eventRetentionMs,
      });
      if (!validation.ok) {
        response.results.push({
          event_id: eventId,
          status: "rejected",
          reason_code: validation.reason,
        });
        continue;
      }
      if (collection?.status !== "active"
        || raw.collection_period_id !== collection.collection_period_id) {
        response.results.push({
          event_id: eventId,
          status: "rejected",
          reason_code: "collection_inactive",
        });
        continue;
      }
      if (this.behaviorEventIds.has(eventId) || batchEventIds.has(eventId)) {
        response.results.push({
          event_id: eventId,
          status: "duplicate",
          reason_code: null,
        });
        continue;
      }
      batchEventIds.add(eventId);
      acceptedResultIndexes.push(response.results.length);
      response.results.push({
        event_id: eventId,
        status: "accepted",
        reason_code: null,
      });
      accepted.push(sanitizeStoredBehaviorEvent({
        ...raw,
        received_at: now,
        device_id: device.device_id,
        environment: this.environment,
        clock_skew_bucket: clockSkewBucket(raw.occurred_at, now),
      }));
    }
    if (accepted.length) {
      try {
        if (existsSync(this.behaviorEventsPath)) enforcePrivateFile(this.behaviorEventsPath);
        appendFileSync(
          this.behaviorEventsPath,
          `${accepted.map((event) => JSON.stringify(event)).join("\n")}\n`,
          { mode: 0o600 },
        );
        enforcePrivateFile(this.behaviorEventsPath);
      } catch (error) {
        console.error("[telemetry] failed to persist behavior events:", error);
        for (const index of acceptedResultIndexes) {
          response.results[index] = {
            event_id: response.results[index].event_id,
            status: "retryable",
            reason_code: "storage_unavailable",
          };
        }
        this.recordBehaviorPipelineResponse(response, now);
        return response;
      }
      for (const event of accepted) {
        this.behaviorEventIds.add(event.event_id);
        this.behaviorEvents.push(event);
      }
      if (this.behaviorEvents.length > this.maxEvents) this.compactBehaviorEvents(now, true);
      device.last_seen_at = now;
      this.saveSoon();
    }
    this.recordBehaviorPipelineResponse(response, now);
    return response;
  }

  loadBehaviorPipelineEvents() {
    if (!existsSync(this.behaviorPipelinePath)) return;
    const lines = readFileSync(this.behaviorPipelinePath, "utf8").split("\n");
    for (const line of lines) {
      if (!line.trim()) continue;
      try {
        const entry = JSON.parse(line);
        if (Number.isFinite(Number(entry?.occurred_at))) this.behaviorPipelineEvents.push(entry);
      } catch {
        // Ignore a torn final line; pipeline health must not block service boot.
      }
    }
    this.compactBehaviorPipelineEvents(Date.now());
  }

  compactBehaviorPipelineEvents(now = Date.now()) {
    const cutoff = now - this.eventRetentionMs;
    const retained = this.behaviorPipelineEvents
      .filter((entry) => Number(entry.occurred_at) >= cutoff)
      .slice(-this.eventCompactionTarget);
    const changed = retained.length !== this.behaviorPipelineEvents.length;
    this.behaviorPipelineEvents = retained;
    if (changed) atomicWriteJsonLines(this.behaviorPipelinePath, retained);
  }

  recordBehaviorPipelineResponse(response, now = Date.now()) {
    const entry = {
      occurred_at: now,
      accepted: 0,
      duplicate: 0,
      rejected: 0,
      retryable: 0,
    };
    for (const result of response.results || []) {
      if (Object.hasOwn(entry, result.status)) entry[result.status] += 1;
    }
    try {
      if (existsSync(this.behaviorPipelinePath)) enforcePrivateFile(this.behaviorPipelinePath);
      appendFileSync(this.behaviorPipelinePath, `${JSON.stringify(entry)}\n`, { mode: 0o600 });
      enforcePrivateFile(this.behaviorPipelinePath);
    } catch (error) {
      console.error("[telemetry] failed to persist behavior pipeline counters:", error);
      return;
    }
    this.behaviorPipelineEvents.push(entry);
    if (this.behaviorPipelineEvents.length > this.maxEvents) {
      this.compactBehaviorPipelineEvents(now);
    }
  }

  rememberBehaviorControlRequest(device, requestId, kind, response) {
    const requests = Array.isArray(device.behavior_control_requests)
      ? device.behavior_control_requests.filter((item) => item?.request_id !== requestId)
      : [];
    requests.push({ request_id: requestId, kind, response, created_at: Date.now() });
    device.behavior_control_requests = requests.slice(-64);
    this.saveNow();
    return response;
  }

  priorBehaviorControlResponse(device, requestId, kind) {
    const request = (device.behavior_control_requests || [])
      .find((item) => item?.request_id === requestId);
    if (!request) return null;
    if (request.kind !== kind) throw new TelemetryHttpError(409, "request_id_conflict");
    return request.response;
  }

  startBehaviorCollection(device, input) {
    const requestId = boundedText(input?.request_id, 128);
    if (requestId.length < 12) throw new TelemetryHttpError(400, "invalid_request_id");
    const prior = this.priorBehaviorControlResponse(device, requestId, "start");
    if (prior) return prior;
    const now = Date.now();
    if (device.behavior_collection?.status === "active") {
      const response = {
        request_id: requestId,
        collection_period_id: device.behavior_collection.collection_period_id,
        status: "active",
        started_at: device.behavior_collection.started_at,
      };
      return this.rememberBehaviorControlRequest(device, requestId, "start", response);
    }
    const collection = {
      collection_period_id: randomId("period_", 18),
      status: "active",
      started_at: now,
      ended_at: null,
    };
    device.behavior_collection = collection;
    return this.rememberBehaviorControlRequest(device, requestId, "start", {
      request_id: requestId,
      collection_period_id: collection.collection_period_id,
      status: "active",
      started_at: now,
    });
  }

  endBehaviorCollection(device, input) {
    const requestId = boundedText(input?.request_id, 128);
    const periodId = boundedText(input?.collection_period_id, 128);
    if (requestId.length < 12 || periodId.length < 12) {
      throw new TelemetryHttpError(400, "invalid_collection_request");
    }
    const prior = this.priorBehaviorControlResponse(device, requestId, "end");
    if (prior) return prior;
    const collection = device.behavior_collection;
    if (!collection || collection.collection_period_id !== periodId) {
      throw new TelemetryHttpError(409, "collection_period_mismatch");
    }
    if (collection.status === "active") {
      collection.status = "ended";
      collection.ended_at = Date.now();
    }
    return this.rememberBehaviorControlRequest(device, requestId, "end", {
      request_id: requestId,
      collection_period_id: periodId,
      status: "ended",
      collection_ended_at: collection.ended_at,
    });
  }

  deleteBehaviorData(device, input) {
    const requestId = boundedText(input?.request_id, 128);
    if (requestId.length < 12) throw new TelemetryHttpError(400, "invalid_request_id");
    const prior = this.priorDeletionResponse(requestId, device.device_token);
    if (prior) return prior;
    const now = Date.now();
    if (device.behavior_collection?.status === "active") {
      device.behavior_collection.status = "ended";
      device.behavior_collection.ended_at = now;
    }
    const tombstone = {
      request_id: requestId,
      deletion_request_id: randomId("delete_", 18),
      subject_hash: this.deletionSubjectHash(device.device_id),
      token_hash: this.deletionTokenHash(device.device_token),
      status: "processing",
      accepted_at: now,
      completed_at: null,
      result_expires_at: now + DELETION_RESULT_RETENTION_MS,
    };
    this.deletionTombstones.push(tombstone);
    this.saveDeletionTombstones();
    this.events = this.events.filter((event) => event.device_id !== device.device_id);
    this.eventIds = new Set(this.events.map((event) => event.event_id));
    this.behaviorEvents = this.behaviorEvents.filter((event) => event.device_id !== device.device_id);
    this.behaviorEventIds = new Set(this.behaviorEvents.map((event) => event.event_id));
    atomicWriteJsonLines(this.eventsPath, this.events);
    atomicWriteJsonLines(this.behaviorEventsPath, this.behaviorEvents);
    this.devices.delete(device.device_id);
    this.devicesByHardwareHash.delete(device.hardware_hash);
    this.rebuildUsageCounters();
    this.saveNow();
    tombstone.status = "completed";
    tombstone.completed_at = Date.now();
    this.saveDeletionTombstones();
    return this.publicDeletionResponse(tombstone);
  }

  behaviorCollectionStatus(device) {
    const collection = device.behavior_collection || null;
    return {
      status: collection?.status || "inactive",
      collection_period_id: collection?.collection_period_id || null,
      started_at: collection?.started_at || null,
      collection_ended_at: collection?.ended_at || null,
    };
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
    return buildBehaviorOverview(
      [...this.devices.values()],
      this.events,
      this.behaviorEvents,
      range,
      Date.now(),
      this.behaviorPipelineEvents,
    );
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
  const behaviorAnalystPassword = String(process.env.PINVOU_BEHAVIOR_ANALYTICS_PASSWORD || "");
  const dataDir = process.env.PINVOU_TELEMETRY_DATA_DIR || "/var/lib/pinvou-telemetry";
  const enabled = enrollmentToken.length >= 24 && devicePepper.length >= 24 && adminPassword.length >= 12;
  const store = enabled ? new TelemetryStore(dataDir, devicePepper, {
    environment: options.environment
      ?? process.env.PINVOU_TELEMETRY_ENVIRONMENT
      ?? process.env.NODE_ENV,
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
  const compactionTimer = store ? setInterval(() => {
    try {
      store.compactExpiredData(Date.now());
    } catch (error) {
      console.error("[telemetry] scheduled retention compaction failed:", error);
    }
  }, Math.max(60_000, Number(options.compactionIntervalMs) || 60 * 60_000)) : null;
  compactionTimer?.unref?.();
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

  function rejectUnknownDevice(res, token) {
    if (store.credentialWasRemoved(token)) {
      json(res, 410, { error: "device_data_absent" });
    } else {
      json(res, 401, { error: "invalid_device_token" });
    }
  }

  function adminAuthorized(req) {
    const token = readCookies(req).pinvou_stats_session;
    const session = token ? adminSessions.get(token) : null;
    if (!session || session.expires_at <= Date.now()) {
      if (token) adminSessions.delete(token);
      return null;
    }
    return session;
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
          rejectUnknownDevice(res, bearer(req));
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
          rejectUnknownDevice(res, bearer(req));
          return true;
        }
        const events = Array.isArray(input.events) ? input.events : [];
        if (events.length > MAX_USAGE_EVENTS_PER_BATCH) {
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
          rejectUnknownDevice(res, bearer(req));
          return true;
        }
        const events = Array.isArray(input.events) ? input.events : [];
        if (events.length > MAX_BEHAVIOR_EVENTS_PER_BATCH) {
          json(res, 413, { error: "too_many_events" });
          return true;
        }
        json(res, 200, store.appendBehaviorEvents(device, events, input.batch_id));
        return true;
      }
      if (req.method === "POST" && routePath === `${STATS_PREFIX}/api/behavior/collection/start`) {
        const input = await readJson(req);
        const device = store.authenticate(input.device_id, bearer(req));
        if (!device) {
          rejectUnknownDevice(res, bearer(req));
          return true;
        }
        json(res, 200, store.startBehaviorCollection(device, input));
        return true;
      }
      if (req.method === "POST" && routePath === `${STATS_PREFIX}/api/behavior/collection/end`) {
        const input = await readJson(req);
        const device = store.authenticate(input.device_id, bearer(req));
        if (!device) {
          rejectUnknownDevice(res, bearer(req));
          return true;
        }
        json(res, 200, store.endBehaviorCollection(device, input));
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/behavior/collection`) {
        const params = new URL(req.url, "http://localhost").searchParams;
        const device = store.authenticate(params.get("device_id"), bearer(req));
        if (!device) {
          rejectUnknownDevice(res, bearer(req));
          return true;
        }
        json(res, 200, store.behaviorCollectionStatus(device));
        return true;
      }
      if (req.method === "POST" && routePath === `${STATS_PREFIX}/api/behavior/deletion`) {
        const input = await readJson(req);
        const token = bearer(req);
        const prior = store.priorDeletionResponse(input.request_id, token);
        if (prior) {
          json(res, 202, prior);
          return true;
        }
        const device = store.authenticate(input.device_id, token);
        if (!device) {
          rejectUnknownDevice(res, token);
          return true;
        }
        json(res, 202, store.deleteBehaviorData(device, input));
        return true;
      }
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/behavior/deletion/status`) {
        const params = new URL(req.url, "http://localhost").searchParams;
        json(res, 200, store.deletionStatus(params.get("deletion_request_id"), bearer(req)));
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
        const role = safeEqual(input.password, adminPassword)
          ? "operations_admin"
          : (behaviorAnalystPassword.length >= 12
            && safeEqual(input.password, behaviorAnalystPassword) ? "behavior_analyst" : null);
        if (!role) {
          json(res, 401, { error: "invalid_credentials" });
          return true;
        }
        const session = randomId("as_", 32);
        adminSessions.set(session, { expires_at: Date.now() + ADMIN_SESSION_MS, role });
        json(res, 200, { ok: true, role }, {
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
      if (req.method === "GET" && routePath === `${STATS_PREFIX}/api/session`) {
        const session = adminAuthorized(req);
        json(res, session ? 200 : 401, session ? { role: session.role } : { error: "unauthorized" });
        return true;
      }
      if (routePath.startsWith(`${STATS_PREFIX}/api/`)) {
        const session = adminAuthorized(req);
        if (!session) {
          json(res, 401, { error: "unauthorized" });
          return true;
        }
        const behaviorRoute = routePath.startsWith(`${STATS_PREFIX}/api/behavior/`)
          || routePath === `${STATS_PREFIX}/api/commercial/overview`;
        if (session.role === "behavior_analyst" && !behaviorRoute) {
          json(res, 403, { error: "forbidden" });
          return true;
        }
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
  return {
    enabled,
    handleHttp,
    close() {
      if (compactionTimer) clearInterval(compactionTimer);
    },
  };
}
