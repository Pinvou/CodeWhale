import React from 'react';
import { AccountPlatformView } from './AccountPlatformView.jsx';

export function AccountPlatformPopover({ open, ...props }) {
  if (!open) return null;
  return <div className="ap-popover-backdrop"><AccountPlatformView {...props} /></div>;
}
