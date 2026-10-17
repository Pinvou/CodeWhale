// Only known public examples are rejected here. Byte length does not prove entropy.
const EXAMPLE_KEYS = new Set([
  "replace-with-at-least-32-random-bytes",
  "replace-with-at-least-32-independent-random-bytes",
]);

export function isExampleHmacKey(value) {
  return typeof value === "string" && EXAMPLE_KEYS.has(value.trim());
}
