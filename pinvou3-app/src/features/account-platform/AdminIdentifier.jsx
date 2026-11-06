import React, { useId, useState } from 'react';
import { adminLayoutCopy } from '../../shared/i18n.js';
import { useIdentifierFeedback } from './useIdentifierFeedback.js';
import './admin-identifier.css';

export function AdminIdentifier({ client, owner, routeScope, value, label, language, targetLabel = '', sessionEpoch, platformAuthorizationVersion }) {
  const words = adminLayoutCopy[language].identifierActions, id = useId();
  const { status, copy, context, canCopy } = useIdentifierFeedback({ client, owner, routeScope, value, sessionEpoch, platformAuthorizationVersion });
  const [expandedFor, setExpandedFor] = useState(null);
  const expanded = expandedFor === context, available = typeof value === 'string' && value.length > 0;
  return <div className="adm-identifier">
    <div className="adm-identifier-value">
      <code id={id} className={expanded ? 'is-expanded' : ''}>{available ? value : '—'}</code>
      {available && <button type="button" className="adm-identifier-copy" disabled={!canCopy || status === 'pending'} aria-label={words.copy(targetLabel ? label + ' · ' + targetLabel : label)} aria-describedby={status ? id + '-feedback' : undefined} onClick={() => void copy()}>
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><rect x="8" y="8" width="12" height="12" rx="2" /><path d="M16 8V4H4v12h4" /></svg>
      </button>}
    </div>
    {available && <button type="button" className="adm-identifier-toggle" aria-expanded={expanded} aria-controls={id} onClick={() => setExpandedFor(expanded ? null : context)}>{expanded ? words.collapse : words.showFull}</button>}
    {status && <span id={id + '-feedback'} className="adm-identifier-feedback" role={status === 'failed' || status === 'unavailable' ? 'alert' : 'status'}>{words[status]}</span>}
  </div>;
}
