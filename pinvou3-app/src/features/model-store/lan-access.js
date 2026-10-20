/** Samples deliberately reference an environment variable, never a real key. */
export function lanSample(status) {
  if (!status?.endpoint || !status?.model_id) return '';
  const quote = (value) => `'${value.replaceAll("'", "'\\''")}'`;
  const body = JSON.stringify({ model: status.model_id, messages: [{ role: 'user', content: 'Hello' }], stream: true });
  return [
    `curl ${quote(status.endpoint)} \\`,
    '  -H "Authorization: Bearer $PINVOU_API_KEY" \\',
    "  -H 'Content-Type: application/json' \\",
    `  -d ${quote(body)}`,
  ].join('\n');
}

export function lanError(copy, error) {
  const code = String(error?.message || error);
  return typeof copy.lanErrors[code] === 'string' ? copy.lanErrors[code] : copy.lanErrors.lan_unavailable;
}
