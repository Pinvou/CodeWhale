const MONEY_FIELDS = new Set(['available_microcredits', 'reserved_microcredits']);

// The finance facade preserves the Billing int64 JSON bytes. Quote only numeric
// wallet fields outside strings before JSON.parse can round them in a WebView.
export function parseBalanceJSON(text) {
  let result = '';
  let depth = 0;
  const seen = new Set();
  for (let i = 0; i < text.length;) {
    if (text[i] !== '"') {
      if (text[i] === '{' || text[i] === '[') depth++;
      if (text[i] === '}' || text[i] === ']') depth--;
      result += text[i++]; continue;
    }
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') break;
    }
    const token = text.slice(start, i);
    result += token;
    let cursor = i;
    while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++;
    const field = JSON.parse(token);
    if (depth !== 1 || text[cursor] !== ':' || !MONEY_FIELDS.has(field)) continue;
    if (seen.has(field)) throw new Error('INVALID_BALANCE');
    seen.add(field);
    cursor++;
    while (/\s/.test(text[cursor] || '') && cursor < text.length) cursor++;
    const match = text.slice(cursor).match(/^-?(?:0|[1-9]\d*)(?=\s*[,}])/);
    if (!match) throw new Error('INVALID_BALANCE');
    const integer = BigInt(match[0]);
    if (integer < -(1n << 63n) || integer > (1n << 63n) - 1n) throw new Error('INVALID_BALANCE');
    result += text.slice(i, cursor) + JSON.stringify(match[0]);
    i = cursor + match[0].length;
  }
  const payload = JSON.parse(result);
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) throw new Error('INVALID_BALANCE');
  for (const key of MONEY_FIELDS) {
    if (typeof payload[key] !== 'string' || !/^-?\d+$/.test(payload[key])) throw new Error('INVALID_BALANCE');
    const integer = BigInt(payload[key]);
    if (integer < -(1n << 63n) || integer > (1n << 63n) - 1n) throw new Error('INVALID_BALANCE');
  }
  return payload;
}

export function formatBalance(value, language = 'zh') {
  if (typeof value === 'number' && !Number.isSafeInteger(value)) return '—';
  if (!/^-?\d+$/.test(String(value ?? ''))) return '—';
  const amount = BigInt(value);
  const absolute = amount < 0n ? -amount : amount;
  const fraction = String(absolute % 10000n).padStart(4, '0').replace(/0+$/, '');
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || 'en-US';
  const decimal = (1.1).toLocaleString(locale).replace(/\d/g, '');
  return `${amount < 0n ? '-' : ''}${(absolute / 10000n).toLocaleString(locale)}${fraction ? decimal + fraction : ''}`;
}
