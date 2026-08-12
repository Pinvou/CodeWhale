import assert from "node:assert/strict";
import test from "node:test";
import {
  decryptJoinCode,
  encryptJoinCode,
  newJoinCode,
  normalizeJoinCode,
} from "../src/crypto.js";
import { JoinRateLimiter } from "../src/join-rate-limiter.js";

test("team join codes are short, readable and normalized without breaking legacy codes", () => {
  const code = newJoinCode();
  assert.match(code, /^[23456789ABCDEFGHJKMNPQRSTVWXYZ]{4}-[23456789ABCDEFGHJKMNPQRSTVWXYZ]{4}$/);
  assert.equal(normalizeJoinCode(code.toLowerCase()), code.replace("-", ""));
  assert.equal(normalizeJoinCode(` ${code.slice(0, 4)} ${code.slice(5)} `), code.replace("-", ""));

  const legacyCode = "AbCdEfghijklmnopqrstuvwxyz0123456789_-";
  assert.equal(normalizeJoinCode(legacyCode), legacyCode);
});

test("team join codes can be recovered only with the configured key and team context", () => {
  const key = Buffer.alloc(32, 7).toString("base64url");
  const code = "K7M4-P9Q2";
  const encrypted = encryptJoinCode(code, key, "team-a");
  assert.notEqual(encrypted, code);
  assert.equal(decryptJoinCode(encrypted, key, "team-a"), code);
  assert.throws(() => decryptJoinCode(encrypted, key, "team-b"));
  assert.throws(() => decryptJoinCode(encrypted, Buffer.alloc(32, 8).toString("base64url"), "team-a"));
});

test("join rate limiter blocks repeated failures and resets after success or cooldown", () => {
  let timestamp = 1_000;
  const limiter = new JoinRateLimiter({
    maximumFailures: 2,
    windowMs: 10_000,
    blockMs: 20_000,
    now: () => timestamp,
  });

  limiter.recordFailure("client-a");
  limiter.check("client-a");
  limiter.recordFailure("client-a");
  assert.throws(
    () => limiter.check("client-a"),
    error => error.status === 429
      && error.code === "rate_limited"
      && error.details.retry_after_seconds === 20,
  );

  timestamp += 20_000;
  limiter.check("client-a");
  limiter.recordFailure("client-a");
  limiter.recordSuccess("client-a");
  limiter.check("client-a");
});

test("join rate limiter bounds stale client state", () => {
  const limiter = new JoinRateLimiter({ maximumEntries: 2 });
  limiter.recordFailure("client-a");
  limiter.recordFailure("client-b");
  limiter.recordFailure("client-c");
  assert.equal(limiter.attempts.size, 2);
  assert.equal(limiter.attempts.has("client-a"), false);
});
