// Keeps keyboard focus inside the visible A connection summary dialog.
export function dialogTabTarget(dialog, activeElement, shiftKey) {
  if (!dialog) return null;
  const focusable = [...dialog.querySelectorAll('button:not([disabled]), [href], input:not([disabled])')];
  if (!focusable.length) return dialog;
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (!dialog.contains(activeElement)) return shiftKey ? last : first;
  if (shiftKey && activeElement === first) return last;
  if (!shiftKey && activeElement === last) return first;
  return null;
}
