import { parseArgs } from "node:util";
import { isDirectCliEntry } from "../cli-entrypoint.js";
import { RelayStateStore, RelayStateStoreError } from "../relay-state-store.js";

if (isDirectCliEntry(import.meta.url)) {
  try {
    const { values } = parseArgs({
      options: {
        "confirmed-primary-revision": { type: "string" },
        "primary-sha256": { type: "string" },
        "staged-backup-sha256": { type: "string" },
      },
    });
    const result = await RelayStateStore.recoverStagedBackup({
      statePath: process.env.PINVOU_REMOTE_CONTROLLER_STATE_PATH,
      legacyStatePath: process.env.PINVOU_REMOTE_STATE_PATH,
    }, {
      primaryRevision: values["confirmed-primary-revision"],
      primarySha256: values["primary-sha256"],
      backupSha256: values["staged-backup-sha256"],
    });
    console.log(JSON.stringify(result));
  } catch (error) {
    console.error(error instanceof RelayStateStoreError ? error.code : "relay_state_recovery_failed");
    process.exitCode = 1;
  }
}
