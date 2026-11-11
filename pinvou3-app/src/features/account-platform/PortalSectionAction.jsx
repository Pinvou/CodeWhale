import React from 'react';

export function PortalActionIcon({ kind = 'refresh' }) {
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    {kind === 'refresh' ? <><path d="M20 7v5h-5M4 17v-5h5" /><path d="M6.1 7a7 7 0 0 1 11.6-1L20 9M4 15l2.3 3a7 7 0 0 0 11.6-1" /></>
      : kind === 'switch' ? <path d="m16 3 4 4-4 4M4 7h16M8 13l-4 4 4 4m12-4H4" />
        : kind === 'invite' ? <><path d="M4 4h16v5a3 3 0 0 0 0 6v5H4v-5a3 3 0 0 0 0-6z" /><path d="M12 7v2m0 2v2m0 2v2" /></>
          : <path d="M5 12h14m-5-5 5 5-5 5" />}
  </svg>;
}

// Reserve both labels in the same grid cell so localization and busy transitions
// cannot resize a section heading or shift the card underneath it.
export function PortalSectionAction({ children, busy = false, loadingLabel, disabled = false, onClick, icon = 'refresh' }) {
  return <button type="button" className="ap-section-action" disabled={disabled || busy} aria-busy={busy || undefined} onClick={onClick}>
    <PortalActionIcon kind={icon} />
    <span className="ap-section-action-labels" data-label={children} data-loading-label={loadingLabel}><span role={busy ? 'status' : undefined}>{busy ? loadingLabel || children : children}</span></span>
  </button>;
}
