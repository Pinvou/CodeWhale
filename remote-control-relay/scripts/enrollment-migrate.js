#!/usr/bin/env node
import { constants as fsConstants } from "node:fs";
import { lstat, open, rename, rm } from "node:fs/promises";
import { dirname, join, parse, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { loadEnrollmentRegistryConfig, EnrollmentRegistry } from "../enrollment-registry.js";
import {
  protectWindowsPath,
  RelayStateStore,
  verifyWindowsAncestors,
  verifyWindowsPath,
} from "../relay-state-store.js";
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

function exactDocument(value, keys, code) {
  if (!value || typeof value !== "object" || Array.isArray(value)
    || Object.getPrototypeOf(value) !== Object.prototype
    || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) {
    throw cliError(code);
  }
  return value;
}

function parseArguments(argv) {
  const result = { mode: null, input: null, output: null, requestId: undefined };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (["--dry-run", "--apply", "--backup", "--restore", "--coverage"].includes(argument)) {
      if (result.mode) throw cliError("mode_conflict");
      result.mode = argument.slice(2);
    } else if (argument === "--input" || argument === "--output") {
      const value = argv[++index];
      if (!value || value.startsWith("--")) throw cliError("argument_invalid");
      result[argument.slice(2)] = resolve(value);
    } else if (argument === "--request-id") {
      const value = argv[++index];
      if (!value || value.startsWith("--") || result.requestId !== undefined) throw cliError("argument_invalid");
      result.requestId = value;
    } else if (argument === "--help") {
      result.mode = "help";
    } else {
      throw cliError("argument_invalid");
    }
  }
  return result;
}

async function secureReadJson(path) {
  await verifyPrivateAncestors(path);
  const metadata = await lstat(path);
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size > MAX_INPUT_BYTES) {
    throw cliError("input_file_unsafe");
  }
  if (process.platform !== "win32" && (metadata.mode & 0o077) !== 0) throw cliError("input_permissions_unsafe");
  if (process.platform === "win32") {
    try { await verifyWindowsPath(path, "file"); } catch { throw cliError("input_permissions_unsafe"); }
  }
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

async function verifyPrivateAncestors(path) {
  const absolute = resolve(path);
  const root = parse(absolute).root;
  const relative = absolute.slice(root.length).split(/[\\/]+/u).filter(Boolean);
  const paths = [];
  let current = root;
  for (const component of relative.slice(0, -1)) {
    current = join(current, component);
    const metadata = await lstat(current);
    if (!metadata.isDirectory() || metadata.isSymbolicLink()) throw cliError("operator_path_unsafe");
    paths.push(current);
  }
  if (process.platform === "win32") {
    try { await verifyWindowsAncestors(paths); } catch { throw cliError("operator_path_unsafe"); }
  }
}

async function atomicWriteJson(path, value) {
  await verifyPrivateAncestors(path);
  try {
    const existing = await lstat(path);
    if (!existing.isFile() || existing.isSymbolicLink()) throw cliError("output_file_unsafe");
    if (process.platform === "win32") await verifyWindowsPath(path, "file");
    else if ((existing.mode & 0o077) !== 0) throw cliError("output_permissions_unsafe");
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  const temporary = `${path}.${process.pid}.tmp`;
  const handle = await open(temporary, "wx", 0o600);
  try {
    if (process.platform === "win32") {
      await protectWindowsPath(temporary, "file");
      await verifyWindowsPath(temporary, "file");
    } else {
      await handle.chmod(0o600);
    }
    await handle.writeFile(`${JSON.stringify(value, null, 2)}\n`, "utf8");
    await handle.sync();
  } catch (error) {
    await handle.close().catch(() => {});
    await rm(temporary, { force: true }).catch(() => {});
    throw error;
  }
  await handle.close();
  try {
    await rename(temporary, path);
  } catch (error) {
    await rm(temporary, { force: true }).catch(() => {});
    throw error;
  }
  if (process.platform === "win32") await verifyWindowsPath(path, "file");
  else if (((await lstat(path)).mode & 0o077) !== 0) throw cliError("output_permissions_unsafe");
  if (process.platform !== "win32") {
    const directory = await open(dirname(path), "r");
    try { await directory.sync(); } finally { await directory.close(); }
  }
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
        usage: "enrollment-migrate.js (--dry-run|--apply|--backup|--restore|--coverage) [--input FILE] [--output FILE] [--request-id RESTORE_OPERATION_ID]",
        note: "credentials are accepted only in a mode-0600 input file, never as command arguments",
      }));
      return 0;
    }
    if (!args.mode) throw cliError("mode_required");
    if (args.requestId !== undefined && args.mode !== "restore") throw cliError("restore_request_id_only");
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
      exactDocument(document, ["schema", "version", "endpoint_ids"], "inventory_document_invalid");
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
      if (!args.input || args.output || args.requestId === undefined) throw cliError("restore_arguments_invalid");
      const backup = await secureReadJson(args.input);
      result = {
        ok: true,
        ...(await restoreEnrollmentBackup({
          registry,
          backup,
          requestId: args.requestId,
        })),
      };
    } else {
      if (!args.input || args.output) throw cliError("batch_arguments_invalid");
      const document = await secureReadJson(args.input);
      exactDocument(document, ["schema", "version", "request_id", "operations"], "batch_document_invalid");
      if (document?.schema !== "pinvou-enrollment-migration-batch" || document?.version !== 1
        || typeof document?.request_id !== "string") {
        throw cliError("batch_document_invalid");
      }
      result = {
        ok: true,
        ...(await runEnrollmentBatch({
          registry,
          operations: document.operations,
          dryRun: args.mode === "dry-run",
          requestId: document.request_id,
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
