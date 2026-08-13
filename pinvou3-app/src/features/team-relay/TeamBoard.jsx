import React from 'react';
import { ArrowLeft, CheckCircle2, Plus, Send, Trash2 } from '../../components/icons.jsx';
import { RelayAvatar } from './RelayAvatar.jsx';

function displayTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value || '') : date.toLocaleString();
}

function MemberRow({ copy, member, duplicateName, canManage, busy, onRemove }) {
  const workload = member.activeCount + member.waitingCount + member.acceptanceCount;
  const groupLabel = member.role === 'owner' ? copy.board.groups.owners : copy.board.groups.members;
  return <article className={`team-relay-member-card${member.role === 'owner' ? ' is-owner' : ''}`} aria-label={`${groupLabel} · ${member.display_name}`}>
    <div className="team-relay-member-identity">
      <RelayAvatar name={member.display_name} seed={member.id} className={`team-relay-member-avatar${member.isMe ? ' is-me' : ''}`} />
      <span><strong>{member.display_name}{member.isMe ? ` · ${copy.board.me}` : ''}</strong><small>{duplicateName && member.email_hint ? `${member.email_hint} · ` : ''}{copy.board.roles[member.role] || member.role}</small></span>
      {member.role === 'owner' && <b>{copy.board.roles.owner}</b>}
    </div>
    <div className="team-relay-member-description">
      <span>{workload ? copy.board.memberWorkSummary(member) : copy.board.available}</span>
      <small>{member.last_seen_at ? copy.board.lastSeen(displayTime(member.last_seen_at)) : copy.board.noLastSeen}</small>
    </div>
    <div className="team-relay-member-load">
      <span><strong>{member.activeCount}</strong><small>{copy.board.load.active}</small></span>
      <span><strong>{member.waitingCount}</strong><small>{copy.board.load.waiting}</small></span>
      <span><strong>{member.acceptanceCount}</strong><small>{copy.board.load.acceptance}</small></span>
    </div>
    <div className="team-relay-member-state">
      <b className={workload ? 'is-busy' : 'is-available'}>{workload ? copy.board.busyCount(workload) : copy.board.availableShort}</b>
      <small>{copy.board.ownedCount(member.ownedCount)}</small>
      {canManage && !member.isMe && <button type="button" disabled={busy} onClick={() => onRemove(member)}><Trash2 size={13} />{copy.board.removeMember}</button>}
    </div>
  </article>;
}

function RelayQueue({ copy, tasks, onOpenTask }) {
  const rows = tasks.filter(task => !['ended', 'hidden'].includes(task.category)).slice(0, 4);
  return <section className="team-relay-board-side-card">
    <header><h3>{copy.board.queue.title}</h3><small>{copy.board.queue.count(rows.length)}</small></header>
    <div className="team-relay-board-queue">
      {rows.map(task => <button type="button" onClick={() => onOpenTask(task)} key={task.id}>
        <i className={`tone-${task.category}`} />
        <span><strong>{task.title}</strong><small>{task.initiator} → {task.current} · {task.status}</small></span>
      </button>)}
      {!rows.length && <p>{copy.board.queue.empty}</p>}
    </div>
  </section>;
}

function AcceptanceReminder({ copy, tasks, onOpenTask }) {
  const task = tasks.find(item => item.category === 'receive');
  return <section className="team-relay-board-side-card is-attention">
    <header><h3>{copy.board.attention.title}</h3></header>
    {task ? <button type="button" className="team-relay-board-attention" onClick={() => onOpenTask(task)}>
      <strong>{copy.board.attention.count(1)}</strong>
      <span>{copy.board.attention.body(task.title)}</span>
    </button> : <div className="team-relay-board-attention is-empty"><CheckCircle2 size={16} /><span>{copy.board.attention.empty}</span></div>}
  </section>;
}

export function TeamBoard({ copy, teamName, board, tasks, canManage, busy, joinCodeOpen, joinCodeBanner, onBack, onInvite, onAssign, onOpenTask, onRemove }) {
  const nameCounts = new Map();
  board.members.forEach(member => nameCounts.set(member.display_name, (nameCounts.get(member.display_name) || 0) + 1));
  const duplicateNames = new Set([...nameCounts].filter(([, count]) => count > 1).map(([name]) => name));
  const inProgressTask = tasks.find(task => !['ended', 'hidden'].includes(task.category));
  const acceptanceTask = tasks.find(task => task.category === 'receive');
  return <section className="team-relay-team" data-testid="team-relay-board">
    <div className="team-relay-board-content">
      <header className="team-relay-team-overview">
        <div><button type="button" className="team-relay-team-back" onClick={onBack}><ArrowLeft size={15} />{copy.teams.backToDirectory}</button><div className="team-relay-board-title-row"><h2>{teamName}</h2><span>{copy.liveBadge}</span></div><p>{copy.board.subtitle}</p></div>
        <div className="team-relay-team-overview-actions">
          <button type="button" className={`team-relay-secondary${joinCodeOpen ? ' is-active' : ''}`} aria-pressed={joinCodeOpen} disabled={busy} onClick={onInvite}>{copy.inviteMembers}</button>
          <button type="button" className="team-relay-primary" disabled={busy} onClick={onAssign}><Plus size={16} />{copy.assign}</button>
        </div>
      </header>
      {joinCodeOpen && joinCodeBanner}
      <div className="team-relay-board-metrics">
        <span><small>{copy.board.summary.members}</small><strong>{board.summary.members || 0}</strong></span>
        <button type="button" disabled={!inProgressTask} onClick={() => onOpenTask(inProgressTask)}><small>{copy.board.summary.inProgress}</small><strong>{board.summary.inProgress || 0}</strong></button>
        <button type="button" disabled={!acceptanceTask} onClick={() => onOpenTask(acceptanceTask)}><small>{copy.board.summary.acceptance}</small><strong>{board.summary.acceptance || 0}</strong></button>
        <span><small>{copy.board.summary.completed}</small><strong>{board.summary.completed || 0}</strong></span>
      </div>
      <div className="team-relay-board-grid">
        <section className="team-relay-member-group">
          <header><div><h3>{copy.board.loadTitle}</h3><p>{copy.board.loadSubtitle}</p></div></header>
          <div>{board.members.map(member => <MemberRow copy={copy} member={member} duplicateName={duplicateNames.has(member.display_name)} canManage={canManage} busy={busy} onRemove={onRemove} key={member.id} />)}</div>
        </section>
        <aside className="team-relay-board-side">
          <RelayQueue copy={copy} tasks={tasks} onOpenTask={onOpenTask} />
          <AcceptanceReminder copy={copy} tasks={tasks} onOpenTask={onOpenTask} />
          <div className="team-relay-board-side-note"><Send size={15} /><span>{copy.board.queue.hint}</span></div>
        </aside>
      </div>
    </div>
  </section>;
}
