/** The local daemon's card identity contract, shared by guidance and handoff. */
export function isCmsCardId(value) {
  return typeof value === 'string' && /^[A-Za-z0-9_.-]{1,128}$/.test(value)
    && value !== '.' && value !== '..';
}
