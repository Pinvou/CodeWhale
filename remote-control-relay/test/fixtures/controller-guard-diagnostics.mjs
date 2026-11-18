import { verifyWindowsAncestors } from "../../relay-state-store.js";

const phases = ["script_started", "payload_decoded", "paths_decoded", "checks_completed"];
const storeCodes = ["relay_state_unavailable", "relay_state_locked", "relay_state_acl_unavailable",
  "relay_state_acl_unverified", "relay_state_path_component_unsafe", "relay_state_path_changed",
  "relay_state_path_verifier_unavailable", "relay_state_directory_unsafe", "relay_state_directory_not_dedicated",
  "relay_state_guard_unavailable", "relay_state_guard_unsafe", "relay_state_guard_identity_changed",
  "relay_state_guard_unsupported", "relay_state_guard_unexpected_journal", "relay_state_permissions_unsafe",
  "relay_state_windows_runtime_unavailable", "relay_state_lock_runtime_unavailable"];
function read(value, field) {
  try { return value?.[field]; } catch { return undefined; }
}
function observe(report, value) {
  try { report(value); } catch { /* Observation cannot replace the original result. */ }
}
function nativeCause(cause) {
  const code = read(cause, "code"), errno = read(cause, "errno");
  const killed = read(cause, "killed"), signal = read(cause, "signal");
  const diagnostic = { code: "unknown" };
  if (["ETIMEDOUT", "EACCES", "EPERM", "ENOENT", "ENOTDIR", "EINVAL",
    "ERR_CHILD_PROCESS_STDIO_MAXBUFFER"].includes(code)) diagnostic.code = code;
  if (Number.isSafeInteger(code) && code >= -2147483648 && code <= 4294967295) diagnostic.exit_code = code;
  if (Number.isSafeInteger(errno) && errno >= -2147483648 && errno <= 2147483647) diagnostic.errno = errno;
  if (typeof killed === "boolean") diagnostic.killed = killed;
  if (["SIGTERM", "SIGKILL", "SIGABRT", "SIGINT"].includes(signal)) diagnostic.signal = signal;
  return diagnostic;
}
export function storeOpenDiagnostic(error) {
  return { stage: "store_open", ...nativeCause(read(error, "cause")) };
}
export function workerFailureDiagnostic(error, workerStage) {
  const code = read(error, "code");
  return { kind: "worker_failure", worker_stage: workerStage === "opening" ? "opening"
    : workerStage === "ready" ? "ready" : "unknown",
  store_code: storeCodes.includes(code) ? code : "unknown", ...nativeCause(read(error, "cause")) };
}
export function observedNativeSecurityAdapters(report, verify = verifyWindowsAncestors, platform = process.platform) {
  return { protectPath: undefined, verifyPath: undefined,
    verifyAncestors: platform !== "win32" ? undefined : async paths => {
      let phase = "unknown";
      try {
        return await verify(paths, { onDiagnostic(value) {
          phase = phases.includes(value) ? value : "unknown";
        } });
      } catch (error) {
        observe(report, { kind: "ancestor_failure", phase });
        throw error;
      }
    } };
}

// Diagnostic frames have their own bounded, per-child cache. They never satisfy
// semantic ready/error/committed waits and are not awaited after child exit.
export function createWorkerDiagnostics() {
  let ancestor, failure;
  return {
    accept(message) {
      if (read(message, "event") !== "controller_guard_diagnostic") return false;
      const kind = read(message, "kind");
      if (kind === "ancestor_failure") {
        const phase = read(message, "phase");
        ancestor = { phase: phases.includes(phase) ? phase : "unknown" };
      }
      if (kind === "worker_failure") {
        const stage = read(message, "worker_stage"), storeCode = read(message, "store_code");
        const code = read(message, "code"), exitCode = read(message, "exit_code");
        const native = nativeCause({ code: typeof code === "string" ? code : undefined,
          errno: read(message, "errno"), killed: read(message, "killed"), signal: read(message, "signal") });
        if (Number.isSafeInteger(exitCode) && exitCode >= -2147483648 && exitCode <= 4294967295) native.exit_code = exitCode;
        failure = { worker_stage: ["opening", "ready"].includes(stage) ? stage : "unknown",
          store_code: storeCodes.includes(storeCode) ? storeCode : "unknown",
          ...native };
      }
      return true;
    },
    snapshot() {
      return { ancestor_failure: ancestor ? { ...ancestor } : "unknown",
        worker_failure: failure ? { ...failure } : "unknown" };
    },
  };
}
