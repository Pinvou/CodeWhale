import { createHash, randomUUID } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import {
  appendFile,
  chmod,
  lstat,
  mkdir,
  open,
  readFile,
  readdir,
  rename,
  rm,
  stat,
} from "node:fs/promises";
import { basename, dirname, join } from "node:path";
import { pipeline } from "node:stream/promises";
import { Transform } from "node:stream";
import Busboy from "busboy";
import { AppError, badRequest, conflict } from "./errors.js";
import { inspectPeInstaller } from "./pe-inspector.js";

export const WINDOWS_PLATFORM = "windows-x86_64";
const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:\.\d+)?$/u;
const HASH_PATTERN = /^[0-9a-f]{64}$/u;
const COMMIT_PATTERN = /^[0-9a-f]{7,40}$/u;
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u;
const STAGING_FILE_PATTERN = /^[0-9a-f-]{36}\.upload$/u;
const PUBLIC_TMP_PATTERN = /^\.pinvou3-release-[0-9a-f-]{36}\.tmp$/u;
const MANIFEST_TMP_PATTERN = /^latest\.json\.[0-9a-f-]{36}\.tmp$/u;
const DRAFT_TMP_PATTERN = /^windows-draft\.json\.[0-9a-f-]{36}\.tmp$/u;
const HISTORY_TMP_PATTERN = /^\d+-[0-9a-f-]{36}\.json\.[0-9a-f-]{36}\.tmp$/u;

function jsonClone(value) {
  return value === undefined ? undefined : JSON.parse(JSON.stringify(value));
}

function sameJson(left, right) {
  return JSON.stringify(left ?? null) === JSON.stringify(right ?? null);
}

function sameReleaseAsset(left, right) {
  return Boolean(
    left
    && right
    && left.version === right.version
    && left.url === right.url
    && left.format === right.format
    && left.sha256 === right.sha256
    && left.size === right.size,
  );
}

function validateText(value, field, maxLength, { multiline = false } = {}) {
  if (typeof value !== "string" || !value.trim() || value.length > maxLength) {
    throw badRequest("invalid_field", `${field} 必填且不能超过 ${maxLength} 个字符`);
  }
  const invalidControl = multiline
    ? /[\u0000-\u0009\u000b\u000c\u000e-\u001f\u007f-\u009f]/u
    : /\p{Cc}/u;
  if (invalidControl.test(value)) throw badRequest("invalid_field", `${field} 包含非法控制字符`);
  return value.trim();
}

export function parseVersion(value) {
  if (typeof value !== "string" || !VERSION_PATTERN.test(value)) {
    throw badRequest("invalid_version", "版本号必须是三段或四段纯数字");
  }
  const parts = value.split(".").map(Number);
  if (parts.some((part) => !Number.isSafeInteger(part))) {
    throw badRequest("invalid_version", "版本号数字段超出安全范围");
  }
  return parts;
}

export function compareVersions(left, right) {
  const a = parseVersion(left);
  const b = parseVersion(right);
  const length = Math.max(a.length, b.length);
  for (let index = 0; index < length; index += 1) {
    const difference = (a[index] ?? 0) - (b[index] ?? 0);
    if (difference !== 0) return Math.sign(difference);
  }
  return 0;
}

export function mergeWindowsEntry(manifest, entry) {
  if (!manifest || typeof manifest !== "object" || Array.isArray(manifest)) {
    throw new AppError(500, "invalid_manifest", "现有 latest.json 不是 JSON 对象");
  }
  const merged = jsonClone(manifest);
  if (!merged.platforms || typeof merged.platforms !== "object" || Array.isArray(merged.platforms)) {
    merged.platforms = {};
  }
  merged.platforms[WINDOWS_PLATFORM] = jsonClone(entry);
  return merged;
}

async function readJson(path, { missing = undefined } = {}) {
  let text;
  try {
    text = await readFile(path, "utf8");
  } catch (error) {
    if (error.code === "ENOENT" && missing !== undefined) return jsonClone(missing);
    throw error;
  }
  try {
    return JSON.parse(text);
  } catch {
    throw new AppError(500, "invalid_json", `${basename(path)} 不是有效 JSON`);
  }
}

