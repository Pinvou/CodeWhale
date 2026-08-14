import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  ArrowLeft, BrainCircuit, CheckCircle2, ChevronDown, Code, FileText, Lightbulb,
  Archive, ClipboardList, Clock, Lock, Moon, MoreHorizontal, Package, Paperclip,
  PinIcon, PinOffIcon, Plus, RotateCcw, Search, Send, Sun, User, Users, X, XCircle,
} from '../../components/icons.jsx';
import { PinvouLogo } from '../../components/PinvouLogo.jsx';
import { MaterialInspector } from './MaterialInspector.jsx';
import { RelayAvatar } from './RelayAvatar.jsx';
import { TeamBoard } from './TeamBoard.jsx';
import {
  TEAM_RELAY_SOURCE_FILTERS, TEAM_RELAY_VIEWS, canUpdateRelayProgress, matchesRelayView,
  matchesSourceFilter, projectSnapshot, projectTeamBoard, relayViewCounts,
} from './team-relay-state.js';
import './team-relay.css';

const DEFAULT_TEAM_RELAY_SERVICE_URL = 'https://pinvou.com/pinvou3/team-relay';

function displayTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value || '') : date.toLocaleString([], {
    year: 'numeric', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}

function displayListTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value || '');
  const now = new Date();
  const sameDay = date.getFullYear() === now.getFullYear()
    && date.getMonth() === now.getMonth()
    && date.getDate() === now.getDate();
  return sameDay
    ? date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : date.toLocaleDateString([], { month: 'numeric', day: 'numeric' });
}

function bridgeState(domain, fallback) {
  try { return window.TauriBridge?.state?.get(domain) || fallback; } catch { return fallback; }
}

function relayError(copy, error) {
  const text = String(error || '');
  const code = text.split(':', 1)[0];
  return copy.errorMessages?.[code] || text;
}

function localDeviceName(fallback) {
  return String(window.navigator?.userAgentData?.platform || window.navigator?.platform || fallback).slice(0, 120);
}

function memberOptionLabel(member, members) {
  const duplicate = members.filter(candidate => candidate.display_name === member.display_name).length > 1;
  return duplicate && member.email_hint ? `${member.display_name} · ${member.email_hint}` : member.display_name;
}

function PersonAvatar({ name, seed, tone = 'blue' }) {
  return <RelayAvatar name={name} seed={seed || name} className={`team-relay-avatar tone-${tone}`} />;
}

function RelayCategoryIcon({ category }) {
  const Icon = { attention: CheckCircle2, waiting: Clock, all: ClipboardList, ended: Archive, hidden: Archive }[category] || ClipboardList;
  return <Icon size={16} />;
}

function CenterCard({ title, body, children }) {
  return <div className="team-relay-setup"><section><h2>{title}</h2><p>{body}</p>{children}</section></div>;
}

function AccountLogin({ copy, busy, error, onSubmit }) {
  return <CenterCard title={copy.setup.title} body={copy.setup.body}>
    <form className="team-relay-setup-form" onSubmit={event => {
      event.preventDefault();
      const input = Object.fromEntries(new window.FormData(event.currentTarget).entries());
      input.hub_url = DEFAULT_TEAM_RELAY_SERVICE_URL;
      input.device_name = localDeviceName(copy.setup.defaultDeviceName);
      onSubmit(input);
    }}>
      <div className="team-relay-service-card"><span>{copy.setup.service}</span><strong>{copy.setup.defaultService}</strong><small>{copy.setup.connectedService}</small></div>
      <label><span>{copy.setup.email}</span><input name="email" type="email" required maxLength="254" autoComplete="email" placeholder={copy.setup.emailPlaceholder} /></label>
      <label><span>{copy.setup.displayName}</span><input name="display_name" required maxLength="80" /></label>
      <p className="team-relay-setup-privacy">{copy.setup.privacy}</p>
      {error && <p className="team-relay-error" role="alert">{relayError(copy, error)}</p>}
      <button type="submit" className="team-relay-primary" disabled={busy}>{busy ? copy.syncing : copy.setup.submit}</button>
    </form>
  </CenterCard>;
}

const MAX_RELAY_ATTACHMENTS = 10;

function relayTitle(message) {
  return String(message || '').trim().split(/\r?\n/, 1)[0].slice(0, 80);
}

function sourceMaterialDefault(material) {
  return material.source !== 'workspace_change' || material.origin !== 'preexisting';
}

function workspaceStats(workspace) {
  const changes = workspace?.changes || [];
  return {
    total: changes.length,
    staged: changes.filter(change => change.staged).length,
    session: changes.filter(change => change.origin === 'session' || change.origin === 'preexisting_modified').length,
  };
}

function codingGitReference(context, copy) {
  const workspace = context?.workspace;
  if (!workspace?.git || !workspace.repositoryUrl || !workspace.commitSha) return null;
  const paths = (workspace.changes || [])
    .filter(change => change.origin !== 'preexisting')
    .slice(0, 20)
    .map(change => change.relativePath);
  return {
    repository_url: workspace.repositoryUrl,
    commit_sha: workspace.commitSha,
    branch: workspace.branch || '',
    pull_request_url: '',
    paths,
    note: copy.codingGitReferenceNote(workspace.changes.length, paths.length),
  };
}

function MaterialChips({ copy, materials, onRemove }) {
  if (!materials.length) return null;
  return <div className="team-relay-draft-materials">{materials.map(material => <span key={material.id}><FileText size={14} /><span>{material.name}</span><button type="button" onClick={() => onRemove(material.id)} aria-label={copy.removeMaterial(material.name)}><X size={12} /></button></span>)}</div>;
}

function RelaySelect({ value, onChange, options = [], groups = [], disabled = false, ariaLabel }) {
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(0);
  const rootRef = useRef(null);
  const normalizedGroups = groups.length ? groups : [{ label: '', options }];
  const flatOptions = normalizedGroups.flatMap(group => group.options || []);
  const selectedIndex = Math.max(0, flatOptions.findIndex(option => option.value === value));
  const selectedOption = flatOptions[selectedIndex] || null;

  useEffect(() => {
    if (!open) return undefined;
    setActiveIndex(selectedIndex);
    const closeOnOutsideClick = event => {
      if (!rootRef.current?.contains(event.target)) setOpen(false);
    };
    document.addEventListener('mousedown', closeOnOutsideClick);
    return () => document.removeEventListener('mousedown', closeOnOutsideClick);
  }, [open, selectedIndex]);

  const choose = (option, event) => {
    event?.preventDefault();
    event?.stopPropagation();
    if (option?.disabled) return;
    setOpen(false);
    onChange(option.value);
  };
  const onKeyDown = event => {
    if (disabled) return;
    if (event.key === 'Escape') {
      setOpen(false);
      return;
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      if (!open) {
        setOpen(true);
        setActiveIndex(selectedIndex);
        return;
      }
      const direction = event.key === 'ArrowDown' ? 1 : -1;
      setActiveIndex(current => (current + direction + flatOptions.length) % flatOptions.length);
      return;
    }
    if ((event.key === 'Enter' || event.key === ' ') && open) {
      event.preventDefault();
      choose(flatOptions[activeIndex]);
    }
  };

  let optionIndex = 0;
  return <div ref={rootRef} className={`team-relay-select${open ? ' is-open' : ''}`} onKeyDown={onKeyDown}>
    <button type="button" className="team-relay-select-trigger" disabled={disabled} aria-label={ariaLabel} aria-haspopup="listbox" aria-expanded={open} onClick={() => setOpen(current => !current)}><span>{selectedOption?.label || ''}</span><ChevronDown size={14} /></button>
    {open && <div className="team-relay-select-menu" role="listbox" aria-label={ariaLabel}>{normalizedGroups.map(group => <section key={group.label || 'default'}>{group.label && <strong>{group.label}</strong>}{(group.options || []).map(option => {
      const index = optionIndex++;
      return <button type="button" role="option" aria-selected={option.value === value} className={`${option.value === value ? 'is-selected' : ''}${index === activeIndex ? ' is-active' : ''}`} disabled={option.disabled} key={option.value} onMouseEnter={() => setActiveIndex(index)} onClick={event => choose(option, event)}>{option.label}</button>;
    })}</section>)}</div>}
  </div>;
}

function TeamDialog({ copy, theme, mode, busy, onClose, onSubmit }) {
  const dialogCopy = mode === 'create' ? copy.teams.createDialog : copy.teams.joinDialog;
  return createPortal(<div className="team-relay-modal-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}>
    <section className={`team-relay-modal team-relay-team-dialog theme-${theme === 'dark' ? 'dark' : 'light'}`} role="dialog" aria-modal="true" aria-labelledby="team-relay-team-dialog-title">
      <header><div><h2 id="team-relay-team-dialog-title">{dialogCopy.title}</h2><p>{dialogCopy.subtitle}</p></div><button type="button" className="team-relay-icon-button" onClick={onClose} aria-label={copy.close}><X size={18} /></button></header>
      <form className="team-relay-setup-form" onSubmit={event => { event.preventDefault(); onSubmit(Object.fromEntries(new window.FormData(event.currentTarget).entries())); }}>
        {mode === 'create' ? <label><span>{dialogCopy.name}</span><input name="name" required autoFocus maxLength="120" placeholder={dialogCopy.placeholder} /></label> : <label><span>{dialogCopy.code}</span><input name="join_code" required autoFocus minLength="8" maxLength="256" autoComplete="off" autoCapitalize="none" spellCheck={false} placeholder={dialogCopy.placeholder} /></label>}
        <p className="team-relay-form-hint"><Lock size={14} />{dialogCopy.hint}</p>
        <footer><button type="button" className="team-relay-secondary" onClick={onClose}>{copy.cancel}</button><button type="submit" className="team-relay-primary" disabled={busy}>{busy ? copy.syncing : dialogCopy.confirm}</button></footer>
      </form>
    </section>
  </div>, window.document.body);
}

