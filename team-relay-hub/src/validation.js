import { invalidRequest } from "./errors.js";

const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/u;

export function objectBody(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw invalidRequest("A JSON object is required.");
  }
  return value;
}

export function exactKeys(value, required, optional = []) {
  objectBody(value);
  const allowed = new Set([...required, ...optional]);
  for (const key of required) {
    if (!Object.hasOwn(value, key)) throw invalidRequest(`Missing field: ${key}`);
  }
  for (const key of Object.keys(value)) {
    if (!allowed.has(key)) throw invalidRequest(`Unknown field: ${key}`);
  }
  return value;
}

export function text(value, name, { minimum = 0, maximum } = {}) {
  if (typeof value !== "string") throw invalidRequest(`${name} must be a string.`);
  const trimmed = value.trim();
  if (trimmed.length < minimum || (maximum !== undefined && value.length > maximum)) {
    throw invalidRequest(`${name} has an invalid length.`);
  }
  return value;
}

export function email(value, name = "email") {
  const normalized = text(value, name, { minimum: 3, maximum: 254 }).trim().toLowerCase();
  if (!EMAIL_PATTERN.test(normalized)) throw invalidRequest(`${name} must be a valid email address.`);
  return normalized;
}

export function uuid(value, name) {
  if (typeof value !== "string" || !UUID_PATTERN.test(value)) {
    throw invalidRequest(`${name} must be a UUID.`);
  }
  return value.toLowerCase();
}

export function uuidArray(value, name, maximum = 10) {
  if (!Array.isArray(value) || value.length > maximum) {
    throw invalidRequest(`${name} must be an array with at most ${maximum} items.`);
  }
  const parsed = value.map((item, index) => uuid(item, `${name}[${index}]`));
  if (new Set(parsed).size !== parsed.length) throw invalidRequest(`${name} must be unique.`);
  return parsed;
}

export function integer(value, name, minimum, maximum) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw invalidRequest(`${name} must be an integer between ${minimum} and ${maximum}.`);
  }
  return value;
}

export function boolean(value, name) {
  if (typeof value !== "boolean") throw invalidRequest(`${name} must be a boolean.`);
  return value;
}
