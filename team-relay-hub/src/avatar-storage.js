import { isAbsolute, relative, resolve, sep } from "node:path";

const STORAGE_KEY_PATTERN = /^[0-9a-f]{2}\/[0-9a-f-]{36}$/;

export function isPathInside(root, candidate, pathApi = { isAbsolute, relative, sep }) {
  const relativePath = pathApi.relative(root, candidate);
  return relativePath !== ""
    && relativePath !== ".."
    && !relativePath.startsWith(`..${pathApi.sep}`)
    && !pathApi.isAbsolute(relativePath);
}

export function avatarRoot(config) {
  return resolve(config.attachmentDir, "avatars");
}

export function avatarStoragePath(config, storageKey) {
  if (!STORAGE_KEY_PATTERN.test(storageKey)) throw new Error("invalid avatar storage key");
  const root = avatarRoot(config);
  const candidate = resolve(root, storageKey);
  if (!isPathInside(root, candidate, { isAbsolute, relative, sep })) {
    throw new Error("avatar path escaped storage root");
  }
  return candidate;
}