function NoTeam({ copy, theme, busy, error, principal, onCreate, onJoin, onLogout }) {
  const [mode, setMode] = useState(null);
  const submit = async payload => {
    try {
      if (mode === 'create') await onCreate(payload.name);
      else await onJoin(payload.join_code);
      setMode(null);
    } catch { /* Bridge state renders the retryable error. */ }
  };
  return <>
    <header className="team-relay-page-header">
      <div className="team-relay-brand"><PinvouLogo className="team-relay-brand-logo" /><strong>{copy.brand}</strong></div>
      {principal && <div className="team-relay-header-actions"><RelayAvatar name={principal.display_name} seed={principal.id} /><button type="button" className="team-relay-secondary team-relay-no-team-logout" onClick={onLogout}>{copy.profile.logout}</button></div>}
    </header>
    <CenterCard title={copy.teams.emptyTitle} body={copy.teams.emptyBody}>
      <div className="team-relay-empty-team-actions">
        <button type="button" className="team-relay-secondary" onClick={() => setMode('join')}>{copy.teams.join}</button>
        <button type="button" className="team-relay-primary" onClick={() => setMode('create')}><Plus size={17} />{copy.teams.create}</button>
      </div>
      {error && <p className="team-relay-error" role="alert">{relayError(copy, error)}</p>}
    </CenterCard>
    {mode && <TeamDialog copy={copy} theme={theme} mode={mode} busy={busy} onClose={() => setMode(null)} onSubmit={submit} />}
  </>;
}

function ProfileDialog({ copy, theme, principal, busy, onClose, onSubmit, onLogout }) {
  return createPortal(<div className="team-relay-modal-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}>
    <section className={`team-relay-modal team-relay-team-dialog theme-${theme === 'dark' ? 'dark' : 'light'}`} role="dialog" aria-modal="true" aria-labelledby="team-relay-profile-dialog-title">
      <header><div><h2 id="team-relay-profile-dialog-title">{copy.profile.title}</h2><p>{copy.profile.subtitle}</p></div><button type="button" className="team-relay-icon-button" onClick={onClose} aria-label={copy.close}><X size={18} /></button></header>
      <form className="team-relay-setup-form" onSubmit={event => { event.preventDefault(); onSubmit(Object.fromEntries(new window.FormData(event.currentTarget).entries())); }}>
        <label><span>{copy.profile.email}</span><input name="email" type="email" defaultValue={principal.email || ''} required={!principal.email} disabled={!!principal.email} maxLength="254" autoComplete="email" placeholder={copy.setup.emailPlaceholder} /></label>
        <label><span>{copy.profile.displayName}</span><input name="display_name" defaultValue={principal.display_name} required maxLength="80" autoFocus /></label>
        <p className="team-relay-form-hint"><Lock size={14} />{principal.email ? copy.profile.emailLocked : copy.profile.bindEmail}</p>
        <footer className="team-relay-profile-footer"><button type="button" className="team-relay-text-danger" disabled={busy} onClick={onLogout}>{copy.profile.logout}</button><button type="button" className="team-relay-secondary" onClick={onClose}>{copy.cancel}</button><button type="submit" className="team-relay-primary" disabled={busy}>{busy ? copy.syncing : copy.profile.confirm}</button></footer>
      </form>
    </section>
  </div>, window.document.body);
}

function LogoutDialog({ copy, theme, busy, error, onClose, onConfirm }) {
  return createPortal(<div className="team-relay-modal-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget && !busy) onClose(); }}>
    <section className={`team-relay-modal team-relay-team-dialog theme-${theme === 'dark' ? 'dark' : 'light'}`} role="alertdialog" aria-modal="true" aria-labelledby="team-relay-logout-dialog-title" aria-describedby="team-relay-logout-dialog-body">
      <header><div><h2 id="team-relay-logout-dialog-title">{copy.profile.logoutTitle}</h2><p id="team-relay-logout-dialog-body">{copy.profile.logoutBody}</p></div><button type="button" className="team-relay-icon-button" disabled={busy} onClick={onClose} aria-label={copy.close}><X size={18} /></button></header>
      <div className="team-relay-logout-confirm">
        {error && <p className="team-relay-error" role="alert">{relayError(copy, error)}</p>}
        <footer><button type="button" className="team-relay-secondary" disabled={busy} onClick={onClose}>{copy.cancel}</button><button type="button" className="team-relay-danger" disabled={busy} onClick={onConfirm}>{busy ? copy.profile.loggingOut : copy.profile.logoutConfirm}</button></footer>
      </div>
    </section>
  </div>, window.document.body);
}

function TeamJoinCodeBanner({ copy, state, owner, busy, onCopy, onRotate, onVisibility, onDisable, onClose }) {
  const code = state.join_password;
  const unavailable = state.configured && !state.recoverable;
  const hidden = state.configured && !state.visible;
  const message = !state.configured
    ? copy.joinCode.disabled
    : unavailable
      ? owner ? copy.joinCode.legacyOwner : copy.joinCode.legacyMember
      : hidden
        ? copy.joinCode.ownerOnly
        : state.visibility === 'owners'
          ? copy.joinCode.ownerVisibility
          : copy.joinCode.memberVisibility;
  return <section className="team-relay-invite" aria-live="polite">
    <div className="team-relay-invite-main"><div className="team-relay-invite-heading"><strong>{copy.joinPasswordReady}</strong>{code && <code>{code}</code>}</div><p>{message}</p></div>
    <div className="team-relay-invite-actions">
      {code && <button type="button" className="team-relay-secondary" onClick={onCopy}>{copy.copyJoinPassword}</button>}
      {owner && <>
        <button type="button" className="team-relay-secondary" disabled={busy} onClick={onRotate}>{state.configured ? copy.joinCode.rotate : copy.joinCode.enable}</button>
        {state.configured && state.recoverable && <button type="button" className="team-relay-secondary" disabled={busy} onClick={onVisibility}>{state.visibility === 'members' ? copy.joinCode.hideFromMembers : copy.joinCode.showToMembers}</button>}
        {state.configured && <button type="button" className="team-relay-text-danger" disabled={busy} onClick={onDisable}>{copy.joinCode.disable}</button>}
      </>}
      <button type="button" className="team-relay-icon-button" onClick={onClose} aria-label={copy.close}><X size={16} /></button>
    </div>
  </section>;
}

