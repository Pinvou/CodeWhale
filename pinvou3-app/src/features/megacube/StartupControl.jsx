import React from 'react';

/** The switch changes a persisted policy, never the current running state. */
export function StartupControl({ title, visualTitle, hint, enabled, disabled, onToggle, copy, testId }) {
  return <div className="cms-startup" data-testid={testId}>
    <div className="min-w-0 flex-1">
      <p className="break-words text-sm font-medium">{visualTitle || title}</p>
      <p className="cms-muted mt-1 text-xs">{copy.startupSummary}</p>
    </div>
    <button type="button" role="switch" aria-label={title} aria-checked={enabled === true}
      disabled={disabled || typeof enabled !== 'boolean'} onClick={onToggle}
      className="cms-startup-switch">
      <span className="cms-switch-track" aria-hidden="true"><span /></span>
      <span>{enabled === true ? copy.startupOn : enabled === false ? copy.startupOff : copy.startupUnknown}</span>
    </button>
    <details className="cms-details cms-startup-help"><summary>{copy.policyDetails}</summary><p className="cms-muted text-xs">{hint}</p></details>
  </div>;
}
