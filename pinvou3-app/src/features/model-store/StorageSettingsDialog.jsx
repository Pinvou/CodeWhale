// 「存储与导入」设置框：权重放在哪，以及下载前去哪找已经在本机的副本。
//
// 为什么这两件事值得一个独立的框：权重目录一旦跟用户实际存放的位置对不上，
// 下载会白跑几十个小时，而**界面上看不出任何异常**——进度条照常涨，日志照常
// 刷。2026-09-20 真机上就是这样，用户已经把 124GB 导到了 `~/models`，而应用
// 认的是另一个目录，他只能看着它从头下。
//
// 框里最要紧的其实是第三块「下载前实际查找的位置」——那是只读的完整列表。
// 用户真正会问的是"到底找了哪些地方"，只给一个可编辑的补充输入框答不了。

import { useEffect, useState } from 'react';

import { fetchStorageSettings, saveStorageSettings } from './model-store-api.js';
import { formatBytes } from './model-store-logic.js';
import {
  INPUT_CLASS,
  MsButton,
  MsDialog,
  MsDialogTitle,
  SUBTLE_SURFACE,
  TEXT_SECONDARY,
} from './model-store-ui.jsx';

/** 可移动介质的挂载点前缀，跟 Rust 侧 `import_scan::REMOVABLE_PREFIXES` 对齐。 */
const REMOVABLE_PREFIXES = ['/media/', '/mnt/', '/run/media/'];

function isRemovable(path) {
  return REMOVABLE_PREFIXES.some((prefix) => path.startsWith(prefix));
}

/**
 * 存储与导入设置框。
 * @param {object} props - 属性。
 * @param {boolean} props.open - 是否打开。
 * @param {() => void} props.onClose - 关闭回调。
 * @param {object} props.copy - 文案（`t.uiModelStore`）。
 * @param {string} props.theme - 主题，透传给 MsDialog。
 * @param {(message: string) => void} [props.onSaved] - 保存成功后的提示回调。
 * @returns {JSX.Element} 对话框。
 */