function RelayDialog({ copy, theme, type, task, teams, teamId, onTeamChange, members, busy, uploadProgress, taskWorkspace, workspaceLoading, onClose, onSubmit, onLoadSourceSessions, onReadSourceSession, onPickFiles, onMountMaterials, onPickAgentWorkspace, onLoadCodeAgents }) {
  const [files, setFiles] = useState([]);
  const dialogCopy = copy.dialogs[type];
  const [draft, setDraft] = useState(() => ({
    message: '',
    expectedNext: '',
  }));
  const [receiverId, setReceiverId] = useState(() => members[0]?.id || '');
  useEffect(() => {
    if (!members.some(member => member.id === receiverId)) setReceiverId(members[0]?.id || '');
  }, [members, receiverId]);
  const [sourceSessions, setSourceSessions] = useState([]);
  const [sourceSessionsStatus, setSourceSessionsStatus] = useState('loading');
  const [sourceSessionKey, setSourceSessionKey] = useState('');
  const [sourceContext, setSourceContext] = useState(null);
  const [sourceContextStatus, setSourceContextStatus] = useState('idle');
  const [selectedSourceMaterialIds, setSelectedSourceMaterialIds] = useState([]);
  const [mountingMaterials, setMountingMaterials] = useState(false);
  const [materialError, setMaterialError] = useState('');
  const [agentWorkspacePath, setAgentWorkspacePath] = useState(() => taskWorkspace?.agent_session?.workspace_path || '');
  const [agentMode, setAgentMode] = useState(() => taskWorkspace?.agent_session?.kind || (task.events.some(event => event.delivery.git_references.length) ? 'coding' : 'chat'));
  const [agentId, setAgentId] = useState(() => taskWorkspace?.agent_session?.agent_id || 'pinvou');
  const [codeAgents, setCodeAgents] = useState([]);
  const [codeAgentsLoading, setCodeAgentsLoading] = useState(false);
  const [selectedAgentMaterialIds, setSelectedAgentMaterialIds] = useState([]);
  const [useNewAgentSession, setUseNewAgentSession] = useState(false);
  const sourceRequest = useRef(0);
  const defaultSourceLoaded = useRef(false);
  const agentMaterialsInitialized = useRef(false);
  useEffect(() => {
    const keydown = event => { if (event.key === 'Escape') onClose(); };
    window.addEventListener('keydown', keydown);
    return () => window.removeEventListener('keydown', keydown);
  }, [onClose]);
  useEffect(() => {
    if (taskWorkspace?.agent_session?.workspace_path) {
      setAgentWorkspacePath(taskWorkspace.agent_session.workspace_path);
    }
    if (taskWorkspace?.agent_session?.kind) setAgentMode(taskWorkspace.agent_session.kind);
    if (taskWorkspace?.agent_session?.agent_id) setAgentId(taskWorkspace.agent_session.agent_id);
  }, [taskWorkspace?.agent_session?.agent_id, taskWorkspace?.agent_session?.kind, taskWorkspace?.agent_session?.workspace_path]);
  useEffect(() => {
    if (type !== 'agent' || workspaceLoading || agentMaterialsInitialized.current) return;
    const introduced = new Set(taskWorkspace?.agent_session?.material_ids || []);
    setSelectedAgentMaterialIds(task.files.filter(file => !introduced.has(file.id)).map(file => file.id));
    agentMaterialsInitialized.current = true;
  }, [task.files, taskWorkspace?.agent_session?.material_ids, type, workspaceLoading]);
  useEffect(() => {
    if (type !== 'agent' || !onLoadCodeAgents) return undefined;
    let active = true;
    setCodeAgentsLoading(true);
    Promise.resolve(onLoadCodeAgents())
      .then(items => { if (active) setCodeAgents(Array.isArray(items) ? items : []); })
      .catch(() => { if (active) setCodeAgents([]); })
      .finally(() => { if (active) setCodeAgentsLoading(false); });
    return () => { active = false; };
  }, [onLoadCodeAgents, type]);
  useEffect(() => {
    if (type === 'agent' || type === 'cancelWork') {
      setSourceSessionsStatus('loaded');
      return undefined;
    }
    let active = true;
    Promise.resolve(onLoadSourceSessions())
      .then(sessions => {
        if (!active) return;
        setSourceSessions(Array.isArray(sessions) ? sessions : []);
        setSourceSessionsStatus('loaded');
      })
      .catch(() => {
        if (active) setSourceSessionsStatus('failed');
      });
    return () => { active = false; };
  }, [onLoadSourceSessions, type]);

  if (!dialogCopy) return null;
  const pick = async () => {
    setMaterialError('');
    try {
      const selected = await onPickFiles();
      setFiles(current => {
        const byPath = new Map(current.map(material => [material.localPath, material]));
        selected.forEach(material => byPath.set(material.localPath, material));
        return [...byPath.values()];
      });
    } catch {
      setMaterialError('pick_failed');
    }
  };
  const loadSource = async key => {
    setSourceSessionKey(key);
    setSourceContext(null);
    setSelectedSourceMaterialIds([]);
    setMaterialError('');
    const request = ++sourceRequest.current;
    if (!key) {
      setSourceContextStatus('idle');
      return;
    }
    const source = sourceSessions.find(session => `${session.kind}:${session.id}` === key);
    if (!source) {
      setSourceContextStatus('failed');
      return;
    }
    setSourceContextStatus('loading');
    try {
      const context = await onReadSourceSession(source);
      if (sourceRequest.current !== request) return;
      setSourceContext(context);
      setSelectedSourceMaterialIds((context.materials || []).filter(sourceMaterialDefault).map(material => material.id));
      setSourceContextStatus('loaded');
    } catch {
      if (sourceRequest.current === request) setSourceContextStatus('failed');
    }
  };
  useEffect(() => {
    if (defaultSourceLoaded.current || sourceSessionsStatus !== 'loaded' || !taskWorkspace?.agent_session) return;
    const link = taskWorkspace.agent_session;
    const key = `${link.kind}:${link.session_id}`;
    if (!sourceSessions.some(session => `${session.kind}:${session.id}` === key)) return;
    defaultSourceLoaded.current = true;
    loadSource(key);
  }, [sourceSessions, sourceSessionsStatus, taskWorkspace?.agent_session]);
  const submit = async event => {
    event.preventDefault();
    setMaterialError('');
    setMountingMaterials(true);
    try {
      const sourceMaterials = (sourceContext?.materials || []).filter(material => selectedSourceMaterialIds.includes(material.id));
      const byPath = new Map([...sourceMaterials, ...files].map(material => [material.localPath, material]));
      const mounted = byPath.size ? await onMountMaterials([...byPath.values()], teamId) : [];
      const gitReference = codingGitReference(sourceContext, copy);
      await onSubmit({
        team_id: teamId,
        receiver: receiverId,
        title: type === 'assign' ? relayTitle(draft.message) : task.title,
        message: draft.message.trim(),
        attachment_ids: mounted.map(file => file.id),
        delivery: {
          schema_version: 1,
          artifact_role: dialogCopy.artifactRole,
          expected_next: draft.expectedNext.trim(),
          open_questions: '',
          git_references: gitReference ? [gitReference] : [],
        },
      });
    } catch {
      setMaterialError('mount_failed');
    } finally {
      setMountingMaterials(false);
    }
  };
  const selectedSourceSession = sourceSessions.find(session => `${session.kind}:${session.id}` === sourceSessionKey) || null;
  const targetName = type === 'assign' || type === 'handoff'
    ? members.find(member => member.id === receiverId)?.display_name || copy.dash
    : type === 'progress' ? task.current
      : type === 'deliver' ? task.finalRecipient : task.owner;
  const selectedSourceMaterials = (sourceContext?.materials || []).filter(material => selectedSourceMaterialIds.includes(material.id));
  const combinedPaths = new Set([...selectedSourceMaterials, ...files].map(material => material.localPath));
  const attachmentCount = combinedPaths.size;
  const attachmentOverflow = attachmentCount > MAX_RELAY_ATTACHMENTS;
  const workspace = sourceContext?.workspace;
  const stats = workspaceStats(workspace);
  const gitReference = codingGitReference(sourceContext, copy);
  const sourcePending = !!sourceSessionKey && sourceContextStatus !== 'loaded';
  const hasGitReferences = task.events.some(event => event.delivery.git_references.length);
  const linkedSession = taskWorkspace?.agent_session || null;
  const reusingLinkedSession = !!linkedSession && !useNewAgentSession;
  const introducedMaterialIds = new Set(linkedSession?.material_ids || []);
  const selectableAgentFiles = reusingLinkedSession
    ? task.files.filter(file => !introducedMaterialIds.has(file.id))
    : task.files;
  const introducedMaterialCount = reusingLinkedSession
    ? task.files.filter(file => introducedMaterialIds.has(file.id)).length
    : 0;
  const selectedNewMaterialCount = selectedAgentMaterialIds.filter(id => !introducedMaterialIds.has(id)).length;
  const latestSequence = task.events.reduce((latest, event) => Math.max(latest, Number(event.sequence) || 0), 0);
  const relayContextChanged = latestSequence > Number(linkedSession?.last_synced_sequence || 0);
  const pickAgentWorkspace = async () => {
    setMaterialError('');
    try {
      const selected = await onPickAgentWorkspace();
      if (selected) setAgentWorkspacePath(selected);
    } catch {
      setMaterialError('workspace_pick_failed');
    }
  };
  const toggleNewAgentSession = enabled => {
    setUseNewAgentSession(enabled);
    if (enabled) {
      setSelectedAgentMaterialIds(task.files.map(file => file.id));
      return;
    }
    const introduced = new Set(linkedSession?.material_ids || []);
    setSelectedAgentMaterialIds(task.files.filter(file => !introduced.has(file.id)).map(file => file.id));
    if (linkedSession?.kind) setAgentMode(linkedSession.kind);
    if (linkedSession?.agent_id) setAgentId(linkedSession.agent_id);
    setAgentWorkspacePath(linkedSession?.workspace_path || '');
  };

  return createPortal(
    <div className="team-relay-modal-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}>
      <section className={`team-relay-modal theme-${theme === 'dark' ? 'dark' : 'light'}${!['agent', 'cancelWork'].includes(type) ? ' is-compose' : ''}${type === 'agent' ? ' is-agent' : ''}`} role="dialog" aria-modal="true" aria-labelledby={`relay-dialog-${type}`}>
        <header><div><h2 id={`relay-dialog-${type}`}>{dialogCopy.title}</h2><p>{dialogCopy.subtitle}</p></div><button type="button" className="team-relay-icon-button" onClick={onClose} aria-label={copy.close}><X size={18} /></button></header>
        {type === 'agent' ? <div className="team-relay-agent-preview">
          <div className="team-relay-agent-task-summary"><BrainCircuit size={18} /><div><span>{copy.currentWork}</span><strong>{task.title}</strong></div><small>{copy.preparedForAgent(task.files.length)}</small></div>
          {linkedSession && <section className="team-relay-agent-session-choice"><h3>{copy.agentSessionChoice}</h3><div>
            <label className={!useNewAgentSession ? 'is-selected' : ''}><input type="radio" name="agent-session-choice" checked={!useNewAgentSession} onChange={() => toggleNewAgentSession(false)} /><CheckCircle2 size={17} /><span><strong>{copy.reuseAgentSession}</strong><small>{linkedSession.kind === 'coding' ? copy.codingSession : copy.chatSession}{linkedSession.agent_id ? ` · ${linkedSession.agent_id === 'pinvou' ? copy.defaultPinvouAgent : codeAgents.find(agent => agent.agent_id === linkedSession.agent_id)?.agent_name || linkedSession.agent_id}` : ''}</small></span></label>
            <label className={useNewAgentSession ? 'is-selected' : ''}><input type="radio" name="agent-session-choice" checked={useNewAgentSession} onChange={() => toggleNewAgentSession(true)} /><Plus size={17} /><span><strong>{copy.useNewAgentSession}</strong><small>{copy.useNewAgentSessionHint}</small></span></label>
          </div></section>}
          {(!linkedSession || useNewAgentSession) && <section className="team-relay-agent-routing"><label><span>{copy.agentWorkMode}</span><RelaySelect value={agentMode} onChange={setAgentMode} ariaLabel={copy.agentWorkMode} options={[{ value: 'chat', label: copy.chatWorkMode }, { value: 'coding', label: copy.pinvouCodeWorkMode }]} /></label>{agentMode === 'coding' && <label><span>{copy.codeAgent}</span><RelaySelect value={agentId} onChange={setAgentId} disabled={codeAgentsLoading} ariaLabel={copy.codeAgent} groups={[{ label: '', options: [{ value: 'pinvou', label: copy.defaultPinvouAgent }, ...(agentId !== 'pinvou' && !codeAgents.some(agent => agent.agent_id === agentId) ? [{ value: agentId, label: agentId }] : [])] }, ...(codeAgents.length ? [{ label: copy.otherAcpAgents, options: codeAgents.map(agent => ({ value: agent.agent_id, label: `${agent.agent_name}${!agent.installed ? ` · ${copy.agentNotReady}` : ''}` })) }] : [])]} /></label>}</section>}
          {agentMode === 'coding' && <section className="team-relay-agent-workspace"><div><Code size={17} /><span><strong>{copy.localCodeWorkspace}</strong><small>{hasGitReferences ? copy.localCodeWorkspaceHint : copy.localCodeWorkspaceOptionalHint}</small></span></div>{!reusingLinkedSession && <button type="button" className="team-relay-secondary" onClick={pickAgentWorkspace}>{agentWorkspacePath ? copy.changeWorkspace : copy.chooseWorkspace}</button>}{agentWorkspacePath && <code>{agentWorkspacePath}</code>}</section>}
          {!!task.files.length && <section className="team-relay-agent-materials"><header><div><Paperclip size={16} /><span><strong>{copy.agentMaterials}</strong><small>{reusingLinkedSession ? copy.continueMaterialSummary(selectedNewMaterialCount) : copy.firstMaterialSummary(task.files.length)}</small></span></div>{!!selectableAgentFiles.length && <button type="button" onClick={() => setSelectedAgentMaterialIds(selectedAgentMaterialIds.length === selectableAgentFiles.length ? [] : selectableAgentFiles.map(file => file.id))}>{selectedAgentMaterialIds.length === selectableAgentFiles.length ? copy.clearSelection : copy.selectAll}</button>}</header>{selectableAgentFiles.map(file => <label key={file.id}><input type="checkbox" checked={selectedAgentMaterialIds.includes(file.id)} onChange={event => setSelectedAgentMaterialIds(current => event.target.checked ? [...new Set([...current, file.id])] : current.filter(id => id !== file.id))} /><FileText size={15} /><span>{file.original_name}</span></label>)}{reusingLinkedSession && !!introducedMaterialCount && <div className="team-relay-agent-materials-introduced"><CheckCircle2 size={14} />{copy.introducedMaterialSummary(introducedMaterialCount)}</div>}</section>}
          {hasGitReferences && <section className="team-relay-agent-git"><header><Code size={16} /><strong>{copy.gitContext}</strong>{linkedSession && relayContextChanged && <small>{copy.contextUpdated}</small>}</header>{task.events.flatMap(event => event.delivery.git_references.map((reference, index) => <div key={`${event.id}-agent-git-${index}`}><code>{reference.repository_url}</code><span>{reference.commit_sha ? reference.commit_sha.slice(0, 12) : copy.mutableGitWarning}{reference.branch ? ` · ${reference.branch}` : ''}</span>{!!reference.paths.length && <small>{reference.paths.join(' · ')}</small>}</div>))}</section>}
          <p className="team-relay-form-hint"><Lock size={14} />{copy.agentPrivacy}</p>
          {materialError && <p className="team-relay-error" role="alert">{copy.agentPrepareFailed}</p>}
          <footer><button type="button" className="team-relay-secondary" onClick={onClose}>{copy.cancel}</button><button type="button" className="team-relay-primary" disabled={busy || workspaceLoading} onClick={() => onSubmit({ mode: agentMode, agent_id: agentMode === 'coding' ? agentId : null, workspace_path: agentMode === 'coding' ? agentWorkspacePath || null : null, material_ids: selectedAgentMaterialIds, latest_sequence: latestSequence, context_changed: useNewAgentSession || relayContextChanged || selectedAgentMaterialIds.length > 0, use_new_session: useNewAgentSession })}>{workspaceLoading ? copy.preparingMaterials : dialogCopy.confirm}</button></footer>
        </div> : type === 'cancelWork' ? <form className="team-relay-cancel-form" onSubmit={event => { event.preventDefault(); onSubmit({ message: draft.message.trim() }); }}>
          <label><span>{dialogCopy.reasonLabel}</span><textarea rows="5" required maxLength="10000" value={draft.message} onChange={event => setDraft(current => ({ ...current, message: event.target.value }))} placeholder={dialogCopy.messagePlaceholder} autoFocus /></label>
          <p><Lock size={14} />{dialogCopy.auditHint}</p>
          <footer><button type="button" className="team-relay-secondary" onClick={onClose}>{copy.cancel}</button><button type="submit" className="team-relay-danger" disabled={busy || !draft.message.trim()}><XCircle size={16} />{dialogCopy.confirm}</button></footer>
        </form> : <form onSubmit={submit} className="team-relay-compose-form team-relay-direct-form">
          <div className="team-relay-compose-grid">
            <section className="team-relay-compose-context">
              {type === 'assign' && <div className="team-relay-recipient-row"><label><span>{copy.views.teams}</span><RelaySelect value={teamId} onChange={onTeamChange} ariaLabel={copy.teams.switchLabel} options={teams.map(team => ({ value: team.id, label: team.name }))} /></label></div>}
              {(type === 'assign' || type === 'handoff') ? <div className="team-relay-recipient-row"><label><span>{dialogCopy.routeLabel}</span><RelaySelect value={receiverId} onChange={setReceiverId} ariaLabel={dialogCopy.routeLabel} options={members.map(member => ({ value: member.id, label: memberOptionLabel(member, members) }))} /></label></div> : <div className="team-relay-fixed-recipient"><span>{dialogCopy.routeLabel}</span><strong>{targetName}</strong></div>}
              <label className="team-relay-session-select"><span>{copy.sourceSession}</span><RelaySelect value={sourceSessionKey} onChange={loadSource} disabled={sourceSessionsStatus === 'loading' || mountingMaterials} ariaLabel={copy.sourceSession} groups={[{ label: '', options: [{ value: '', label: sourceSessionsStatus === 'loading' ? copy.sourceSessionsLoading : copy.noSourceSession }] }, ...(sourceSessions.some(session => session.kind === 'chat') ? [{ label: copy.chatSessions, options: sourceSessions.filter(session => session.kind === 'chat').map(session => ({ value: `chat:${session.id}`, label: session.title })) }] : []), ...(sourceSessions.some(session => session.kind === 'coding') ? [{ label: copy.codingSessions, options: sourceSessions.filter(session => session.kind === 'coding').map(session => ({ value: `coding:${session.id}`, label: session.title })) }] : [])]} />{sourceSessionsStatus === 'failed' && <small className="is-error">{copy.sourceSessionsFailed}</small>}</label>
              {sourceContextStatus === 'loading' && <div className="team-relay-session-loading">{copy.sourceSessionLoading}</div>}
              {sourceContextStatus === 'failed' && <div className="team-relay-session-error" role="alert"><span>{copy.sourceSessionFailed}</span><button type="button" onClick={() => loadSource(sourceSessionKey)}>{copy.retry}</button></div>}
              {selectedSourceSession && sourceContextStatus === 'loaded' ? <section className={`team-relay-session-summary kind-${selectedSourceSession.kind}`}>
                <header><div><span>{selectedSourceSession.kind === 'coding' ? copy.codingSession : copy.chatSession}</span><strong>{selectedSourceSession.title}</strong></div><b>{copy.selectedSourceMaterials(selectedSourceMaterials.length)}</b></header>
                {selectedSourceSession.kind === 'chat' ? <p>{copy.chatSessionMaterialHint(sourceContext?.materials?.length || 0)}</p> : <div className="team-relay-coding-summary">
                  <div className="team-relay-code-reference"><Code size={16} /><div><strong>{workspace?.repositoryUrl || copy.noGitRemote}</strong><code>{workspace?.commitSha ? workspace.commitSha.slice(0, 12) : copy.noGitCommit}</code><span>{workspace?.branch || copy.detachedHead}</span></div></div>
                  <div className="team-relay-code-stats"><span>{copy.codingChanges(stats.total)}</span><span>{copy.codingSessionChanges(stats.session)}</span><span>{copy.codingStaged(stats.staged)}</span></div>
                  {!!stats.total && <p className="team-relay-git-warning">{gitReference ? copy.dirtyGitWarning(stats.total) : copy.unshareableGitWarning}</p>}
                </div>}
                <MaterialChips copy={copy} materials={selectedSourceMaterials} onRemove={id => setSelectedSourceMaterialIds(current => current.filter(value => value !== id))} />
                <small><Lock size={11} />{copy.sourceSessionPrivacy}</small>
              </section> : sourceContextStatus === 'idle' && <div className="team-relay-session-placeholder"><Package size={24} /><strong>{copy.noSourceSession}</strong><span>{copy.sourceSessionPrivacy}</span></div>}
            </section>
            <section className="team-relay-compose-request">
              <label className="team-relay-prompt-card"><span><b>01</b>{dialogCopy.taskLabel}</span><textarea name="source_message" rows="4" required maxLength="10000" value={draft.message} onChange={event => setDraft(current => ({ ...current, message: event.target.value }))} placeholder={dialogCopy.messagePlaceholder} autoFocus /></label>
              <label className="team-relay-prompt-card"><span><b>02</b>{dialogCopy.acceptanceLabel}</span><textarea name="source_expected_next" rows="3" required={!dialogCopy.acceptanceOptional} maxLength="2000" value={draft.expectedNext} onChange={event => setDraft(current => ({ ...current, expectedNext: event.target.value }))} placeholder={dialogCopy.acceptancePlaceholder || copy.deliveryGoalPlaceholder} /></label>
              <fieldset className="team-relay-direct-attachments"><div className="team-relay-attachment-icon"><Paperclip size={17} /></div><div className="team-relay-attachment-copy"><legend>{copy.uploadAttachments}</legend><span>{copy.selectedAttachmentCount(attachmentCount)} · {copy.attachmentUploadHint}</span></div><button type="button" className="team-relay-secondary" onClick={pick} disabled={mountingMaterials}><Plus size={14} />{copy.addFiles}</button>
                <MaterialChips copy={copy} materials={files} onRemove={id => setFiles(current => current.filter(material => material.id !== id))} />
                {mountingMaterials && uploadProgress && <progress value={uploadProgress.sent_bytes} max={uploadProgress.total_bytes || 1} aria-label={copy.uploading} />}
              </fieldset>
              {attachmentOverflow && <p className="team-relay-error" role="alert">{copy.attachmentLimit(MAX_RELAY_ATTACHMENTS)}</p>}
              {materialError && <p className="team-relay-error" role="alert">{copy.materialMountFailed}</p>}
            </section>
          </div>
          <footer className="team-relay-direct-footer"><p><Lock size={13} /><span>{copy.attachmentUploadHint} {copy.sourceSessionPrivacy}</span></p><div><button type="button" className="team-relay-secondary" onClick={onClose} disabled={mountingMaterials}>{copy.cancel}</button><button type="submit" className="team-relay-primary" disabled={busy || mountingMaterials || sourcePending || attachmentOverflow || !draft.message.trim() || (!dialogCopy.acceptanceOptional && !draft.expectedNext.trim()) || ((type === 'assign' || type === 'handoff') && !receiverId)}><CheckCircle2 size={16} />{mountingMaterials ? attachmentCount ? copy.uploadingAndSending : copy.sending : dialogCopy.confirm}</button></div></footer>
        </form>}
      </section>
    </div>, document.body,
  );
}