async function atomicWriteJson(path, value, mode = 0o600) {
  const temporaryPath = `${path}.${randomUUID()}.tmp`;
  const contents = `${JSON.stringify(value, null, 2)}\n`;
  const handle = await open(temporaryPath, "wx", mode);
  try {
    await handle.writeFile(contents, "utf8");
    // open(2) applies the process umask to `mode`. The production service runs
    // with UMask=0077, so public manifests requested as 0644 would otherwise
    // become 0600 and Nginx could not read them. fchmod applies the final mode
    // explicitly while keeping private state files at the default 0600.
    await handle.chmod(mode);
    await handle.sync();
  } finally {
    await handle.close();
  }
  try {
    await rename(temporaryPath, path);
    await syncDirectory(dirname(path));
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }
}

async function syncDirectory(path) {
  // Node can open a directory handle on Windows, but FlushFileBuffers on that
  // handle may never complete. Directory fsync is the Linux durability
  // primitive used by the production container; skip it before opening there.
  if (process.platform === "win32") return;
  let handle;
  try {
    handle = await open(path, "r");
    await handle.sync();
  } finally {
    await handle?.close();
  }
}

async function hashFile(path) {
  const hash = createHash("sha256");
  let size = 0;
  for await (const chunk of createReadStream(path)) {
    size += chunk.length;
    hash.update(chunk);
  }
  return { sha256: hash.digest("hex"), size };
}

function sameHashAndSize(actual, expected) {
  return actual.sha256 === expected.sha256 && actual.size === expected.size;
}

async function copyStagedAsset(stagingPath, destinationPath, expected) {
  const publicDirectory = dirname(destinationPath);
  const temporaryPath = join(publicDirectory, `.pinvou3-release-${randomUUID()}.tmp`);
  const streamedHash = createHash("sha256");
  let streamedSize = 0;
  try {
    const inspect = new Transform({
      transform(chunk, _encoding, callback) {
        streamedSize += chunk.length;
        streamedHash.update(chunk);
        callback(null, chunk);
      },
    });
    await pipeline(
      createReadStream(stagingPath),
      inspect,
      createWriteStream(temporaryPath, { flags: "wx", mode: 0o600 }),
    );
    await syncFile(temporaryPath);

    const streamed = { sha256: streamedHash.digest("hex"), size: streamedSize };
    if (!sameHashAndSize(streamed, expected)) {
      throw conflict("staged_file_changed", "暂存安装器在复制期间发生变化");
    }
    const copied = await hashFile(temporaryPath);
    if (!sameHashAndSize(copied, expected)) {
      throw conflict("public_copy_verification_failed", "发布目录临时安装器回读校验失败");
    }

    if (await pathExists(destinationPath)) {
      const existing = await hashFile(destinationPath);
      if (!sameHashAndSize(existing, expected)) {
        throw conflict("immutable_asset_exists", "线上已有同名但内容不同的安装器，拒绝覆盖");
      }
      await rm(temporaryPath, { force: true });
      await chmod(destinationPath, 0o644);
      await syncFile(destinationPath);
      await syncDirectory(publicDirectory);
    } else {
      // temporaryPath 与 destinationPath 同属 publicRoot；这里只做同目录原子切换，
      // 不再依赖 stateRoot 与 publicRoot 是否位于同一文件系统。
      await chmod(temporaryPath, 0o644);
      await syncFile(temporaryPath);
      await rename(temporaryPath, destinationPath);
      await syncDirectory(publicDirectory);
    }
    const published = await hashFile(destinationPath);
    if (!sameHashAndSize(published, expected)) {
      throw conflict("online_verification_failed", "发布目录中的安装器回读校验失败");
    }
    await rm(stagingPath, { force: true });
    await syncDirectory(dirname(stagingPath));
    return published;
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }
}

