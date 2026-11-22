import { spawnSync } from "node:child_process";

// A timeout may follow writes or a service-stop request. Preserve that first
// result instead of replaying an operation whose outcome is unknown.
export function spawnSyncOnce(command, args, options, execute = spawnSync) {
  return execute(command, args, options);
}

// Keep existing test callers compatible; this legacy name no longer retries.
export const spawnSyncWithRetry = spawnSyncOnce;