function WorkCard({ copy, task, selected, onClick }) {
  const lastEvent = task.events[task.events.length - 1];
  return <button type="button" className={`team-relay-card${selected ? ' is-selected' : ''}${task.pinned ? ' is-pinned' : ''}`} onClick={onClick} aria-pressed={selected} data-testid={`team-relay-task-${task.id}`}>
    <span className="team-relay-card-heading"><strong>{task.pinned && <PinIcon size={13} aria-label={copy.pinned} />}{task.title}</strong><span className="team-relay-card-heading-meta">{!!task.unreadCount && <b className="team-relay-unread-badge" aria-label={copy.unread.updates(task.unreadCount)}>{task.unreadCount > 99 ? '99+' : task.unreadCount}</b>}<time>{displayListTime(lastEvent?.time || task.updated)}</time></span></span>
    <span className="team-relay-card-status"><PersonAvatar name={task.current === copy.dash ? task.owner : task.current} seed={task.currentActorId || task.raw.owner_id} tone={selected ? 'green' : 'orange'} /><span className="team-relay-card-status-text">{task.status}</span><em className={`tone-${task.category}`}>{copy.statuses[task.category]}</em></span>
    <span className="team-relay-card-meta"><span className="team-relay-card-team"><Users size={12} />{task.teamName}</span>{!!task.files.length && <small><Paperclip size={11} />{copy.attachments(task.files.length)}</small>}</span>
  </button>;
}