function StorageSettingsDialog({ open, onClose, copy, theme, onSaved }) {
  const [settings, setSettings] = useState(null);
  const [modelsDir, setModelsDir] = useState('');
  const [importText, setImportText] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // 这一次打开有没有成功读到设置。**保存的前提**，见下面 `canSave`。
  //
  // 不能用 `settings !== null` 代替：这个组件不随弹窗卸载（MsDialog 只是在
  // `open` 为假时返回 null，组件自己的 state 全都留着）。于是第二次打开时
  // `settings` 还挂着上一次的值，读取失败也依旧非空——判据会失效。
  const [loaded, setLoaded] = useState(false);

  // 每次打开都重读一次，而不是只在挂载时读一次：权重目录也可能被别处改掉
  // （手改 config.json 是排查时的常规动作），拿着一份旧值保存会把它覆盖回去。
  useEffect(() => {
    if (!open) return undefined;
    let alive = true;
    setError('');
    setLoaded(false);
    fetchStorageSettings()
      .then((data) => {
        if (!alive) return;
        setSettings(data);
        setModelsDir(data.models_dir || '');
        setImportText((data.import_scan_dirs || []).join('\n'));
        setLoaded(true);
      })
      .catch((e) => {
        if (alive) setError(String(e));
      });
    return () => {
      alive = false;
    };
  }, [open]);

  async function submit() {
    setBusy(true);
    setError('');
    try {
      const next = await saveStorageSettings({
        modelsDir,
        importScanDirs: importText
          .split('\n')
          .map((line) => line.trim())
          .filter(Boolean),
      });
      setSettings(next);
      onSaved?.(copy.storageSaved);
      onClose();
    } catch (e) {
      // 保存失败不关框：路径填错是最常见的失败，关掉就得让用户从头再填一遍。
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  const scanDirs = settings?.scan_dirs || [];
  const freeBytes = settings?.free_bytes;

  // 读取没成功过就不让保存。
  //
  // 读失败时两个输入框都还是空串，而**空的 `importText` 会被解析成 `[]`**——
  // 用户看见报错、随手把权重目录填回去点保存，已经存着的那几条「额外的查找
  // 目录」就被这个 `[]` 静默清空了，没有任何确认，也看不出发生过。
  // 查找目录正是「几十 GB 本地副本还找不找得到」的那份配置，丢了要人命。
  //
  // 判据必须把 `importText` 的加载态一起算进去：它跟 `modelsDir` 是在同一次
  // 请求里填的，只看 `modelsDir` 非空等于默许了"空 importText 也能提交"。
  // `loaded` 同时覆盖这两个框——请求还在飞、或者飞失败了，都不给存。
  //
  // 保存失败不影响 `loaded`：那时表单里是用户自己填的内容，本来就该让他改完
  // 再存一次（路径填错是最常见的失败）。
  const canSave = loaded && !busy && !!modelsDir.trim();

  return (
    <MsDialog
      open={open}
      onClose={onClose}
      theme={theme}
      wide
      labelledBy="model-store-storage-title"
      testId="model-store-storage-dialog"
    >
      <MsDialogTitle id="model-store-storage-title" onClose={onClose} closeLabel={copy.storageCancel}>
        {copy.storageTitle}
      </MsDialogTitle>

      <label className="mb-1 block text-[13px] font-medium" htmlFor="model-store-models-dir">
        {copy.storageModelsDirLabel}
      </label>
      <input
        id="model-store-models-dir"
        className={INPUT_CLASS}
        data-testid="model-store-models-dir"
        value={modelsDir}
        spellCheck={false}
        onChange={(event) => setModelsDir(event.target.value)}
      />
      <p className={`mb-4 mt-1 text-[12px] leading-5 ${TEXT_SECONDARY}`}>
        {copy.storageModelsDirHint}
        {typeof freeBytes === 'number' ? ` · ${copy.storageFree(formatBytes(freeBytes))}` : ''}
      </p>

      <label className="mb-1 block text-[13px] font-medium" htmlFor="model-store-import-dirs">
        {copy.storageImportLabel}
      </label>
      <textarea
        id="model-store-import-dirs"
        rows={3}
        className={`${INPUT_CLASS} resize-y font-mono text-[12px]`}
        data-testid="model-store-import-dirs"
        value={importText}
        spellCheck={false}
        onChange={(event) => setImportText(event.target.value)}
      />
      <p className={`mb-4 mt-1 text-[12px] leading-5 ${TEXT_SECONDARY}`}>{copy.storageImportHint}</p>

      {/* 只读的完整查找范围。这一块是整个框存在的理由——见文件头。
          刻意放在输入框下面：它要反映的是「保存之后会怎样」，而保存前它显示的
          还是上一次的结果，放在上面会让人以为编辑立刻生效了。 */}
      <div className="mb-1 text-[13px] font-medium">{copy.storageScanLabel}</div>
      <ol
        data-testid="model-store-scan-dirs"
        className={`max-h-44 overflow-auto rounded-[12px] px-3 py-2 font-mono text-[12px] leading-6 ${SUBTLE_SURFACE}`}
      >
        {scanDirs.map((dir) => (
          <li key={dir} className="truncate" title={dir}>
            {dir}
            {isRemovable(dir) ? (
              <span className={`ml-2 font-sans text-[11px] ${TEXT_SECONDARY}`}>
                {copy.storageRemovable}
              </span>
            ) : null}
          </li>
        ))}
      </ol>
      <p className={`mt-1 text-[12px] leading-5 ${TEXT_SECONDARY}`}>{copy.storageScanHint}</p>

      {error ? (
        <p className="mt-3 text-[12px] leading-5 text-red-600 dark:text-red-400" data-testid="model-store-storage-error">
          {error}
          {/* 读取失败时补一句「保存已禁用」。只把按钮灰掉不解释的话，用户面对
              一个点不动的保存键只能猜——而这里真正要传达的是"现在框里显示的
              不是你的设置"，不说清楚他会以为设置本身被清空了。 */}
          {loaded ? null : (
            <span className="mt-1 block" data-testid="model-store-storage-load-failed">
              {copy.storageLoadFailedHint}
            </span>
          )}
        </p>
      ) : null}

      <div className="mt-5 flex justify-end gap-2">
        <MsButton tone="ghost" onClick={onClose}>
          {copy.storageCancel}
        </MsButton>
        <MsButton
          busy={busy}
          disabled={!canSave}
          onClick={submit}
          data-testid="model-store-storage-save"
        >
          {copy.storageSave}
        </MsButton>
      </div>
    </MsDialog>
  );
}

export { StorageSettingsDialog };
