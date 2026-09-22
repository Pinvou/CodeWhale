// Windows A 远程会话能力界面（DEV-23）。
//
// 只消费 DEV-20 的 bridge contract：模型列表是 opaque 描述、任务提交只传
// 选中的 modelId + 任务绑定，组件自身不读取 prefs / credential_ref，也不
// 构造模型包。远程上下文始终有显著标识；离线历史明确标注为副本；审批与
// 成果/附件状态都使用可恢复的显式文案。

import { useEffect, useMemo, useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';
import { isTaskTerminal, taskStatusKey } from './remote-control-logic.mjs';

export default function RemoteConversationView({ t, bs, modelSource = 'controlled_device', onModelSourceChange }) {
  const copy = (t && t.remoteConversation) || {};
  const connection = (bs && bs.controllerConnection) || {};
  const capabilities = (bs && bs.remoteCapabilities) || null;
  const models = (bs && bs.remoteModels) || [];
  const tasks = (bs && bs.tasks) || [];
  const transcript = (bs && bs.transcript) || [];
  const permissions = (bs && bs.remotePermissions) || [];
  const histories = (bs && bs.remoteHistories) || [];
  const [selectedModelId, setSelectedModelId] = useState(null);
  const [mode, setMode] = useState('chat');
  const [draft, setDraft] = useState('');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [artifacts, setArtifacts] = useState([]);

  const exportableModels = useMemo(
    () => models.filter((model) => model.exportable),
    [models],
  );
  const modes = (capabilities && Array.isArray(capabilities.modes)) ? capabilities.modes : ['chat'];
  const activeTask = tasks.find((task) => !isTaskTerminal(task.status)) || null;

  useEffect(() => {
    if (!bridge || !bridge.available || !bridge.desktopRemoteControl) return;
    const sessionId = connection.sessionId;
    if (!sessionId) return;
    bridge.desktopRemoteControl.artifactIndex(sessionId)
      .then((entries) => setArtifacts(Array.isArray(entries) ? entries : []))
      .catch(() => setArtifacts([]));
  }, [connection.sessionId]);

  const remote = bridge && bridge.available ? bridge.desktopRemoteControl : null;

  // E2E-07：连接建立后先开远程会话意图，再索取能力快照（拿到 revision 才允许
  // 构造任务）；两者都失败关闭，不暴露半通路。
  const sessionRef = connection.sessionRef || null;
  const connected = connection.status === 'connected';
  const capabilityReady = !!sessionRef && connection.capabilityRevision != null;
  const [sessionBusy, setSessionBusy] = useState(false);
  const [sessionError, setSessionError] = useState(null);

  const openSession = () => {
    if (!remote || !connected || sessionRef || sessionBusy) return;
    setSessionBusy(true);
    setSessionError(null);
    remote.controllerSessionOpen(copy.title || 'remote', 'chat')
      .then(() => setSessionError(null))
      .catch((failure) => setSessionError(failure && failure.code ? String(failure.code) : 'errorGeneric'))
      .finally(() => setSessionBusy(false));
  };

  useEffect(() => {
    if (!remote || !connected || sessionRef) return;
    openSession();
  }, [remote, connected, sessionRef]);
  useEffect(() => {
    if (!remote || !sessionRef || connection.capabilityRevision != null) return;
    remote.controllerCapabilitySend(sessionRef).catch(function () {});
  }, [remote, sessionRef, connection.capabilityRevision]);

  const send = async () => {
    if (!remote || !draft.trim() || !capabilityReady) return;
    setBusy(true);
    setError(null);
    try {
      // E2E-04：只传会话引用与正文；协议字段与输入摘要全部由 Rust 构造并加密。
      await remote.controllerTaskSend(sessionRef, draft.trim(), modelSource);
      setDraft('');
    } catch (failure) {
      setError((failure && failure.code) || 'errorGeneric');
    } finally {
      setBusy(false);
    }
  };

  const decide = async (permission, decision) => {
    if (!remote) return;
    try {
      await remote.permissionDecide(permission.taskId, permission.permissionRequestId, decision);
    } catch (failure) {
      setError((failure && failure.code) || 'errorGeneric');
    }
  };

  return (
    <div className="remote-conversation" role="region" aria-label={copy.title}>
      <div className="mb-2 flex items-center gap-2 text-[11px]" role="status" aria-live="polite">
        <span className="rounded-full bg-[#FCE8E6] px-2 py-0.5 text-[#C5221F] dark:bg-[#3A2123]">
          {copy.remoteBadge}
        </span>
        <span>{connection.deviceName || copy.remoteBadge}</span>
      </div>

      {capabilities && (
        <section aria-labelledby="remote-capabilities-title" className="mb-3">
          <h3 id="remote-capabilities-title" className="text-[12px] font-medium">{copy.capabilitiesTitle}</h3>
          {((capabilities.tools || []).length + (capabilities.workspaces || []).length) === 0 ? (
            <p className="text-[11px] opacity-70">{copy.capabilitiesEmpty}</p>
          ) : (
            <ul className="text-[11px] opacity-80">
              {(capabilities.tools || []).map((tool) => <li key={tool.toolId}>{tool.displayName}</li>)}
              {(capabilities.workspaces || []).map((workspace) => (
                <li key={workspace.workspaceId}>{copy.workspaceLabel}: {workspace.displayName}</li>
              ))}
            </ul>
          )}
          <div className="mt-1 flex gap-2 text-[11px]">
            {modes.map((value) => (
              <label key={value}>
                <input
                  type="radio"
                  name="remote-mode"
                  value={value}
                  checked={mode === value}
                  onChange={() => setMode(value)}
                />
                {value === 'code' ? copy.modeCode : copy.modeChat}
              </label>
            ))}
            {!modes.includes('code') && <span className="opacity-60">{copy.modeHidden}</span>}
          </div>
        </section>
      )}

      {/* E2E-07 首版能力开关：A 临时模型（含模型导出列表）保持隐藏，模型来源固定为
          B 本机模型；能力开放后再恢复选择器。 */}
      <fieldset className="mb-3 text-[11px]">
        <legend className="mb-1 opacity-70">{copy.modelPickerTitle}</legend>
        <label className="mr-3 inline-flex items-center gap-1"><input type="radio" name="remote-model-source" checked={modelSource === 'controlled_device'} onChange={() => onModelSourceChange?.('controlled_device')} />{copy.modelSourceDevice}</label>
        <label className="inline-flex items-center gap-1"><input type="radio" name="remote-model-source" checked={modelSource === 'controller_temporary'} onChange={() => onModelSourceChange?.('controller_temporary')} />{copy.modelSourceMine}</label>
      </fieldset>
      {transcript.length === 0 && tasks.length === 0 && (
        <p className="text-[12px] opacity-70">{copy.empty}</p>
      )}

      <ol className="remote-transcript" aria-live="polite">
        {transcript.map((entry) => (
          <li
            key={`${entry.sessionId}:${entry.epoch}:${entry.sequence}`}
            className="mb-1 text-[12px]"
          >
            <span className="mr-2 rounded-full bg-black/5 px-2 py-0.5 text-[10px] dark:bg-white/10">
              {entry.role === 'user' ? copy.localBadge : copy.remoteBadge}
            </span>
            <span className="whitespace-pre-wrap break-words">{entry.text}</span>
          </li>
        ))}
      </ol>

      <ol className="remote-tasks" aria-live="polite">
        {tasks.map((task) => (
          <li key={task.taskId} className="mb-1 text-[12px]">
            <span className="mr-2">{copy[taskStatusKey(task.status)]}</span>
            {task.resultUnknown && <span className="opacity-70">{copy.resultUnknownHint}</span>}
          </li>
        ))}
      </ol>

      <form
        className="sticky bottom-0 mt-3 flex items-center gap-2 border-t border-black/10 bg-white pt-3 dark:border-white/10 dark:bg-[#1F2023]"
        onSubmit={(event) => {
          event.preventDefault();
          send();
        }}
      >
        <label className="sr-only" htmlFor="remote-conversation-composer">{copy.composerPlaceholder}</label>
        <input
          id="remote-conversation-composer"
          data-testid="remote-conversation-composer"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          placeholder={copy.composerPlaceholder}
          className="min-w-0 flex-1 rounded-full border border-black/10 px-4 py-2 text-[13px] outline-none dark:border-white/15 dark:bg-transparent"
        />
        <button
          type="submit"
          data-testid="remote-conversation-send"
          disabled={busy || sessionBusy || !draft.trim() || !capabilityReady}
          className="shrink-0 rounded-full bg-[#1A73E8] px-4 py-2 text-[13px] text-white disabled:opacity-60"
        >
          {busy ? copy.sending : copy.send}
        </button>
      </form>
      {!sessionRef && !sessionBusy && !sessionError && connected && (
        <p className="mt-2 text-[11px] opacity-70">{copy.sessionInitializing}</p>
      )}
      {!sessionRef && sessionBusy && (
        <p className="mt-2 text-[11px] opacity-70" role="status">{copy.sessionInitializing}</p>
      )}
      {!sessionRef && sessionError && (
        <p className="mt-2 flex items-center gap-2 text-[11px] text-[#EA4335]" role="alert">
          <span>{copy.sessionInitializeFailed} ({sessionError})</span>
          <button
            type="button"
            onClick={openSession}
            className="rounded-full border border-[#EA4335]/40 px-2 py-0.5"
          >
            {copy.retry}
          </button>
        </p>
      )}

      {permissions.length > 0 && (
        <section aria-labelledby="remote-permission-title" className="mt-3">
          <h3 id="remote-permission-title" className="text-[12px] font-medium">{copy.permissionTitle}</h3>
          <ul className="text-[12px]">
            {permissions.map((permission) => (
              <li key={permission.permissionRequestId}>
                <span>{permission.operationType} · {permission.riskLevel}</span>
                <span className="mx-2 opacity-70">{permission.preview}</span>
                {permission.state === 'pending' ? (
                  <>
                    <button type="button" onClick={() => decide(permission, 'allow')}>{copy.permissionAllow}</button>
                    <button type="button" onClick={() => decide(permission, 'deny')}>{copy.permissionDeny}</button>
                  </>
                ) : (
                  <span className="opacity-70">
                    {permission.state === 'expired' ? copy.permissionExpired : copy.permissionDecided}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </section>
      )}

      {artifacts.length > 0 && (
        <section aria-labelledby="remote-artifact-title" className="mt-3">
          <h3 id="remote-artifact-title" className="text-[12px] font-medium">{copy.artifactTitle}</h3>
          <ul className="text-[12px]">
            {artifacts.map((artifact) => (
              <li key={artifact.artifactId}>
                <span>{artifact.displayName}</span>
                <button
                  type="button"
                  className="ml-2"
                  onClick={() => remote && remote.artifactDownloadBegin(connection.sessionId, activeTask && activeTask.taskId, artifact.artifactId)}
                >
                  {artifact.previewKind === 'text' ? copy.open : copy.artifactDownload}
                </button>
                {artifact.previewKind !== 'text' && (
                  <span className="ml-2 opacity-70">
                    {artifact.previewKind === 'download_only' ? copy.artifactDownloadOnly : copy.artifactPreviewUnsupported}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </section>
      )}

      {histories.length > 0 && (
        <section aria-labelledby="remote-history-title" className="mt-3">
          <h3 id="remote-history-title" className="text-[12px] font-medium">{copy.historyTitle}</h3>
          <p className="text-[11px] opacity-70">{copy.offlineHint}</p>
          <ul className="text-[12px]">
            {histories.map((record) => (
              <li key={record.historyId}>
                {record.title}
                {record.state === 'offline_copy' && <span className="ml-2 opacity-70">{copy.offlineCopy}</span>}
              </li>
            ))}
          </ul>
        </section>
      )}

      {activeTask && (
        <button
          type="button"
          onClick={() => remote && sessionRef && remote
            .controllerTaskCancelSend(sessionRef, activeTask.taskId)
            .catch(function () {})}
        >
          {copy.cancel}
        </button>
      )}
      {error && <p role="alert" className="mt-1 text-[12px] text-[#EA4335]">{copy.errorGeneric}</p>}
    </div>
  );
}