function TeamDirectory({ copy, teams, busy, onJoin, onCreate, onOpen }) {
  const totalWork = teams.reduce((sum, team) => sum + Number(team.open_work_count || 0), 0);
  return <section className="team-relay-directory" data-testid="team-relay-directory">
    <header>
      <div><span>{copy.views.teams}</span><h1>{copy.teams.directoryTitle}</h1><p>{copy.teams.directorySubtitle}</p></div>
      <div><button type="button" className="team-relay-secondary" disabled={busy} onClick={onJoin}>{copy.teams.join}</button><button type="button" className="team-relay-primary" disabled={busy} onClick={onCreate}><Plus size={16} />{copy.teams.create}</button></div>
    </header>
    <div className="team-relay-directory-summary"><span><small>{copy.teams.joinedCount}</small><strong>{teams.length}</strong></span><span><small>{copy.teams.totalOpenWork}</small><strong>{totalWork}</strong></span></div>
    <div className="team-relay-directory-grid">{teams.map(team => <button type="button" className="team-relay-directory-card" onClick={() => onOpen(team.id)} key={team.id}>
      <span className="team-relay-directory-avatar"><Users size={22} /></span>
      <span><strong>{team.name}</strong><small>{copy.teams.teamSummary(team.member_count || 0, team.open_work_count || 0)}</small></span>
      <b>{copy.board.roles[team.role] || team.role}</b>
      <em>{copy.teams.openTeam}</em>
    </button>)}</div>
  </section>;
}

function WorkThread({ copy, task, meId, taskWorkspace, onBack, onDialog, onAction, onPreference }) {
  const [menuOpen, setMenuOpen] = useState(false);
  useEffect(() => setMenuOpen(false), [task.id]);
  const finished = new Set(['closed', 'cancelled']).has(task.raw.phase);
  const showFinalRecipient = task.raw.final_recipient_id !== task.raw.initiator_id;
  return <section className="team-relay-detail" data-testid="team-relay-detail">
    <header className="team-relay-detail-header"><button type="button" className="team-relay-mobile-back" onClick={onBack} aria-label={copy.backToList}><ArrowLeft size={18} /></button><div className="team-relay-detail-heading"><div className="team-relay-detail-eyebrow"><span>{copy.workId(String(task.id).slice(0, 12))}</span><b className={`tone-${task.category}`}>{task.status}</b></div><h2>{task.title}</h2></div><div className="team-relay-task-menu"><button type="button" className="team-relay-icon-button" aria-label={copy.more} aria-haspopup="dialog" aria-expanded={menuOpen} onClick={() => setMenuOpen(value => !value)}><MoreHorizontal size={20} /></button>{menuOpen && <div role="dialog" aria-label={copy.taskInfo}>
      <section className="team-relay-task-info"><strong>{copy.taskInfo}</strong><span><small>{copy.initiator}</small><b>{task.initiator}</b></span>{showFinalRecipient && <span><small>{copy.finalRecipient}</small><b>{task.finalRecipient}</b></span>}</section>
      <button type="button" onClick={() => { setMenuOpen(false); onPreference({ pinned: !task.pinned }); }}>{task.pinned ? <PinOffIcon size={16} /> : <PinIcon size={16} />}{task.pinned ? copy.unpinWork : copy.pinWork}</button>
      {finished && <button type="button" onClick={() => { setMenuOpen(false); onPreference({ hidden: !task.hidden }); }}><Archive size={16} />{task.hidden ? copy.restoreWork : copy.hideWork}</button>}
      {task.raw.initiator_id === meId && !finished && <button type="button" className="is-danger" onClick={() => { setMenuOpen(false); onDialog('cancelWork'); }}><XCircle size={16} />{copy.cancelWork}</button>}
    </div>}</div></header>
    <div className="team-relay-thread-summary">
      <section><small>{copy.currentActor}</small><div><PersonAvatar name={task.current} seed={task.currentActorId} tone="green" /><span><strong>{task.current}</strong><b>{copy.owner}: {task.owner}</b></span></div></section>
      <div className={`team-relay-banner tone-${task.category}`}><Lightbulb size={18} /><div><strong>{task.banner.title}</strong><p>{task.banner.body}</p></div></div>
    </div>
    {task.acceptedPackage && <div className="team-relay-accepted-package"><CheckCircle2 size={18} /><div><strong>{copy.acceptedPackage(task.acceptedPackage.sequence)}</strong><p>{task.acceptedPackage.message}</p><small>{copy.acceptedPackageSummary(task.acceptedPackage.files.length, task.acceptedPackage.delivery.git_references.length)}</small></div></div>}
    <h3 className="team-relay-history-title">{copy.history}</h3>
    <div className="team-relay-timeline">{task.events.map((event, index) => <article className="team-relay-event" id={`team-relay-package-${event.id}`} key={event.id || `${task.id}-${index}`}><PersonAvatar name={event.avatar || event.actor} seed={event.actorId} tone={index === task.events.length - 1 ? 'green' : 'gray'} /><div className="team-relay-event-card"><header><div><strong>{event.title}</strong><span className={`team-relay-package-badge role-${event.delivery.artifact_role}`}><Package size={12} />{copy.packageId(event.sequence)} · {copy.artifactRoles[event.delivery.artifact_role]}</span></div><time>{displayTime(event.time)}</time></header><p>“{event.message}”</p>{!!event.files.length && <div className="team-relay-files">{event.files.map(file => <span key={file.id}><FileText size={15} />{file.original_name}<small>{copy.artifactRoles[event.delivery.artifact_role]}</small></span>)}</div>}{event.delivery.expected_next && <div className="team-relay-package-line"><b>{copy.expectedNext}:</b> {event.delivery.expected_next}</div>}{event.delivery.open_questions && <div className="team-relay-package-line"><b>{copy.openQuestions}:</b> {event.delivery.open_questions}</div>}{event.delivery.git_references.map((reference, gitIndex) => <div className="team-relay-git-reference" key={`${event.id}-git-${gitIndex}`}><Code size={15} /><div><strong>{copy.gitReference}</strong><code>{reference.repository_url}</code><span>{reference.commit_sha ? `@ ${reference.commit_sha}` : copy.mutableGitWarning}{reference.branch ? ` · ${reference.branch}` : ''}</span>{!!reference.paths.length && <small>{reference.paths.join(' · ')}</small>}{reference.pull_request_url && <code>{reference.pull_request_url}</code>}{reference.note && <p>{reference.note}</p>}</div></div>)}</div></article>)}</div>
    <footer className="team-relay-action-bar"><p><Lock size={15} />{copy.privacyNotice}</p><div>
      {task.category === 'pending' && task.currentActorId === meId && <>
        <button type="button" className="team-relay-secondary" onClick={() => onDialog('agent')}><BrainCircuit size={17} />{taskWorkspace?.agent_session ? copy.continueWithAgent : copy.useMyAgent}</button>
        {canUpdateRelayProgress(task.raw, meId) && <button type="button" className="team-relay-secondary" onClick={() => onDialog('progress')}><ClipboardList size={17} />{copy.updateProgress}</button>}
        {task.raw.owner_id === meId && <button type="button" className="team-relay-secondary" onClick={() => onDialog('handoff')}><User size={17} />{copy.askForHelp}</button>}
        {task.raw.owner_id !== meId && <button type="button" className="team-relay-primary" onClick={() => onDialog('return')}><RotateCcw size={17} />{copy.returning}</button>}
        {task.raw.owner_id === meId && <button type="button" className="team-relay-primary" onClick={() => onDialog('deliver')}><Send size={17} />{copy.finalDelivery}</button>}
      </>}
      {task.category === 'receive' && <><button type="button" className="team-relay-secondary" onClick={() => onDialog('supplement')}>{copy.requestSupplement}</button><button type="button" className="team-relay-primary" onClick={() => onAction('accept', copy.acceptDefault)}><CheckCircle2 size={17} />{copy.acceptDelivery}</button></>}
    </div></footer>
  </section>;
}