export async function verifyOnlineAsset(url, expected, timeoutMs) {
  let response;
  try {
    response = await fetch(url, {
      cache: "no-store",
      headers: { "Accept-Encoding": "identity" },
      redirect: "error",
      signal: AbortSignal.timeout(timeoutMs),
    });
  } catch (error) {
    throw new AppError(502, "online_asset_unavailable", `无法通过 HTTPS 回读安装器：${error.message}`);
  }
  if (!response.ok || !response.body) {
    throw new AppError(502, "online_asset_unavailable", `HTTPS 回读安装器失败：HTTP ${response.status}`);
  }
  const contentLength = response.headers.get("content-length");
  if (contentLength && Number(contentLength) !== expected.size) {
    await response.body.cancel();
    throw conflict("online_size_mismatch", `线上安装器大小不一致：期望 ${expected.size}，实际 ${contentLength}`);
  }

  const hash = createHash("sha256");
  let size = 0;
  try {
    for await (const chunk of response.body) {
      size += chunk.length;
      if (size > expected.size) {
        throw conflict("online_size_mismatch", `线上安装器大于期望值 ${expected.size}`);
      }
      hash.update(chunk);
    }
  } catch (error) {
    if (error instanceof AppError) throw error;
    throw new AppError(502, "online_asset_unavailable", `HTTPS 回读安装器中断：${error.message}`);
  }
  const sha256 = hash.digest("hex");
  if (size !== expected.size) {
    throw conflict("online_size_mismatch", `线上安装器大小不一致：期望 ${expected.size}，实际 ${size}`);
  }
  if (sha256 !== expected.sha256) {
    throw conflict("online_sha256_mismatch", `线上安装器 SHA-256 不一致：实际值为 ${sha256}`);
  }
  return { size, sha256 };
}

async function pathExists(path) {
  try {
    await stat(path);
    return true;
  } catch (error) {
    if (error.code === "ENOENT") return false;
    throw error;
  }
}

async function syncFile(path) {
  // Windows only permits FlushFileBuffers through a writable handle.
  const handle = await open(path, "r+");
  try {
    await handle.sync();
  } finally {
    await handle.close();
  }
}

async function requireDirectory(path, label) {
  let metadata;
  try {
    metadata = await lstat(path);
  } catch (error) {
    throw new AppError(503, "not_ready", `${label} 不可访问：${error.message}`);
  }
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new AppError(503, "not_ready", `${label} 必须是实际目录`);
  }
}

async function requirePublicFile(path, label) {
  let metadata;
  try {
    metadata = await lstat(path);
  } catch (error) {
    throw new AppError(503, "not_ready", `${label} 不可访问：${error.message}`);
  }
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new AppError(503, "not_ready", `${label} 必须是实际普通文件`);
  }
  if (process.platform !== "win32" && (metadata.mode & 0o777) !== 0o644) {
    throw new AppError(503, "not_ready", `${label} 权限必须为 0644`);
  }
}

async function removeOwnedTemporaryFiles(directory, patterns) {
  for (const name of await readdir(directory)) {
    if (patterns.some((pattern) => pattern.test(name))) {
      await rm(join(directory, name), { force: true });
    }
  }
}

async function verifyWritableDirectory(path, label) {
  const probe = join(path, `.pinvou3-write-probe-${randomUUID()}.tmp`);
  let handle;
  try {
    handle = await open(probe, "wx", 0o600);
    await handle.writeFile("ready\n", "utf8");
    await handle.sync();
  } catch (error) {
    throw new AppError(503, "not_ready", `${label} 不可写：${error.message}`);
  } finally {
    await handle?.close().catch(() => {});
    await rm(probe, { force: true }).catch(() => {});
  }
}

function safeUploadFields(fields) {
  const version = validateText(fields.version, "version", 32);
  parseVersion(version);
  const expectedSha256 = validateText(fields.expectedSha256, "expectedSha256", 64).toLowerCase();
  if (!HASH_PATTERN.test(expectedSha256)) {
    throw badRequest("invalid_sha256", "expectedSha256 必须是 64 位十六进制 SHA-256");
  }
  const gitCommit = validateText(fields.gitCommit, "gitCommit", 40).toLowerCase();
  if (!COMMIT_PATTERN.test(gitCommit)) {
    throw badRequest("invalid_git_commit", "gitCommit 必须是 7 到 40 位十六进制提交号");
  }
  return {
    version,
    expectedSha256,
    gitCommit,
    notes: validateText(fields.notes, "notes", 2000, { multiline: true }),
    operator: validateText(fields.operator, "operator", 80),
  };
}

