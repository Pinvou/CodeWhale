import React from 'react';

export function TokenAccountStatus({ value, copy }) {
  const text = Object.prototype.hasOwnProperty.call(copy.tokenAccountStatuses, value) ? copy.tokenAccountStatuses[value] : copy.tokenAccountStatusUnknown;
  return <span className="adm-status">{text}</span>;
}

export function TokenAccountCount({ value, missing, open, label }) {
  if (!Number.isSafeInteger(value) || value < 0) return missing;
  return open ? <button type="button" className="adm-token-account-metric" aria-label={label + ' · ' + value} onClick={open}>{value}</button> : value;
}
