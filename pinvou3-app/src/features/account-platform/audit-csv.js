export function auditCSV(items) {
  const fields = ['occurred_at', 'actor_display_name', 'actor_email', 'actor_user_id', 'action', 'resource_type', 'resource_id', 'result', 'request_id', 'change_summary'];
  const cell = value => {
    let text = typeof value === 'object' && value !== null ? JSON.stringify(value) : String(value ?? '');
    // Spreadsheet readers can discard whitespace before evaluating formulas.
    // Control characters and fullwidth operators also need neutralization.
    if (/^[\s\u0000-\u001f]*[=+\-@＝＋－＠]|^[\t\r\n]/u.test(text)) text = `'${text}`;
    return `"${text.replaceAll('"', '""')}"`;
  };
  return '\uFEFF' + [fields.map(cell).join(','), ...items.map(item => fields.map(field => cell(item[field])).join(','))].join('\r\n');
}