function validateStoredDraft(draft) {
  const valid = draft
    && typeof draft === "object"
    && draft.platform === WINDOWS_PLATFORM
    && draft.status === "draft"
    && UUID_PATTERN.test(draft.id || "")
    && STAGING_FILE_PATTERN.test(draft.staging_file || "")
    && VERSION_PATTERN.test(draft.entry?.version || "")
    && HASH_PATTERN.test(draft.entry?.sha256 || "")
    && draft.file_name === `pinvou3_${draft.entry?.version}_x64-setup.exe`;
  if (!valid) throw new AppError(500, "invalid_draft", "保存的 Windows 发布草稿结构无效");
  return draft;
}

export async function receiveUpload(
  request,
  stagingDir,
  maxUploadBytes,
  uploadIdleTimeoutMs = 2 * 60 * 1000,
) {
  const temporaryPath = join(stagingDir, `${randomUUID()}.upload`);
  let parser;
  try {
    parser = Busboy({
      headers: request.headers,
      preservePath: true,
      defParamCharset: "utf8",
      limits: {
        fieldNameSize: 64,
        // notes allows 2,000 Unicode characters; reserve enough UTF-8 bytes.
        fieldSize: 8192,
        fields: 5,
        files: 1,
        // Busboy emits partsLimit as soon as the configured sentinel is reached,
        // so keep one slot beyond the five fields and single installer.
        parts: 7,
        fileSize: maxUploadBytes + 1,
        headerPairs: 64,
      },
    });
  } catch {
    throw badRequest("invalid_multipart", "请求必须是有效的 multipart/form-data");
  }

  const fields = Object.create(null);
  const hash = createHash("sha256");
  let filename;
  let filePromise;
  let size = 0;
  let tooLarge = false;
  let parseError;
  let aborted = false;
  let idleError;
  let idleTimer;

  const resetIdleTimer = () => {
    clearTimeout(idleTimer);
    idleTimer = setTimeout(() => {
      idleError = new AppError(408, "upload_idle_timeout", "上传长时间没有数据进展，已终止并清理临时文件");
      request.unpipe(parser);
      parser.destroy(idleError);
      // Stop accepting a request body that may otherwise remain half-open
      // behind the reverse proxy. The client retries on the closed connection.
      request.destroy();
    }, uploadIdleTimeoutMs);
  };

  const abortHandler = () => {
    if (idleError) return;
    aborted = true;
    parser.destroy(new Error("upload aborted"));
  };
  const endHandler = () => clearTimeout(idleTimer);
  request.once("aborted", abortHandler);
  request.once("end", endHandler);
  request.on("data", resetIdleTimer);
  resetIdleTimer();
  try {
    await new Promise((resolvePromise, rejectPromise) => {
      parser.on("field", (name, value, info) => {
        if (
          !["version", "gitCommit", "expectedSha256", "notes", "operator"].includes(name)
          || Object.hasOwn(fields, name)
          || info.nameTruncated
          || info.valueTruncated
        ) {
          parseError ||= badRequest("invalid_multipart", "上传字段重复、未知或超出长度限制");
          return;
        }
        fields[name] = value;
      });
      parser.on("file", (name, stream, info) => {
        if (name !== "installer" || filePromise) {
          parseError ||= badRequest("invalid_multipart", "必须且只能上传一个 installer 文件");
          stream.resume();
          return;
        }
        if (!info.filename || /[/\\\u0000-\u001f]/u.test(info.filename)) {
          parseError ||= badRequest("invalid_filename", "安装器文件名无效");
          stream.resume();
          return;
        }
        filename = info.filename.normalize("NFC");
        stream.once("limit", () => {
          tooLarge = true;
        });
        const inspect = new Transform({
          transform(chunk, _encoding, callback) {
            size += chunk.length;
            hash.update(chunk);
            callback(null, chunk);
          },
        });
        filePromise = pipeline(
          stream,
          inspect,
          createWriteStream(temporaryPath, { flags: "wx", mode: 0o600 }),
        );
        filePromise.catch(() => {});
      });
      for (const event of ["fieldsLimit", "filesLimit", "partsLimit"]) {
        parser.once(event, () => {
          parseError ||= badRequest("invalid_multipart", "上传字段或文件数量超出限制");
        });
      }
      parser.once("error", rejectPromise);
      parser.once("close", resolvePromise);
      request.pipe(parser);
    });
    if (filePromise) await filePromise;
    if (aborted) throw badRequest("upload_aborted", "上传已中断");
    if (parseError) throw parseError;
    if (!filePromise || !filename) throw badRequest("missing_installer", "缺少 installer 文件");
    if (tooLarge || size > maxUploadBytes) {
      throw new AppError(413, "upload_too_large", "安装器超过服务端允许的大小");
    }
    return { fields: safeUploadFields(fields), filename, temporaryPath, size, sha256: hash.digest("hex") };
  } catch (error) {
    if (filePromise) await filePromise.catch(() => {});
    await rm(temporaryPath, { force: true });
    if (idleError) throw idleError;
    if (aborted || request.aborted) {
      throw badRequest("upload_aborted", "上传已中断");
    }
    throw error;
  } finally {
    clearTimeout(idleTimer);
    request.off("aborted", abortHandler);
    request.off("end", endHandler);
    request.off("data", resetIdleTimer);
  }
}

