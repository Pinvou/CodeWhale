// Windows A 远程会话能力界面（DEV-23）。
//
// 只消费 DEV-20 的 bridge contract：模型列表是 opaque 描述、任务提交只传
// 选中的 modelId + 任务绑定，组件自身不读取 prefs / credential_ref，也不
// 构造模型包。远程上下文始终有显著标识；离线历史明确标注为副本；审批与
// 成果/附件状态都使用可恢复的显式文案。

import { useEffect, useMemo, useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';
import { isTaskTerminal, taskStatusKey } from './remote-control-logic.mjs';

export default function RemoteConversationView({ t, bs }) {
  const copy = (t && t.remoteConversation) || {};
  const connection = (bs && bs.controllerConnection) || {};
  const capabilities = (bs && bs.remoteCapabilities) || null;
  const models = (bs && bs.remoteModels) || [];
  const tasks = (bs && bs.tasks) || [];
  const permissions = (bs && bs.remotePermissions) || [];
  const histories = (bs && bs.remoteHistories) || [];
  const [modelSource, setModelSource] = useState('controlled_device');
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

  const send = async () => {
    if (!remote || !draft.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const taskId = `task-${Date.now().toString(36)}`;
      const payload = {
        connectionId: connection.connectionId,
        leaseEpoch: connection.leaseEpoch,
        taskId,
        sessionId: connection.sessionId,
        modelSource,
        mode,
        input: { text: draft.trim() },
      };
      await remote.submitRemoteTask(payload, modelSource === 'controller_temporary' ? selectedModelId : null);
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

      <section aria-labelledby="remote-model-source-title" className="mb-3">
        <h3 id="remote-model-source-title" className="text-[12px] font-medium">{copy.modelPickerTitle}</h3>
        <label className="mr-3 text-[11px]">
          <input
            type="radio"
            name="remote-model-source"
            checked={modelSource === 'controlled_device'}
            onChange={() => setModelSource('controlled_device')}
          />
          {copy.modelSourceDevice}
        </label>
        <label className="text-[11px]">
          <input
            type="radio"
            name="remote-model-source"
            checked={modelSource === 'controller_temporary'}
            onChange={() => setModelSource('controller_temporary')}
          />
          {copy.modelSourceMine}
        </label>
        {modelSource === 'controller_temporary' && (
          <ul className="mt-1 text-[11px]">
            {models.map((model) => (
              <li key={model.modelId}>
                <label>
                  <input
                    type="radio"
                    name="remote-model"
                    disabled={!model.exportable}
                    checked={selectedModelId === model.modelId}
                    onChange={() => setSelectedModelId(model.modelId)}
                  />
                  {model.displayName}
                  {!model.exportable && <span className="opacity-60"> · {copy.modelNotExportable}</span>}
                </label>
              </li>
            ))}
          </ul>
        )}
      </section>

      <ol className="remote-tasks" aria-live="polite">
        {tasks.length === 0 && <li className="text-[12px] opacity-70">{copy.empty}</li>}
        {tasks.map((task) => (
          <li key={task.taskId} className="mb-1 text-[12px]">
            <span className="mr-2">{copy[taskStatusKey(task.status)]}</span>
            {task.resultUnknown && <span className="opacity-70">{copy.resultUnknownHint}</span>}
          </li>
        ))}
      </ol>

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

      <div className="mt-3 flex items-end gap-2">
        <label className="sr-only" htmlFor="remote-composer">{copy.composerPlaceholder}</label>
        <textarea
          id="remote-composer"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          placeholder={copy.composerPlaceholder}
          rows={2}
          className="w-full rounded-lg border border-black/10 px-2 py-1 text-[13px] dark:border-white/15 dark:bg-transparent"
        />
        <button type="button" onClick={send} disabled={busy || !draft.trim()}>
          {busy ? copy.sending : copy.send}
        </button>
        {activeTask && (
          <button type="button" onClick={() => remote && remote.cancelRemoteTask(activeTask.taskId)}>
            {copy.cancel}
          </button>
        )}
      </div>
      {error && <p role="alert" className="mt-1 text-[12px] text-[#EA4335]">{copy.errorGeneric}</p>}
    </div>
  );
}
