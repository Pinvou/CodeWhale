import { AppError } from "./errors.js";

const MINUTE_MS = 60 * 1000;

export class JoinRateLimiter {
  constructor({
    maximumFailures = 5,
    windowMs = 10 * MINUTE_MS,
    blockMs = 15 * MINUTE_MS,
    maximumEntries = 10_000,
    now = () => Date.now(),
  } = {}) {
    this.maximumFailures = maximumFailures;
    this.windowMs = windowMs;
    this.blockMs = blockMs;
    this.maximumEntries = maximumEntries;
    this.now = now;
    this.attempts = new Map();
  }

  check(key) {
    const timestamp = this.now();
    const attempt = this.attempts.get(key);
    if (!attempt) return;
    if (attempt.blockedUntil > timestamp) {
      const retryAfterSeconds = Math.max(1, Math.ceil((attempt.blockedUntil - timestamp) / 1000));
      throw new AppError(
        429,
        "rate_limited",
        "Too many invalid team join code attempts.",
        { retry_after_seconds: retryAfterSeconds },
      );
    }
    if (attempt.windowStartedAt + this.windowMs <= timestamp) this.attempts.delete(key);
  }

  recordFailure(key) {
    const timestamp = this.now();
    for (const [entryKey, entry] of this.attempts) {
      if (entry.blockedUntil <= timestamp && entry.windowStartedAt + this.windowMs <= timestamp) {
        this.attempts.delete(entryKey);
      }
    }
    if (!this.attempts.has(key) && this.attempts.size >= this.maximumEntries) {
      this.attempts.delete(this.attempts.keys().next().value);
    }
    let attempt = this.attempts.get(key);
    if (!attempt || attempt.windowStartedAt + this.windowMs <= timestamp) {
      attempt = { failures: 0, windowStartedAt: timestamp, blockedUntil: 0 };
    }
    attempt.failures += 1;
    if (attempt.failures >= this.maximumFailures) attempt.blockedUntil = timestamp + this.blockMs;
    this.attempts.set(key, attempt);
  }

  recordSuccess(key) {
    this.attempts.delete(key);
  }
}