export class ReleaseService {
  constructor(config, { now = () => new Date(), onlineVerifier = verifyOnlineAsset } = {}) {
    this.config = config;
    this.now = now;
    this.onlineVerifier = onlineVerifier;
    this.manifestPath = join(config.publicRoot, "latest.json");
    this.stagingDir = join(config.stateRoot, "staging");
    this.historyDir = join(config.stateRoot, "history");
    this.draftPath = join(config.stateRoot, "windows-draft.json");
    this.auditPath = join(config.stateRoot, "audit.ndjson");
    this.lockPath = join(config.stateRoot, "publish.lock");
    this.ready = false;
  }

  async initialize() {
    this.ready = false;
    // publicRoot 必须由部署挂载预先提供；自行创建会把缺失/错误挂载伪装成健康服务。
    await requireDirectory(this.config.publicRoot, "publicRoot");
    await mkdir(this.config.stateRoot, { recursive: true, mode: 0o700 });
    await mkdir(this.stagingDir, { recursive: true, mode: 0o700 });
    await mkdir(this.historyDir, { recursive: true, mode: 0o700 });
    // The supported deployment is a single process. A lock can only survive
    // when that process was interrupted, so clear it during process startup.
    await rm(this.lockPath, { force: true });
    await removeOwnedTemporaryFiles(
      this.config.publicRoot,
      [PUBLIC_TMP_PATTERN, MANIFEST_TMP_PATTERN],
    );
    await removeOwnedTemporaryFiles(this.config.stateRoot, [DRAFT_TMP_PATTERN]);
    await removeOwnedTemporaryFiles(this.historyDir, [HISTORY_TMP_PATTERN]);
    const draft = await this.getDraft();
    const retainedStagingFile = draft ? validateStoredDraft(draft).staging_file : null;
    for (const name of await readdir(this.stagingDir)) {
      if (name !== retainedStagingFile) await rm(join(this.stagingDir, name), { force: true });
    }
    await this.verifyStartupReadiness();
    this.ready = true;
  }

  async verifyStartupReadiness() {
    const directories = [
      [this.config.publicRoot, "publicRoot"],
      [this.config.stateRoot, "stateRoot"],
      [this.stagingDir, "stagingDir"],
      [this.historyDir, "historyDir"],
    ];
    for (const [path, label] of directories) await requireDirectory(path, label);
    await requirePublicFile(this.manifestPath, "latest.json");
    await this.readManifest();
    for (const [path, label] of directories) await verifyWritableDirectory(path, label);
  }

  getHealthStatus() {
    return { status: this.ready ? "ok" : "not_ready" };
  }

  async readManifest() {
    const manifest = await readJson(this.manifestPath);
    if (!manifest || typeof manifest !== "object" || Array.isArray(manifest)) {
      throw new AppError(500, "invalid_manifest", "latest.json 必须是 JSON 对象");
    }
    return manifest;
  }

  async getDraft() {
    const draft = await readJson(this.draftPath, { missing: null });
    return draft ? validateStoredDraft(draft) : null;
  }

  async getHistory() {
    const names = (await readdir(this.historyDir)).filter((name) => name.endsWith(".json")).sort().reverse();
    const records = [];
    for (const name of names.slice(0, 100)) records.push(await readJson(join(this.historyDir, name)));
    return records;
  }

  async getStatus() {
    const [manifest, draft, history] = await Promise.all([
      this.readManifest(),
      this.getDraft(),
      this.getHistory(),
    ]);
    return {
      windows: manifest.platforms?.[WINDOWS_PLATFORM] ?? null,
      draft,
      history,
    };
  }

