import React from 'react';
import { X } from '../../components/icons.jsx';

export function CmsNotice({ copy, onRead, className = '', children, ...props }) {
  return <div role="alert" className={`cms-readable-notice ${className}`} {...props}>
    <div className="min-w-0 flex-1">{children}</div>
    <button type="button" className="cms-notice-close" aria-label={copy.markNoticeRead}
      title={copy.markNoticeRead} onClick={onRead} data-testid="cms-notice-read">
      <X size={14} aria-hidden="true" />
    </button>
  </div>;
}
