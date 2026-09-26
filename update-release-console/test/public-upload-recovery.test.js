import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

class TestFormData {
  constructor(form) {
    this.values = form.values;
  }

  get(name) {
    return this.values[name];
  }
}

function formValues(overrides = {}) {
  return {
    version: "0.10.4",
    gitCommit: "ABCDEF1234567890",
    expectedSha256: "A".repeat(64),
    notes: "第一行\n第二行",
    operator: "测试发布员",
    ...overrides,
  };
}

function matchingDraft(overrides = {}) {
  return {
    file_name: "pinvou3_0.10.4_x64-setup.exe",
    git_commit: "abcdef1234567890",
    operator: "测试发布员",
    entry: {
      version: "0.10.4",
      sha256: "a".repeat(64),
      size: 4096,
      notes: "第一行\r\n第二行",
    },
    ...overrides,
  };
}

async function recoveryFunctions(overrides = {}) {
  const page = await readFile(join(root, "public/index.html"), "utf8");
  const start = page.indexOf("function retryableUploadError");
  const end = page.indexOf("function render()", start);
  assert.ok(start >= 0 && end > start, "upload recovery functions must remain extractable");
  const context = {
    FormData: TestFormData,
    XMLHttpRequest: class {},
    token: "test-token",
    state: { draft: null },
    $: () => ({ files: [] }),
    showBusy: () => {},
    refresh: async () => {},
    wait: async () => {},
    ...overrides,
  };
  vm.runInNewContext(
    `${page.slice(start, end)}\nthis.recovery = { uploadOnce, draftMatchesUpload, uploadWithRecovery };`,
    context,
  );
  return { context, ...context.recovery };
}

test("XHR 错误保留服务端错误码供恢复流程判断", async () => {
  class ConflictRequest {
    constructor() {
      this.status = 409;
      this.response = { error: "draft_exists", message: "已有待处理草稿" };
      this.listeners = {};
      this.upload = { addEventListener() {} };
    }

    open() {}

    setRequestHeader() {}

    addEventListener(name, listener) {
      this.listeners[name] = listener;
    }

    send() {
      queueMicrotask(() => this.listeners.load());
    }
  }
  const { uploadOnce } = await recoveryFunctions({ XMLHttpRequest: ConflictRequest });
  await assert.rejects(
    uploadOnce({ values: formValues() }, 1),
    (error) => error.code === "draft_exists" && error.status === 409,
  );
});

test("草稿恢复匹配包含安装器和全部发布元数据", async () => {
  const { draftMatchesUpload } = await recoveryFunctions();
  const form = { values: formValues() };
  const file = { name: "pinvou3_0.10.4_x64-setup.exe", size: 4096 };
  assert.equal(draftMatchesUpload(matchingDraft(), form, file), true);
  assert.equal(draftMatchesUpload(matchingDraft({ git_commit: "1111111" }), form, file), false);
  assert.equal(draftMatchesUpload(matchingDraft({ operator: "其他发布员" }), form, file), false);
  assert.equal(draftMatchesUpload(matchingDraft({
    entry: { ...matchingDraft().entry, notes: "其他说明" },
  }), form, file), false);
});

test("首次查询为空但重试遇到 draft_exists 时再次确认并恢复成功", async () => {
  const file = { name: "pinvou3_0.10.4_x64-setup.exe", size: 4096 };
  const form = { values: formValues() };
  let uploadCalls = 0;
  let refreshCalls = 0;
  let context;
  const functions = await recoveryFunctions({
    $: () => ({ files: [file] }),
    uploadOnce: async () => {},
    refresh: async () => {},
  });
  context = functions.context;
  context.uploadOnce = async () => {
    uploadCalls += 1;
    if (uploadCalls === 1) {
      const error = new Error("connection lost");
      error.retryable = true;
      throw error;
    }
    context.state.draft = matchingDraft();
    const error = new Error("已有待处理草稿");
    error.code = "draft_exists";
    throw error;
  };
  context.refresh = async () => {
    refreshCalls += 1;
    if (refreshCalls === 1) context.state.draft = null;
  };

  const recovered = await functions.uploadWithRecovery(form);
  assert.equal(recovered, context.state.draft);
  assert.equal(uploadCalls, 2);
  assert.equal(refreshCalls, 2);
});