  async audit(action, details = {}) {
    const record = { timestamp: this.now().toISOString(), action, ...details };
    await appendFile(this.auditPath, `${JSON.stringify(record)}\n`, { encoding: "utf8", mode: 0o600 });
  }

  async createDraft(request, remoteAddress) {
    // Reject the common case before receiving a potentially multi-gigabyte
    // body, then repeat the check under the lock after validation to close the
    // race between concurrent uploaders.
    if (await this.getDraft()) throw conflict("draft_exists", "已有待处理草稿，请先发布或取消");
    const upload = await receiveUpload(
      request,
      this.stagingDir,
      this.config.maxUploadBytes,
      this.config.uploadIdleTimeoutMs,
    );
    let draftSaved = false;
    try {
      const expectedFilename = `pinvou3_${upload.fields.version}_x64-setup.exe`;
      if (upload.filename !== expectedFilename) {
        throw badRequest("invalid_filename", `安装器必须命名为 ${expectedFilename}`);
      }
      if (upload.sha256 !== upload.fields.expectedSha256) {
        throw badRequest("sha256_mismatch", `SHA-256 不一致，实际值为 ${upload.sha256}`);
      }
      await inspectPeInstaller(upload.temporaryPath, upload.fields.version);
      await syncFile(upload.temporaryPath);

      return await this.withPublishLock(async () => {
        if (await this.getDraft()) throw conflict("draft_exists", "已有待处理草稿，请先发布或取消");
        const manifest = await this.readManifest();
        const currentWindows = manifest.platforms?.[WINDOWS_PLATFORM] ?? null;
        if (currentWindows?.version && compareVersions(upload.fields.version, currentWindows.version) <= 0) {
          throw conflict("version_not_newer", `新版本必须高于当前 Windows 版本 ${currentWindows.version}`);
        }

        const preparedAt = this.now().toISOString();
        const entry = {
          version: upload.fields.version,
          url: `${this.config.publicBaseUrl}/${expectedFilename}`,
          format: "nsis",
          sha256: upload.sha256,
          size: upload.size,
          restart_after_install: false,
          notes: upload.fields.notes,
          pub_date: preparedAt,
        };
        const draft = {
          id: randomUUID(),
          platform: WINDOWS_PLATFORM,
          status: "draft",
          prepared_at: preparedAt,
          operator: upload.fields.operator,
          git_commit: upload.fields.gitCommit,
          file_name: expectedFilename,
          staging_file: basename(upload.temporaryPath),
          entry,
          base_windows: jsonClone(currentWindows),
          preview_manifest: mergeWindowsEntry(manifest, entry),
        };
        await atomicWriteJson(this.draftPath, draft);
        draftSaved = true;
        await this.audit("draft_created", {
          remote_address: remoteAddress,
          draft_id: draft.id,
          operator: draft.operator,
          version: entry.version,
          sha256: entry.sha256,
          size: entry.size,
          git_commit: draft.git_commit,
        });
        return draft;
      });
    } catch (error) {
      if (draftSaved) await rm(this.draftPath, { force: true });
      await rm(upload.temporaryPath, { force: true });
      throw error;
    }
  }

  async cancelDraft(draftId, operator, remoteAddress) {
    return this.withPublishLock(() => this.cancelDraftUnlocked(draftId, operator, remoteAddress));
  }

  async cancelDraftUnlocked(draftId, operator, remoteAddress) {
    const draft = await this.requireDraft(draftId);
    const actor = validateText(operator, "operator", 80);
    await rm(join(this.stagingDir, draft.staging_file), { force: true });
    await rm(this.draftPath, { force: true });
    await this.audit("draft_cancelled", {
      remote_address: remoteAddress,
      draft_id: draft.id,
      operator: actor,
      version: draft.entry.version,
    });
    return { cancelled: true };
  }

  async requireDraft(draftId) {
    const draft = await this.getDraft();
    if (!draft) throw new AppError(404, "draft_not_found", "没有待处理草稿");
    if (typeof draftId !== "string" || draft.id !== draftId) {
      throw conflict("draft_changed", "草稿已发生变化，请刷新页面后重试");
    }
    return draft;
  }

