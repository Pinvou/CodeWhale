// Display labels never replace canonical request or policy values. Unknown
// codes remain visible, including names inherited by Object.prototype.
export function approvalLabel(labels, value) {
  return labels && Object.prototype.hasOwnProperty.call(labels, value) ? labels[value] : value;
}
