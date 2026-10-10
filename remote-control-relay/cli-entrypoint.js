import { realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";

/** Node realpaths ESM modules but retains the invocation alias in argv[1].
 * Compare both actual paths, including Windows junctions and path casing.
 * This identifies script execution only; it never relaxes data-path checks.
 */
export function isDirectCliEntry(moduleUrl, entry = process.argv[1]) {
  if (!entry) return false;
  try {
    return realpathSync.native(entry) === realpathSync.native(fileURLToPath(moduleUrl));
  } catch {
    // An unreadable entry must not turn a safety CLI into a successful no-op.
    throw Object.assign(new Error("cli_entrypoint_unreadable"), { code: "cli_entrypoint_unreadable" });
  }
}
