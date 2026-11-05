import React, { useState } from 'react';
import { accountCenterCopy } from '../../shared/i18n.js';

export function CopyIdentifier({ value, language, compact = false }) {
  const copy = accountCenterCopy[language];
  const [feedback, setFeedback] = useState('');
  const identifier = String(value || '');
  return <span className="ap-copy-identifier"><code title={identifier}>{compact ? `…${identifier.slice(-8)}` : identifier}</code>
    <button type="button" className="ap-text-button" aria-label={`${copy.copyIdentifier}: ${identifier}`} onClick={async () => {
      try { await globalThis.navigator.clipboard.writeText(identifier); setFeedback(copy.copied); }
      catch { setFeedback(copy.copyManually); }
    }}>{copy.copy}</button>
    {feedback && <small role="status">{feedback}</small>}
  </span>;
}