  async withPublishLock(callback) {
    let handle;
    try {
      handle = await open(this.lockPath, "wx", 0o600);
    } catch (error) {
      if (error.code === "EEXIST") throw conflict("publish_locked", "另一项发布操作正在执行");
      throw error;
    }
    try {
      return await callback();
    } finally {
      try {
        await handle.close();
      } finally {
        await rm(this.lockPath, { force: true });
      }
    }
  }

  async publish(draftId, operator, remoteAddress) {
    return this.withPublishLock(async () => {
      const draft = await this.requireDraft(draftId);
      const actor = validateText(operator, "operator", 80);
      const manifest = await this.readManifest();
      const currentWindows = manifest.platforms?.[WINDOWS_PLATFORM] ?? null;
      const existingRecord = (await this.getHistory()).find(
        (record) => record.type === "publish" && record.id === draft.id,
      );
      if (sameReleaseAsset(currentWindows, draft.entry)) {
        const recoveredRecord = existingRecord || {
          id: draft.id,
          type: "publish",
          published_at: currentWindows.pub_date || this.now().toISOString(),
          operator: actor,
          git_commit: draft.git_commit,
          file_name: draft.file_name,
          entry: jsonClone(currentWindows),
          previous_windows: jsonClone(draft.base_windows),
        };
        if (!existingRecord) {
          await atomicWriteJson(
            join(this.historyDir, `${Date.now()}-${draft.id}.json`),
            recoveredRecord,
          );
        }
        await rm(this.draftPath, { force: true });
        await this.audit("release_publish_recovered", {
          remote_address: remoteAddress,
          release_id: recoveredRecord.id,
          operator: actor,
          version: recoveredRecord.entry.version,
        });
        return recoveredRecord;
      }
      if (!sameJson(currentWindows, draft.base_windows)) {
        throw conflict("windows_release_changed", "线上 Windows 节点已变化，请取消草稿并重新上传复核");
      }

      const stagingPath = join(this.stagingDir, draft.staging_file);
      const destinationPath = join(this.config.publicRoot, draft.file_name);
      const expectedAsset = { sha256: draft.entry.sha256, size: draft.entry.size };
      if (await pathExists(stagingPath)) {
        const actual = await hashFile(stagingPath);
        if (!sameHashAndSize(actual, expectedAsset)) {
          throw conflict("staged_file_changed", "暂存安装器的大小或 SHA-256 已变化");
        }
        await copyStagedAsset(stagingPath, destinationPath, expectedAsset);
      } else {
        if (!(await pathExists(destinationPath))) {
          throw conflict("staged_file_missing", "暂存安装器不存在，请取消草稿并重新上传");
        }
        const existing = await hashFile(destinationPath);
        if (!sameHashAndSize(existing, expectedAsset)) {
          throw conflict("published_asset_changed", "线上安装器与草稿不一致");
        }
      }

      const onlineAsset = await hashFile(destinationPath);
      if (!sameHashAndSize(onlineAsset, expectedAsset)) {
        throw conflict("online_verification_failed", "发布目录中的安装器回读校验失败");
      }
      await this.onlineVerifier(
        draft.entry.url,
        { sha256: draft.entry.sha256, size: draft.entry.size },
        this.config.onlineVerifyTimeoutMs,
      );

      const publishedAt = this.now().toISOString();
      const record = existingRecord || {
        id: draft.id,
        type: "publish",
        published_at: publishedAt,
        operator: actor,
        git_commit: draft.git_commit,
        file_name: draft.file_name,
        entry: { ...draft.entry, pub_date: publishedAt },
        previous_windows: jsonClone(currentWindows),
      };
      await atomicWriteJson(this.manifestPath, mergeWindowsEntry(manifest, record.entry), 0o644);
      if (!existingRecord) {
        await atomicWriteJson(join(this.historyDir, `${Date.now()}-${draft.id}.json`), record);
      }
      await rm(this.draftPath, { force: true });
      await this.audit("release_published", {
        remote_address: remoteAddress,
        release_id: record.id,
        operator: actor,
        version: record.entry.version,
        sha256: record.entry.sha256,
        size: record.entry.size,
        git_commit: record.git_commit,
      });
      return record;
    });
  }

