import { createCipheriv, createDecipheriv, createHash, randomBytes, randomUUID } from "node:crypto";

const JOIN_CODE_ALPHABET = "23456789ABCDEFGHJKMNPQRSTVWXYZ";
const JOIN_CODE_LENGTH = 8;

export function newId() {
  return randomUUID();
}

export function newSecret() {
  return randomBytes(32).toString("base64url");
}

export function newJoinCode() {
  const characters = [];
  const unbiasedLimit = Math.floor(256 / JOIN_CODE_ALPHABET.length) * JOIN_CODE_ALPHABET.length;
  while (characters.length < JOIN_CODE_LENGTH) {
    for (const byte of randomBytes(JOIN_CODE_LENGTH)) {
      if (byte >= unbiasedLimit) continue;
      characters.push(JOIN_CODE_ALPHABET[byte % JOIN_CODE_ALPHABET.length]);
      if (characters.length === JOIN_CODE_LENGTH) break;
    }
  }
  const compact = characters.join("");
  return `${compact.slice(0, 4)}-${compact.slice(4)}`;
}

export function normalizeJoinCode(value) {
  const trimmed = String(value).trim();
  const compact = trimmed.replace(/[\s-]/g, "").toUpperCase();
  const shortCodePattern = new RegExp(`^[${JOIN_CODE_ALPHABET}]{${JOIN_CODE_LENGTH}}$`);
  return shortCodePattern.test(compact) ? compact : trimmed;
}

function decodeEncryptionKey(encodedKey) {
  const key = Buffer.from(String(encodedKey || ""), "base64url");
  if (key.length !== 32) throw new Error("team join code encryption key must contain 32 bytes");
  return key;
}

export function encryptJoinCode(value, encodedKey, context) {
  const iv = randomBytes(12);
  const cipher = createCipheriv("aes-256-gcm", decodeEncryptionKey(encodedKey), iv);
  cipher.setAAD(Buffer.from(context));
  const ciphertext = Buffer.concat([cipher.update(value, "utf8"), cipher.final()]);
  return `v1.${iv.toString("base64url")}.${ciphertext.toString("base64url")}.${cipher.getAuthTag().toString("base64url")}`;
}

export function decryptJoinCode(value, encodedKey, context) {
  const [version, iv, ciphertext, tag] = String(value || "").split(".");
  if (version !== "v1" || !iv || !ciphertext || !tag) throw new Error("team join code ciphertext is invalid");
  const decipher = createDecipheriv("aes-256-gcm", decodeEncryptionKey(encodedKey), Buffer.from(iv, "base64url"));
  decipher.setAAD(Buffer.from(context));
  decipher.setAuthTag(Buffer.from(tag, "base64url"));
  return Buffer.concat([decipher.update(Buffer.from(ciphertext, "base64url")), decipher.final()]).toString("utf8");
}

export function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value).sort().map((key) => [key, canonicalize(value[key])]),
    );
  }
  return value;
}

export function requestHash(value) {
  return sha256(JSON.stringify(canonicalize(value)));
}
