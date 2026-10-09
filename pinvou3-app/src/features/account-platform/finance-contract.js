const MONEY_KEY = /^(?:amount_fen|price_fen|refund_amount_fen|display_points|base_points|bonus_points|fixed_price_per_denominator|minimum_precision|price_denominator|input_per_million|output_per_million|cached_input_per_million|resolved_price|expected_etag|current_space_etag|space_etag|[a-z_]+_microcredits)$/;
const TOKEN_COMMAND_INTEGER_KEY = /^(?:lifecycle_plan_version|expected_account_version|credential_version|source_catalog_version)$/;
const RECEIVABLE_INTEGER_KEY = /^(?:expected_receivable_version|receivable_version|receivable_version_before|receivable_version_after)$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function requireFinanceID(value) {
  if (typeof value !== 'string' || !UUID.test(value)) throw new Error('INVALID_FINANCE_ACK');
  return value;
}

export function integerMoney(value) {
  if (typeof value === 'number' && !Number.isSafeInteger(value)) throw new Error('INVALID_FINANCE_AMOUNT');
  const text = String(value ?? '');
  if (!/^-?(?:0|[1-9]\d*)$/.test(text)) throw new Error('INVALID_FINANCE_AMOUNT');
  const amount = BigInt(text);
  if (amount < -(1n << 63n) || amount > (1n << 63n) - 1n) throw new Error('INVALID_FINANCE_AMOUNT');
  return amount;
}

// A displayed decimal string must regain its original JSON integer token when
// included in a typed financial proof. Never route those digits through Number.
export function encodeFinanceJSON(value) {
  const seen = new Set();
  const encode = (item, key = '') => {
    if (MONEY_KEY.test(key) || TOKEN_COMMAND_INTEGER_KEY.test(key) || RECEIVABLE_INTEGER_KEY.test(key) || key === 'expected_version') return item === null ? 'null' : integerMoney(item).toString();
    if (item === null || typeof item !== 'object') {
      if (typeof item === 'number' && !Number.isFinite(item)) throw new Error('INVALID_FINANCE_AMOUNT');
      return JSON.stringify(item);
    }
    if (seen.has(item)) throw new Error('INVALID_FINANCE_ACK');
    seen.add(item);
    const result = Array.isArray(item)
      ? `[${item.map(child => encode(child) ?? 'null').join(',')}]`
      : `{${Object.entries(item).filter(([, child]) => child !== undefined).map(([name, child]) => `${JSON.stringify(name)}:${encode(child, name)}`).join(',')}}`;
    seen.delete(item);
    return result;
  };
  return encode(value);
}

// Personal finance keeps the existing int64 wire contract. Preserve its source
// digits before JSON.parse; Admin decimal-string money is accepted unchanged.
export function parseFinanceJSON(text) {
  let result = '';
  for (let i = 0; i < text.length;) {
    if (text[i] !== '"') { result += text[i++]; continue; }
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') break;
    }
    const token = text.slice(start, i);
    result += token;
    let cursor = i;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    if (text[cursor] !== ':' || !(MONEY_KEY.test(JSON.parse(token)) || RECEIVABLE_INTEGER_KEY.test(JSON.parse(token)))) continue;
    cursor++;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    const match = text.slice(cursor).match(/^-?(?:0|[1-9]\d*)(?=\s*[,}\]])/);
    if (!match) {
      if (text[cursor] !== '"' && !/^null(?=\s*[,}\]])/.test(text.slice(cursor))) throw new Error('INVALID_FINANCE_AMOUNT');
      continue; // Quoted Admin money is checked below.
    }
    integerMoney(match[0]);
    result += text.slice(i, cursor) + JSON.stringify(match[0]);
    i = cursor + match[0].length;
  }
  const payload = JSON.parse(result);
  const validate = value => {
    if (!value || typeof value !== 'object') return;
    for (const [key, child] of Object.entries(value)) {
      if ((MONEY_KEY.test(key) || RECEIVABLE_INTEGER_KEY.test(key)) && child !== null) integerMoney(child);
      else validate(child);
    }
  };
  validate(payload);
  return payload;
}

export function requireFinanceCollection(value) {
  if (!value || !Array.isArray(value.items)) throw new Error('INVALID_FINANCE_ACK');
  return value.items;
}

export function requirePaymentOrder(value, expectedID = '') {
  requireFinanceID(value?.order_id);
  if (expectedID && value.order_id !== expectedID) throw new Error('INVALID_FINANCE_ACK');
  if (!['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'EXPIRED', 'MANUAL_REVIEW'].includes(value.status)) throw new Error('INVALID_FINANCE_ACK');
  integerMoney(value.amount_fen);
  return value;
}

export function formatFen(value, language = 'zh') {
  const amount = integerMoney(value);
  const absolute = amount < 0n ? -amount : amount;
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || 'en-US';
  const separator = (1.1).toLocaleString(locale).replace(/\d/g, '');
  return `${amount < 0n ? '-' : ''}${(absolute / 100n).toLocaleString(locale)}${separator}${String(absolute % 100n).padStart(2, '0')}`;
}

export function safePaymentURL(value) {
  if (typeof value !== 'string' || !value) return '';
  try {
    const url = new URL(value);
    if (url.username || url.password || url.hash) return '';
    if (url.protocol === 'https:') return url.href;
    if (url.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname)) return url.href;
  } catch { /* Invalid provider URLs remain unavailable. */ }
  return '';
}

export function safePaymentQR(value) {
  if (typeof value === 'string' && /^data:image\/(?:png|jpeg|webp);base64,[A-Za-z0-9+/]+={0,2}$/.test(value) && value.length <= 2 << 20) return value;
  return safePaymentURL(value);
}

export function wholeRefundTargets(detail) {
  if (!detail || !Array.isArray(detail.transactions) || !Array.isArray(detail.entitlement_packages)) return [];
  return detail.entitlement_packages.flatMap(item => {
    const transaction = detail.transactions.find(value => value.payment_transaction_id === item.source_payment_transaction_id);
    const components = item.components;
    if (!transaction || transaction.payment_order_id !== detail.order_id || !['PAID', 'LATE_PAID'].includes(transaction.status) || transaction.fulfillment_status !== 'CREDITED' || item.status !== 'ACTIVE' || !Array.isArray(components) || !components.length || components.some(value => !['BASE', 'BONUS'].includes(value.component_type))) return [];
    requireFinanceID(transaction.payment_transaction_id); requireFinanceID(item.package_id);
    return [{ transaction, package: item }];
  });
}
