#!/usr/bin/env node
import { constants as fsConstants } from "node:fs";
import { lstat, open, readFile, rename, rm } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { loadEnrollmentRegistryConfig, EnrollmentRegistry } from "../enrollment-registry.js";
import { RelayStateStore } from "../relay-state-store.js";
import {
  EnrollmentMigrationHandler,
  createEnrollmentBackup,
  restoreEnrollmentBackup,
  runEnrollmentBatch,
} from "../enrollment-migration-handler.js";

const MAX_INPUT_BYTES = 4 * 1024 * 1024;

function cliError(code) {
  const error = new Error(code);
  error.code = code;
  return error;
}

function parseArguments(argv) {
  const result = { mode: null, input: null, output: null };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (["--dry-run", "--apply", "--backup", "--restore", "--coverage"].includes(argument)) {
      if (result.mode) throw cliError("mode_conflict");
      result.mode = argument.slice(2);
    } else if (argument === "--input" || argument === "--output") {
      const value = argv[++index];
      if (!value || value.startsWith("--")) throw cliError("argument_invalid");
      result[argument.slice(2)] = resolve(value);
    } else if (argument === "--help") {
      result.mode = "help";
    } else {
      throw cliError("argument_invalid");
    }
  }
  return result;
}

async function secureReadJson(path) {
  const metadata = await lstat(path);
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size > MAX_INPUT_BYTES) {
    throw cliError("input_file_unsafe");
  }
  if (process.platform !== "win32" && (metadata.mode & 0o077) !== 0) throw cliError("input_permissions_unsafe");
  const handle = await open(path, fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW || 0));
  try {
    const afterOpen = await handle.stat();
    if (!afterOpen.isFile() || afterOpen.size > MAX_INPUT_BYTES
      || afterOpen.dev !== metadata.dev || afterOpen.ino !== metadata.ino) throw cliError("input_file_changed");
    const contents = await handle.readFile({ encoding: "utf8" });
    return JSON.parse(contents);
  } finally {
    await handle.close();
  }
}

async function atomicWriteJson(path, value) {
  const temporary = `${path}.${process.pid}.tmp`;
  const handle = await open(temporary, "wx", 0o600);
  try {
    await handle.writeFile(`${JSON.stringify(value, null, 2)}\n`, "utf8");
    await handle.sync();
  } catch (error) {
    await handle.close().catch(() => {});
    await rm(temporary, { force: true }).catch(() => {});
    throw error;
  }
  await handle.close();
  await rename(temporary, path);
  const directory = await open(dirname(path), "r");
  try { await directory.sync(); } finally { await directory.close(); }
}

function publicFailure(error) {
  return { ok: false, code: error?.code || "migration_failed" };
}

export async function main(argv = process.argv.slice(2), env = process.env, io = console) {
  let store;
  try {
    const args = parseArguments(argv);
    if (args.mode === "help") {
      io.log(JSON.stringify({
        usage: "enrollment-migrate.js (--dry-run|--apply|--backup|--restore|--coverage) [--input FILE] [--output FILE]",
        note: "credentials are accepted only in a mode-0600 input file, never as command arguments",
      }));
      return 0;
    }
    if (!args.mode) throw cliError("mode_required");
    const config = loadEnrollmentRegistryConfig(env);
    if (["dry-run", "apply"].includes(args.mode) && config.deployment !== "self_hosted") {
      throw cliError("self_hosted_mode_required");
    }
    store = await RelayStateStore.open({
      statePath: config.controller_state_path,
      legacyStatePath: config.legacy_web_state_path,
    });
    const registry = EnrollmentRegistry.open({ stateStore: store, config });
    let result;
    if (args.mode === "coverage") {
      if (!args.input || args.output) throw cliError("coverage_arguments_invalid");
      const document = await secureReadJson(args.input);
      if (document?.schema !== "pinvou-legacy-endpoint-inventory" || document?.version !== 1) {
        throw cliError("inventory_document_invalid");
      }
      const handler = new EnrollmentMigrationHandler({ registry });
      result = { ok: true, ...handler.coverage(document.endpoint_ids) };
    } else if (args.mode === "backup") {
      if (!args.output || args.input) throw cliError("backup_arguments_invalid");
      result = createEnrollmentBackup(registry);
      await atomicWriteJson(args.output, result);
      result = { ok: true, backup_written: true, namespace_revision: result.namespace_revision };
    } else if (args.mode === "restore") {
      if (!args.input || args.output) throw cliError("restore_arguments_invalid");
      const backup = await secureReadJson(args.input);
      result = {
        ok: true,
        ...(await restoreEnrollmentBackup({
          registry,
          backup,
        })),
      };
    } else {
      if (!args.input || args.output) throw cliError("batch_arguments_invalid");
      const document = await secureReadJson(args.input);
      if (document?.schema !== "pinvou-enrollment-migration-batch" || document?.version !== 1) {
        throw cliError("batch_document_invalid");
      }
      result = {
        ok: true,
        ...(await runEnrollmentBatch({
          registry,
          operations: document.operations,
          dryRun: args.mode === "dry-run",
        })),
      };
    }
    io.log(JSON.stringify(result));
    return 0;
  } catch (error) {
    io.error(JSON.stringify(publicFailure(error)));
    return 1;
  } finally {
    await store?.close().catch(() => {});
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
  process.exitCode = await main();
}
