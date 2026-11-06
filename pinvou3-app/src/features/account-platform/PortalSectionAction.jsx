import React from 'react';

// Reserve both labels in the same grid cell so localization and busy transitions
// cannot resize a section heading or shift the card underneath it.
export function PortalSectionAction({ children, busy = false, loadingLabel, disabled = false, onClick, icon = 'refresh' }) {
  return <button type="button" className="ap-section-action" disabled={disabled || busy} aria-busy={busy || undefined} onClick={onClick}>
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {icon === 'refresh' ? <><path d="M20 7v5h-5M4 17v-5h5" /><path d="M6.1 7a7 7 0 0 1 11.6-1L20 9M4 15l2.3 3a7 7 0 0 0 11.6-1" /></> : <path d="M5 12h14m-5-5 5 5-5 5" />}
    </svg>
    <span className="ap-section-action-labels" data-label={children} data-loading-label={loadingLabel}><span role={busy ? 'status' : undefined}>{busy ? loadingLabel || children : children}</span></span>
  </button>;
}
