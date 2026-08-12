import React from 'react';
import { expertAvatarSrc } from '../../shared/expert-avatar.mjs';

function initials(name) {
  return Array.from(String(name || '?')).slice(-2).join('');
}

export function RelayAvatar({ name, seed, className = '' }) {
  return <span className={`${className} team-relay-expert-avatar`} aria-hidden="true">
    <span>{initials(name)}</span>
    <img src={expertAvatarSrc(seed || name)} alt="" onError={event => event.currentTarget.remove()} />
  </span>;
}
