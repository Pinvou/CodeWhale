import { parseArgs } from "node:util";
import { isDirectCliEntry } from "./cli-entrypoint.js";
import { RelayStateStore, RelayStateStoreError } from "./relay-state-store.js";

const USAGE = `Usage: node recover-controller-state.js [--help]
  --confirmed-primary-revision <revision>
  --primary-sha256 <sha256>
  --staged-backup-sha256 <sha256>
  --archive-root <existing-private-directory>

Stop the Relay and establish the committed primary from external evidence first.
PINVOU_REMOTE_CONTROLLER_STATE_PATH and PINVOU_REMOTE_STATE_PATH select the store.
The default archive root is the state directory's parent; it must also be private.
Store recovery requires Node.js 24 or newer with built-in SQLite.`;

function failureDiagnostic(error, stage) {
  const read = (object, key) => {
    try { return object?.[key]; } catch { return undefined; }
  };
  const allowedCodes = [
    "ERR_PARSE_ARGS_UNKNOWN_OPTION", "ERR_PARSE_ARGS_INVALID_OPTION_VALUE",
    "ERR_PARSE_ARGS_UNEXPECTED_POSITIONAL", "ERR_INVALID_ARG_TYPE", "ERR_INVALID_ARG_VALUE",
    "ERR_UNKNOWN_BUILTIN_MODULE", "ERR_ENCODING_INVALID_ENCODED_DATA",
    "EACCES", "EPERM", "ENOENT", "ENOTDIR", "EISDIR", "EIO", "ENOSPC", "EROFS", "EBUSY",
  ];
  const code = read(error, "code");
  const name = read(error, "name");
  const causeCode = read(read(error, "cause"), "code");
  return {
    stage,
    name: ["Error", "TypeError", "RangeError", "SyntaxError", "AggregateError"].includes(name) ? name : "Error",
    code: allowedCodes.includes(code) ? code : "unknown",
    cause_code: allowedCodes.includes(causeCode) ? causeCode : "unknown",
    ...(stage === "arguments" ? { help: "node recover-controller-state.js --help" } : {}),
  };
}

if (isDirectCliEntry(import.meta.url)) {
  let stage = "arguments";
  try {
    const { values } = parseArgs({
      options: {
        help: { type: "boolean" },
        "confirmed-primary-revision": { type: "string" },
        "primary-sha256": { type: "string" },
        "staged-backup-sha256": { type: "string" },
        "archive-root": { type: "string" },
      },
    });
    if (values.help) {
      console.log(USAGE);
    } else {
      stage = "recovery";
      const result = await RelayStateStore.recoverStagedBackup({
        statePath: process.env.PINVOU_REMOTE_CONTROLLER_STATE_PATH,
        legacyStatePath: process.env.PINVOU_REMOTE_STATE_PATH,
      }, {
        primaryRevision: values["confirmed-primary-revision"],
        primarySha256: values["primary-sha256"],
        backupSha256: values["staged-backup-sha256"],
        archiveRoot: values["archive-root"],
      });
      console.log(JSON.stringify(result));
    }
  } catch (error) {
    process.exitCode = 1;
    const code = error instanceof RelayStateStoreError ? error.code : "relay_state_recovery_failed";
    if (error instanceof RelayStateStoreError && error.recoveryArchive) {
      console.error(JSON.stringify({ code, archive: error.recoveryArchive }));
    } else {
      console.error(code);
    }
    if (!(error instanceof RelayStateStoreError)) {
      // Keep the existing public failure code, with closed diagnostic fields.
      // Never read or print arbitrary message/stack/path/command/frame content.
      try { console.error(JSON.stringify(failureDiagnostic(error, stage))); }
      catch { /* Reporting cannot turn the recovery failure into success. */ }
    }
  }
}