export function TeamRelayView({ theme, t, onThemeChange, onUseAgent, onLoadCodeAgents }) {
  const copy = t.uiTeamRelay;
  const bridge = window.TauriBridge;
  const [viewState, setViewState] = useState(() => ({ ...bridgeState('platform', {}), ...bridgeState('teamRelay', { teamRelay: {} }) }));
  const [activeView, setActiveView] = useState('attention');
  const [sourceFilter, setSourceFilter] = useState('all');
  const [viewMode, setViewMode] = useState('personal');
  const [teamFilter, setTeamFilter] = useState('all');
  const [selectedTeamId, setSelectedTeamId] = useState('');
  const [assignmentTeamId, setAssignmentTeamId] = useState('');
  const [selectedId, setSelectedId] = useState(null);
  const [query, setQuery] = useState('');
  const [dialog, setDialog] = useState(null);
  const [teamDialog, setTeamDialog] = useState(null);
  const [profileOpen, setProfileOpen] = useState(false);
  const [logoutOpen, setLogoutOpen] = useState(false);
  const [logoutAttempted, setLogoutAttempted] = useState(false);
  const [toast, setToast] = useState('');
  const [teamJoinCode, setTeamJoinCode] = useState(null);
  const [teamJoinCodeOpen, setTeamJoinCodeOpen] = useState(false);
  const [teamJoinCodeLoading, setTeamJoinCodeLoading] = useState(false);
  const [mobileDetailOpen, setMobileDetailOpen] = useState(false);
  const [workspaceLoading, setWorkspaceLoading] = useState(false);
  const [legacyReadCursors, setLegacyReadCursors] = useState(() => {
    try {
      const value = JSON.parse(window.localStorage.getItem('pinvou.team-relay.read-cursors.v1') || '{}');
      return value && typeof value === 'object' && !Array.isArray(value) ? value : {};
    } catch { return {}; }
  });
  const joinCodeRequestRef = useRef(null);
  const joinCodeRequestGenerationRef = useRef(0);
  const joinCodeFetchedAtRef = useRef(0);

  const relay = viewState.teamRelay || {};
  const webHost = window.PinvouPlatform?.kind === 'web' || window.PinvouPlatform?.isWeb === true;
  const capabilitiesLoaded = viewState.platformCapabilities?.loaded || webHost;
  const supported = !webHost && viewState.platformCapabilities?.teamRelaySupported === true;
  useEffect(() => bridge?.state?.subscribeMany ? bridge.state.subscribeMany(['platform', 'teamRelay'], setViewState) : undefined, [bridge]);
  useEffect(() => { if (supported && bridge?.teamRelay) bridge.teamRelay.load().catch(() => {}); }, [bridge, supported]);
  useEffect(() => { if (!toast) return undefined; const timer = window.setTimeout(() => setToast(''), 2600); return () => window.clearTimeout(timer); }, [toast]);

  const snapshot = relay.snapshot;
  const teams = snapshot?.teams?.length ? snapshot.teams : snapshot?.bootstrap?.team ? [{ ...snapshot.bootstrap.team, open_work_count: 0 }] : [];
  const activeTeamId = selectedTeamId || snapshot?.bootstrap?.team?.id;
  const activeBootstrap = (snapshot?.team_bootstraps || []).find(item => item.team.id === activeTeamId) || snapshot?.bootstrap;
  const activeSnapshot = snapshot && activeBootstrap ? { ...snapshot, bootstrap: activeBootstrap } : snapshot;
  const tasks = useMemo(() => projectSnapshot(snapshot, copy, legacyReadCursors), [snapshot, copy, legacyReadCursors]);
  const teamTasks = useMemo(() => tasks.filter(task => task.teamId === activeBootstrap?.team?.id), [activeBootstrap?.team?.id, tasks]);
  const board = useMemo(() => projectTeamBoard(activeSnapshot, teamTasks), [activeSnapshot, teamTasks]);
  const meId = snapshot?.bootstrap?.me?.id;
  const countedTasks = useMemo(() => teamFilter === 'all' ? tasks : tasks.filter(task => task.teamId === teamFilter), [tasks, teamFilter]);
  const counts = useMemo(() => relayViewCounts(countedTasks, meId), [countedTasks, meId]);
  const unreadTaskCount = useMemo(() => tasks.filter(task => task.category !== 'hidden' && task.unreadCount > 0).length, [tasks]);
  const visibleTasks = useMemo(() => tasks
    .filter(task => matchesRelayView(task, activeView, meId)
      && (teamFilter === 'all' || task.teamId === teamFilter)
      && (activeView !== 'all' || matchesSourceFilter(task, sourceFilter, meId))
      && task.title.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()))
    .sort((left, right) => activeView === 'attention'
      ? Number(right.category === 'receive') - Number(left.category === 'receive')
        || Number(right.pinned) - Number(left.pinned)
        || new Date(right.updated) - new Date(left.updated)
      : Number(right.pinned) - Number(left.pinned) || new Date(right.updated) - new Date(left.updated)), [activeView, meId, query, sourceFilter, tasks, teamFilter]);
  const selectedTask = visibleTasks.find(task => task.id === selectedId) || visibleTasks[0] || null;
  const dialogTeamId = dialog === 'assign' ? (assignmentTeamId || snapshot?.bootstrap?.team?.id) : (selectedTask?.teamId || snapshot?.bootstrap?.team?.id);
  const dialogBootstrap = (snapshot?.team_bootstraps || [snapshot?.bootstrap]).find(item => item?.team?.id === dialogTeamId) || snapshot?.bootstrap;
  const members = (dialogBootstrap?.members || []).filter(member => member.id !== meId);
  const taskWorkspace = selectedTask ? relay.taskWorkspaces?.[selectedTask.id] || null : null;

  useEffect(() => { if (!selectedId && visibleTasks.length) setSelectedId(visibleTasks[0].id); }, [selectedId, visibleTasks]);
  useEffect(() => { if (!assignmentTeamId && teams.length) setAssignmentTeamId(snapshot?.bootstrap?.team?.id || teams[0].id); }, [assignmentTeamId, snapshot?.bootstrap?.team?.id, teams]);
  useEffect(() => { if (visibleTasks.length && !visibleTasks.some(task => task.id === selectedId)) setSelectedId(visibleTasks[0].id); }, [selectedId, visibleTasks]);
  useEffect(() => {
    if (!selectedTask?.id || !bridge?.teamRelay?.taskWorkspaceStatus) return undefined;
    let active = true;
    setWorkspaceLoading(true);
    bridge.teamRelay.taskWorkspaceStatus(selectedTask.id, selectedTask.teamId)
      .catch(() => null)
      .finally(() => { if (active) setWorkspaceLoading(false); });
    return () => { active = false; };
  }, [bridge, selectedTask?.id, selectedTask?.teamId]);

  const updateProfile = async payload => {
    try {
      await bridge.teamRelay.updateProfile({
        display_name: payload.display_name,
        ...(payload.email ? { email: payload.email } : {}),
      });
      setProfileOpen(false);
      setToast(copy.profile.updated);
    } catch { /* state */ }
  };
  const openLogout = () => {
    setProfileOpen(false);
    setLogoutAttempted(false);
    setLogoutOpen(true);
  };
  const disconnectAccount = async () => {
    setLogoutAttempted(true);
    try {
      await bridge.teamRelay.disconnect();
      setLogoutOpen(false);
      setProfileOpen(false);
      setDialog(null);
      setTeamDialog(null);
      setSelectedId(null);
      setTeamJoinCode(null);
      setTeamJoinCodeOpen(false);
      setMobileDetailOpen(false);
    } catch { /* Logout dialog keeps the localized retryable error visible. */ }
  };

  if (capabilitiesLoaded && !supported) return <div className={`team-relay-root theme-${theme === 'dark' ? 'dark' : 'light'}`}><CenterCard title={copy.unsupportedTitle} body={copy.unsupportedBody} /></div>;
  if (!capabilitiesLoaded || relay.loading && !relay.status?.configured) return <div className={`team-relay-root theme-${theme === 'dark' ? 'dark' : 'light'}`}><CenterCard title={copy.title} body={copy.syncing} /></div>;
  if (!relay.status?.configured) return <div className={`team-relay-root theme-${theme === 'dark' ? 'dark' : 'light'}`}><AccountLogin copy={copy} busy={relay.loading} error={relay.error} onSubmit={input => bridge.teamRelay.registerAccount(input).catch(() => {})} /></div>;
  if (!snapshot) return <div className={`team-relay-root theme-${theme === 'dark' ? 'dark' : 'light'}`}><NoTeam copy={copy} theme={theme} busy={relay.mutating} error={relay.error} principal={relay.account?.principal} onCreate={name => bridge.teamRelay.createTeam(name)} onJoin={code => bridge.teamRelay.joinTeam(code)} onLogout={openLogout} />{logoutOpen && <LogoutDialog copy={copy} theme={theme} busy={relay.mutating} error={logoutAttempted ? relay.error : null} onClose={() => setLogoutOpen(false)} onConfirm={disconnectAccount} />}{toast && <div className="team-relay-toast" role="status"><CheckCircle2 size={18} />{toast}</div>}</div>;

  const runAction = async (kind, message, attachmentIds = [], recipientId = null, delivery = null) => {
    if (!selectedTask) return false;
    try {
      const artifactRole = kind === 'request_changes' ? 'input' : kind === 'final_deliver' || kind === 'accept' ? 'final' : kind === 'note' || kind === 'cancel' ? 'evidence' : 'intermediate';
      await bridge.teamRelay.createAction(selectedTask.id, { kind, recipient_id: recipientId, message, attachment_ids: attachmentIds, delivery: delivery || { schema_version: 1, artifact_role: artifactRole, expected_next: '', open_questions: '', git_references: [] }, expected_revision: selectedTask.revision });
      const toastKey = { accept: 'accepted', note: 'progressUpdated', return: 'returned', handoff: 'handedOff', final_deliver: 'delivered', request_changes: 'supplementRequested', cancel: 'cancelled' }[kind];
      setDialog(null); setToast(toastKey ? copy.toasts[toastKey] : '');
      return true;
    } catch {
      /* Bridge state renders the authoritative error. */
      return false;
    }
  };
  const submitDialog = async payload => {
    try {
      if (dialog === 'assign') await bridge.teamRelay.createThread({ team_id: payload.team_id, title: payload.title, assignee_id: payload.receiver, message: payload.message, attachment_ids: payload.attachment_ids, delivery: payload.delivery });
      else if (dialog === 'progress' && !await runAction('note', payload.message, payload.attachment_ids, null, { ...payload.delivery, subtype: 'progress' })) return;
      else if (dialog === 'handoff') await runAction('handoff', payload.message, payload.attachment_ids, payload.receiver, payload.delivery);
      else if (dialog === 'return') await runAction('return', payload.message, payload.attachment_ids, null, payload.delivery);
      else if (dialog === 'deliver') await runAction('final_deliver', payload.message, payload.attachment_ids, null, payload.delivery);
      else if (dialog === 'supplement') await runAction('request_changes', payload.message, payload.attachment_ids, null, payload.delivery);
      else if (dialog === 'cancelWork' && !await runAction('cancel', payload.message)) return;
      else if (dialog === 'agent') {
        setWorkspaceLoading(true);
        const workspace = await bridge.teamRelay.prepareTaskWorkspace(selectedTask.id);
        const selectedIds = new Set(payload.material_ids || []);
        const selectedMaterials = (workspace.materials || []).filter(material => selectedIds.has(material.attachment_id));
        const existingLink = payload.use_new_session ? null : (workspace.agent_session || null);
        const previousSequence = Number(existingLink?.last_synced_sequence || 0);
        const contextEvents = existingLink
          ? selectedTask.events.filter(event => Number(event.sequence) > previousSequence)
          : selectedTask.events;
        const shouldSupplementContext = !existingLink || payload.context_changed;
        const link = await onUseAgent({
          taskId: selectedTask.id,
          title: selectedTask.title,
          sessionTitle: copy.agentSessionTitle(selectedTask.title),
          prompt: shouldSupplementContext ? copy.buildAgentPrompt(selectedTask, { ...workspace, materials: selectedMaterials }, { incremental: !!existingLink, events: contextEvents }) : '',
          initialPrompt: copy.buildAgentPrompt(selectedTask, workspace, { incremental: false, events: selectedTask.events }),
          contextUpdate: shouldSupplementContext,
          materials: selectedMaterials,
          allMaterials: workspace.materials || [],
          materialIds: payload.material_ids || [],
          allMaterialIds: (workspace.materials || []).map(material => material.attachment_id),
          latestSequence: payload.latest_sequence || 0,
          existingLink,
          mode: payload.mode,
          agentId: payload.agent_id || null,
          codingWorkspace: payload.workspace_path || null,
        });
        if (link) await bridge.teamRelay.setTaskAgentSession(selectedTask.id, link, selectedTask.teamId);
        setDialog(null);
        return;
      }
      setDialog(null);
      if (dialog === 'assign') setToast(copy.toasts.assigned);
    } catch {
      if (dialog === 'agent') setToast(copy.agentOpenFailed);
      /* Team Relay mutations expose their localized retry point through Bridge state. */
    } finally {
      if (dialog === 'agent') setWorkspaceLoading(false);
    }
  };
  const owner = activeBootstrap?.members.find(member => member.id === meId)?.role === 'owner';
  const openTeam = teamId => {
    if (!teamId) return;
    setSelectedTeamId(teamId);
    joinCodeRequestGenerationRef.current += 1; joinCodeRequestRef.current = null; joinCodeFetchedAtRef.current = 0;
    setSelectedId(null); setTeamJoinCode(null); setTeamJoinCodeOpen(false); setTeamJoinCodeLoading(false); setMobileDetailOpen(false);
    setViewMode('board');
    if (teamId !== snapshot.bootstrap.team.id) {
      bridge.teamRelay.switchTeam(teamId).catch(() => {});
    }
  };
  const submitTeamDialog = async payload => {
    try {
      const nextSnapshot = teamDialog === 'create'
        ? await bridge.teamRelay.createTeam(payload.name)
        : await bridge.teamRelay.joinTeam(payload.join_code);
      setSelectedTeamId(nextSnapshot.bootstrap.team.id);
      joinCodeRequestGenerationRef.current += 1; joinCodeRequestRef.current = null; joinCodeFetchedAtRef.current = 0;
      setTeamDialog(null); setSelectedId(null); setTeamJoinCode(null); setTeamJoinCodeOpen(false); setTeamJoinCodeLoading(false); setMobileDetailOpen(false);
      setViewMode('board');
      setToast(teamDialog === 'create' ? copy.teams.created : copy.teams.joined);
    } catch { /* state */ }
  };
  const showTeamJoinCode = async () => {
    setTeamJoinCodeOpen(true);
    if (Date.now() - joinCodeFetchedAtRef.current < 2000 || joinCodeRequestRef.current) return;
    const generation = joinCodeRequestGenerationRef.current;
    const request = bridge.teamRelay.getJoinPassword();
    joinCodeRequestRef.current = request;
    setTeamJoinCodeLoading(true);
    try {
      const state = await request;
      if (generation === joinCodeRequestGenerationRef.current) {
        setTeamJoinCode(state);
        joinCodeFetchedAtRef.current = Date.now();
      }
    } catch {
      if (generation === joinCodeRequestGenerationRef.current) setTeamJoinCodeOpen(false);
    } finally {
      if (joinCodeRequestRef.current === request) {
        joinCodeRequestRef.current = null;
        setTeamJoinCodeLoading(false);
      }
    }
  };
  const toggleTeamJoinCode = () => {
    if (teamJoinCodeOpen) {
      setTeamJoinCodeOpen(false);
      return;
    }
    showTeamJoinCode();
  };
  const rotateTeamJoinCode = async () => {
    if (teamJoinCode?.configured && !window.confirm(copy.rotateJoinPasswordConfirm)) return;
    try { setTeamJoinCode(await bridge.teamRelay.rotateJoinPassword()); joinCodeFetchedAtRef.current = Date.now(); } catch { /* state */ }
  };
  const changeTeamJoinCodeVisibility = async () => {
    const visibility = teamJoinCode?.visibility === 'members' ? 'owners' : 'members';
    try { setTeamJoinCode(await bridge.teamRelay.setJoinPasswordVisibility(visibility)); joinCodeFetchedAtRef.current = Date.now(); } catch { /* state */ }
  };
  const disableTeamJoinCode = async () => {
    if (!window.confirm(copy.joinCode.disableConfirm)) return;
    try { setTeamJoinCode(await bridge.teamRelay.disableJoinPassword()); joinCodeFetchedAtRef.current = Date.now(); } catch { /* state */ }
  };
  const removeMember = async member => {
    if (!window.confirm(copy.board.removeConfirm(member.display_name))) return;
    try {
      await bridge.teamRelay.removeMember(member.id);
      joinCodeFetchedAtRef.current = 0; setTeamJoinCode(null); setTeamJoinCodeOpen(false);
      setToast(activeBootstrap.team.join_password_configured ? copy.board.removedAndCodeRotated(member.display_name) : copy.board.removed(member.display_name));
    } catch { /* state */ }
  };
  const locateMaterialPackage = actionId => {
    window.document.getElementById(`team-relay-package-${actionId}`)?.scrollIntoView({ behavior: 'smooth', block: 'center' });
  };
  const setPreference = async patch => {
    if (!selectedTask) return;
    const next = {
      pinned: patch.pinned === undefined ? selectedTask.pinned : patch.pinned,
      hidden: patch.hidden === undefined ? selectedTask.hidden : patch.hidden,
    };
    try {
      await bridge.teamRelay.setThreadPreference(selectedTask.id, next);
      setToast(patch.pinned === true ? copy.toasts.pinned : patch.pinned === false ? copy.toasts.unpinned : patch.hidden === true ? copy.toasts.hidden : copy.toasts.restored);
    } catch { /* Bridge state renders the authoritative error. */ }
  };

  const openBoardTask = task => {
    if (!task) return;
    setViewMode('personal');
    setTeamFilter(task.teamId);
    setSourceFilter('all');
    setActiveView(task.category === 'hidden' ? 'hidden' : task.category === 'ended' ? 'ended' : 'all');
    setSelectedId(task.id);
    setMobileDetailOpen(true);
    acknowledgeTask(task);
  };

  const openPersonalTask = task => {
    setSelectedId(task.id);
    setMobileDetailOpen(true);
    acknowledgeTask(task);
  };

  function acknowledgeTask(task) {
    if (!task || task.unreadCount <= 0) return;
    setLegacyReadCursors(current => {
      const next = { ...current, [task.id]: Math.max(Number(current[task.id] || 0), Number(task.revision || 0)) };
      try { window.localStorage.setItem('pinvou.team-relay.read-cursors.v1', JSON.stringify(next)); } catch { /* optional cache */ }
      return next;
    });
    if (bridge?.teamRelay?.markThreadRead) bridge.teamRelay.markThreadRead(task.id, task.revision).catch(() => {});
  }

  return <div className={`team-relay-root theme-${theme === 'dark' ? 'dark' : 'light'}${viewMode === 'personal' && mobileDetailOpen ? ' mobile-detail-open' : ''}`} data-testid="team-relay-page">
    <header className="team-relay-page-header">
      <div className="team-relay-brand"><PinvouLogo className="team-relay-brand-logo" /><strong>{copy.brand}</strong></div>
      <div className="team-relay-header-actions">
        <nav className="team-relay-view-switch" aria-label={copy.views.label}><button type="button" className={viewMode === 'personal' ? 'is-active' : ''} onClick={() => setViewMode('personal')}><span>{copy.views.personal}</span>{!!unreadTaskCount && <b className="team-relay-unread-badge" aria-label={copy.unread.tasks(unreadTaskCount)}>{unreadTaskCount > 99 ? '99+' : unreadTaskCount}</b>}</button><button type="button" className={viewMode !== 'personal' ? 'is-active' : ''} onClick={() => { setViewMode('teams'); setMobileDetailOpen(false); }}>{copy.views.teams}</button></nav>
        <button type="button" className="team-relay-secondary team-relay-sync-button" disabled={relay.loading} onClick={() => bridge.teamRelay.refresh().catch(() => {})} aria-label={copy.syncNow} title={copy.syncNow}><RotateCcw size={15} /><span>{relay.loading ? copy.syncingNow : copy.syncNow}</span></button>
        {onThemeChange && <button type="button" className="team-relay-theme-button" onClick={() => onThemeChange(theme === 'dark' ? 'light' : 'dark')} aria-label={copy.toggleTheme}>{theme === 'dark' ? <Sun size={17} /> : <Moon size={17} />}</button>}
        <button type="button" className="team-relay-profile-button" onClick={() => setProfileOpen(true)} aria-label={copy.profile.open}><RelayAvatar name={snapshot.bootstrap.me.display_name} seed={snapshot.bootstrap.me.id} /></button>
        <button type="button" className="team-relay-primary" onClick={() => { setAssignmentTeamId(viewMode === 'board' ? activeBootstrap.team.id : assignmentTeamId || snapshot.bootstrap.team.id); setDialog('assign'); }} data-testid="team-relay-assign"><Plus size={18} />{copy.assign}</button>
      </div>
    </header>
    {relay.error && <div className="team-relay-error-banner" role="alert"><span>{relayError(copy, relay.error)}</span><button type="button" onClick={() => bridge.teamRelay.refresh().catch(() => {})}>{copy.retry}</button></div>}
    {viewMode === 'teams' ? <TeamDirectory copy={copy} teams={teams} busy={relay.mutating} onJoin={() => setTeamDialog('join')} onCreate={() => setTeamDialog('create')} onOpen={openTeam} /> : viewMode === 'board' ? <TeamBoard copy={copy} teamName={activeBootstrap.team.name} board={board} tasks={teamTasks} canManage={owner} busy={relay.mutating || teamJoinCodeLoading || relay.status?.team_id !== activeBootstrap.team.id} joinCodeOpen={teamJoinCodeOpen} joinCodeBanner={teamJoinCodeOpen && teamJoinCode ? <TeamJoinCodeBanner copy={copy} state={teamJoinCode} owner={owner} busy={relay.mutating} onCopy={() => { navigator.clipboard.writeText(teamJoinCode.join_password); setToast(copy.copiedJoinPassword); }} onRotate={rotateTeamJoinCode} onVisibility={changeTeamJoinCodeVisibility} onDisable={disableTeamJoinCode} onClose={() => setTeamJoinCodeOpen(false)} /> : null} onBack={() => setViewMode('teams')} onInvite={toggleTeamJoinCode} onAssign={() => { setAssignmentTeamId(activeBootstrap.team.id); setDialog('assign'); }} onOpenTask={openBoardTask} onRemove={removeMember} /> : <div className="team-relay-layout"><nav className="team-relay-categories" aria-label={copy.inbox} data-testid="team-relay-categories"><strong>{copy.inbox}</strong><div className="team-relay-team-filter"><small>{copy.teams.filterLabel}</small><RelaySelect value={teamFilter} onChange={value => { setTeamFilter(value); setSelectedId(null); }} ariaLabel={copy.teams.filterLabel} options={[{ value: 'all', label: copy.teams.allTeams }, ...teams.map(team => ({ value: team.id, label: team.name }))]} /></div>{TEAM_RELAY_VIEWS.map(view => <button type="button" key={view} className={activeView === view ? 'is-active' : ''} onClick={() => { setActiveView(view); setSelectedId(null); setMobileDetailOpen(false); }} aria-current={activeView === view ? 'page' : undefined}><RelayCategoryIcon category={view} /><span>{copy.categories[view]}</span>{!!counts[view] && <b>{counts[view]}</b>}</button>)}</nav>
      <section className="team-relay-list" data-testid="team-relay-list"><header className={activeView === 'all' ? 'has-source-filters' : ''}><label><Search size={18} /><input value={query} onChange={event => setQuery(event.target.value)} placeholder={`${copy.search}${copy.title}…`} aria-label={copy.search} /></label>{activeView === 'all' && <div className="team-relay-source-filters" role="group" aria-label={copy.sourceFilterLabel}>{TEAM_RELAY_SOURCE_FILTERS.map(filter => <button type="button" key={filter} className={sourceFilter === filter ? 'is-active' : ''} aria-pressed={sourceFilter === filter} onClick={() => { setSourceFilter(filter); setSelectedId(null); setMobileDetailOpen(false); }}>{copy.sourceFilters[filter]}</button>)}</div>}</header><div className="team-relay-list-scroll">{visibleTasks.map(task => <WorkCard copy={copy} task={task} selected={task.id === selectedTask?.id} key={task.id} onClick={() => openPersonalTask(task)} />)}{!visibleTasks.length && <div className="team-relay-empty"><Search size={26} /><p>{copy.empty}</p></div>}</div></section>
      <div className="team-relay-workspace">
        {selectedTask ? <WorkThread copy={copy} task={selectedTask} meId={meId} taskWorkspace={taskWorkspace} onBack={() => setMobileDetailOpen(false)} onDialog={setDialog} onAction={runAction} onPreference={setPreference} /> : <section className="team-relay-detail team-relay-empty"><p>{copy.selectWork}</p></section>}
        {selectedTask && <MaterialInspector copy={copy} task={selectedTask} taskWorkspace={taskWorkspace} workspaceLoading={workspaceLoading || relay.preparingWorkspace === selectedTask.id} onPrepare={() => bridge.teamRelay.prepareTaskWorkspace(selectedTask.id).catch(() => {})} onDownload={file => bridge.teamRelay.downloadAttachment(file).catch(() => {})} onOpenPreparedMaterial={path => bridge.teamRelay.openPreparedMaterial(path).catch(() => {})} onRevealPreparedMaterial={attachmentId => bridge.teamRelay.revealPreparedMaterial(selectedTask.id, attachmentId, selectedTask.teamId).catch(() => {})} onLocatePackage={locateMaterialPackage} />}
      </div>
    </div>}
    {dialog && <RelayDialog copy={copy} theme={theme} type={dialog} task={selectedTask || { title: '', files: [], events: [] }} teams={teams} teamId={dialogTeamId} onTeamChange={setAssignmentTeamId} members={members} busy={relay.mutating} uploadProgress={relay.uploadProgress} taskWorkspace={taskWorkspace} workspaceLoading={workspaceLoading || relay.preparingWorkspace === selectedTask?.id} onClose={() => setDialog(null)} onSubmit={submitDialog} onLoadSourceSessions={bridge.teamRelay.listDraftSourceSessions} onReadSourceSession={bridge.teamRelay.readDraftSourceSession} onPickFiles={() => bridge.teamRelay.pickDraftAttachments()} onMountMaterials={(materials, teamId) => bridge.teamRelay.mountDraftMaterials(materials, teamId)} onPickAgentWorkspace={() => bridge.teamRelay.pickAgentWorkspace()} onLoadCodeAgents={onLoadCodeAgents} />}
    {teamDialog && <TeamDialog copy={copy} theme={theme} mode={teamDialog} busy={relay.mutating} onClose={() => setTeamDialog(null)} onSubmit={submitTeamDialog} />}
    {profileOpen && <ProfileDialog copy={copy} theme={theme} principal={snapshot.bootstrap.me} busy={relay.mutating} onClose={() => setProfileOpen(false)} onSubmit={updateProfile} onLogout={openLogout} />}
    {logoutOpen && <LogoutDialog copy={copy} theme={theme} busy={relay.mutating} error={logoutAttempted ? relay.error : null} onClose={() => setLogoutOpen(false)} onConfirm={disconnectAccount} />}
    {toast && <div className="team-relay-toast" role="status"><CheckCircle2 size={18} />{toast}</div>}
  </div>;
}