  async verifyRollbackTarget(entry) {
    const valid = entry
      && typeof entry === "object"
      && VERSION_PATTERN.test(entry.version || "")
      && entry.format === "nsis"
      && HASH_PATTERN.test(entry.sha256 || "")
      && Number.isSafeInteger(entry.size)
      && entry.size > 0;
    if (!valid) throw new AppError(500, "invalid_rollback_asset", "回滚目标 Windows 资产结构无效");
    const fileName = `pinvou3_${entry.version}_x64-setup.exe`;
    if (entry.url !== `${this.config.publicBaseUrl}/${fileName}`) {
      throw new AppError(500, "invalid_rollback_asset", "回滚目标 URL 不属于当前发布目录");
    }
    const expected = { sha256: entry.sha256, size: entry.size };
    const localPath = join(this.config.publicRoot, fileName);
    if (!(await pathExists(localPath))) {
      throw conflict("rollback_asset_missing", "回滚目标安装器在发布目录中不存在");
    }
    const local = await hashFile(localPath);
    if (!sameHashAndSize(local, expected)) {
      throw conflict("rollback_asset_changed", "回滚目标安装器的大小或 SHA-256 不一致");
    }
    await this.onlineVerifier(entry.url, expected, this.config.onlineVerifyTimeoutMs);
  }

  async rollback(releaseId, operator, reason, remoteAddress) {
    return this.withPublishLock(async () => {
      const actor = validateText(operator, "operator", 80);
      const rollbackReason = validateText(reason, "reason", 500, { multiline: true });
      const history = await this.getHistory();
      const release = history.find((record) => record.type === "publish" && record.id === releaseId);
      if (!release) throw new AppError(404, "release_not_found", "未找到可回滚的发布记录");

      const manifest = await this.readManifest();
      const currentWindows = manifest.platforms?.[WINDOWS_PLATFORM] ?? null;
      const existingRollback = history.find(
        (record) => record.type === "rollback" && record.release_id === release.id,
      );
      const targetIsCurrent = sameJson(currentWindows, release.previous_windows);
      const releaseIsCurrent = sameJson(currentWindows, release.entry);
      if (!targetIsCurrent && !releaseIsCurrent) {
        throw conflict("release_not_current", "只能回滚当前正在生效的 Windows 版本");
      }
      if (release.previous_windows) await this.verifyRollbackTarget(release.previous_windows);
      if (targetIsCurrent) {
        if (existingRollback) return existingRollback;
        const recoveredRollback = {
          id: randomUUID(),
          type: "rollback",
          rolled_back_at: this.now().toISOString(),
          operator: actor,
          reason: rollbackReason,
          release_id: release.id,
          from_windows: release.entry,
          to_windows: release.previous_windows,
        };
        await atomicWriteJson(
          join(this.historyDir, `${Date.now()}-${recoveredRollback.id}.json`),
          recoveredRollback,
        );
        await this.audit("release_rollback_recovered", {
          remote_address: remoteAddress,
          rollback_id: recoveredRollback.id,
          release_id: release.id,
          operator: actor,
        });
        return recoveredRollback;
      }
      const rolledBack = jsonClone(manifest);
      if (!rolledBack.platforms || typeof rolledBack.platforms !== "object") rolledBack.platforms = {};
      if (release.previous_windows) rolledBack.platforms[WINDOWS_PLATFORM] = release.previous_windows;
      else delete rolledBack.platforms[WINDOWS_PLATFORM];

      const rolledBackAt = this.now().toISOString();
      const rollbackRecord = {
        id: randomUUID(),
        type: "rollback",
        rolled_back_at: rolledBackAt,
        operator: actor,
        reason: rollbackReason,
        release_id: release.id,
        from_windows: release.entry,
        to_windows: release.previous_windows,
      };
      await atomicWriteJson(this.manifestPath, rolledBack, 0o644);
      await atomicWriteJson(join(this.historyDir, `${Date.now()}-${rollbackRecord.id}.json`), rollbackRecord);
      await this.audit("release_rolled_back", {
        remote_address: remoteAddress,
        rollback_id: rollbackRecord.id,
        release_id: release.id,
        operator: actor,
        from_version: release.entry.version,
        to_version: release.previous_windows?.version ?? null,
        reason: rollbackReason,
      });
      return rollbackRecord;
    });
  }
}
