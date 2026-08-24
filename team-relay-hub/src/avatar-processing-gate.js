import { AppError } from "./errors.js";

export class AvatarUploadGate {
  constructor({
    maximumAttempts = 6,
    windowMs = 60_000,
    maximumEntries = 10_000,
    now = () => Date.now(),
  } = {}) {
    for (const [name, value] of Object.entries({
      maximumAttempts,
      windowMs,
      maximumEntries,
    })) {
      if (!Number.isSafeInteger(value) || value < 1) {
        throw new TypeError(`${name} must be a positive safe integer`);
      }
    }
    this.maximumAttempts = maximumAttempts;
    this.windowMs = windowMs;
    this.maximumEntries = maximumEntries;
    this.now = now;
    this.activePrincipals = new Set();
    this.attempts = new Map();
  }

  #recordAttempt(principalId) {
    const timestamp = this.now();
    // New windows are inserted at the Map tail, so expired windows form a
    // prefix. Removing only that prefix makes normal admission amortized O(1).
    while (this.attempts.size > 0) {
      const [oldestKey, oldest] = this.attempts.entries().next().value;
      if (oldest.windowStartedAt + this.windowMs > timestamp) break;
      this.attempts.delete(oldestKey);
    }
    let entry = this.attempts.get(principalId);
    if (!entry || entry.windowStartedAt + this.windowMs <= timestamp) {
      if (!this.attempts.has(principalId) && this.attempts.size >= this.maximumEntries) {
        this.attempts.delete(this.attempts.keys().next().value);
      }
      entry = { count: 0, windowStartedAt: timestamp };
      // Deleting before set also preserves monotonic insertion order when the
      // clock/test seam advances exactly as an old entry expires.
      this.attempts.delete(principalId);
    }
    if (entry.count >= this.maximumAttempts) {
      const retryAfterSeconds = Math.max(
        1,
        Math.ceil((entry.windowStartedAt + this.windowMs - timestamp) / 1000),
      );
      throw new AppError(
        429,
        "avatar_upload_rate_limited",
        "Too many avatar uploads. Retry later.",
        { retry_after_seconds: retryAfterSeconds },
      );
    }
    entry.count += 1;
    this.attempts.set(principalId, entry);
  }

  enter(principalId) {
    if (this.activePrincipals.has(principalId)) {
      throw new AppError(
        409,
        "avatar_upload_in_progress",
        "An avatar upload is already in progress for this account.",
      );
    }
    this.#recordAttempt(principalId);
    this.activePrincipals.add(principalId);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.activePrincipals.delete(principalId);
    };
  }
}

export class AvatarProcessingGate {
  constructor({ maximumConcurrent = 2 } = {}) {
    if (!Number.isSafeInteger(maximumConcurrent) || maximumConcurrent < 1) {
      throw new TypeError("maximumConcurrent must be a positive safe integer");
    }
    this.maximumConcurrent = maximumConcurrent;
    this.active = 0;
  }

  enter() {
    if (this.active >= this.maximumConcurrent) {
      throw new AppError(
        429,
        "avatar_processing_busy",
        "Avatar processing is busy. Retry shortly.",
        { retry_after_seconds: 2 },
      );
    }
    this.active += 1;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.active -= 1;
    };
  }
}
