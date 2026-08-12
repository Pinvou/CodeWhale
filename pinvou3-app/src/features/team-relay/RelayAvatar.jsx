import React, { useEffect, useMemo, useState } from 'react';
import { relayAvatarFallback, relayAvatarTone } from './relay-avatar.mjs';

export function RelayAvatar({ name, seed, avatar = null, loadSource = null, className = 'team-relay-avatar' }) {
  const [source, setSource] = useState(null);
  const digest = avatar?.sha256 || '';
  const tone = useMemo(() => relayAvatarTone(seed || name), [name, seed]);

  useEffect(() => {
    let active = true;
    setSource(null);
    if (!digest || typeof loadSource !== 'function') return () => { active = false; };
    Promise.resolve(loadSource(seed || name, avatar))
      .then(value => { if (active) setSource(value || null); })
      .catch(() => { if (active) setSource(null); });
    return () => { active = false; };
  }, [avatar, digest, loadSource, name, seed]);

  return <span
    className={`${className} team-relay-user-avatar`.trim()}
    style={{
      '--relay-avatar-bg': tone.background,
      '--relay-avatar-fg': tone.foreground,
      backgroundColor: tone.background,
      color: tone.foreground,
    }}
    aria-hidden="true"
  >
    <span>{relayAvatarFallback(name)}</span>
    {source && <img src={source} alt="" onError={() => setSource(null)} />}
  </span>;
}
