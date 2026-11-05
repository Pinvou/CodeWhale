import { summarizeUsageRecords } from './usage-summary.js';

export const USAGE_PERIOD_BATCH_PAGES = 20;

export function usagePeriodQuery(filters) {
  const day = value => {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(value || '')) throw new Error('INVALID_USAGE_PERIOD');
    const date = new Date(`${value}T00:00:00`);
    if (!Number.isFinite(date.getTime()) || date.getFullYear() !== Number(value.slice(0, 4)) || date.getMonth() + 1 !== Number(value.slice(5, 7)) || date.getDate() !== Number(value.slice(8))) throw new Error('INVALID_USAGE_PERIOD');
    return date;
  };
  const from = day(filters.from), through = day(filters.to);
  if (from > through) throw new Error('INVALID_USAGE_PERIOD');
  through.setDate(through.getDate() + 1); // Local calendar days, including DST.
  return { from: from.getTime(), until: through.getTime(), model: filters.model || '', status: filters.status || '' };
}

// Scan existing owned history without retaining the entire account in memory.
// Each explicit batch is bounded; only exhausted cursors produce a period total.
// Do not stop by a page's date: logical-turn cursors need not use displayed time.
export async function readUsagePeriodBatch({ readPage, query, checkpoint, signal, isCurrent = () => true, maxPages = USAGE_PERIOD_BATCH_PAGES }) {
  if (!Number.isSafeInteger(maxPages) || maxPages < 1 || maxPages > USAGE_PERIOD_BATCH_PAGES) throw new Error('INVALID_USAGE_PERIOD');
  const result = { cursor: '', charged: '0', scanned: 0, unknown: 0, ...checkpoint };
  const assertCurrent = () => { if (signal?.aborted || !isCurrent()) throw Object.assign(new Error('ABORTED'), { name: 'AbortError' }); };
  const seen = new Set();
  for (let page = 0; page < maxPages && result.cursor !== null; page++) {
    assertCurrent();
    if (seen.has(result.cursor)) throw new Error('INVALID_USAGE_PAGE');
    seen.add(result.cursor);
    const value = await readPage(result.cursor, signal);
    assertCurrent();
    if (!value || !Array.isArray(value.items) || !Object.prototype.hasOwnProperty.call(value, 'next_cursor') || value.next_cursor !== null && (typeof value.next_cursor !== 'string' || !value.next_cursor || seen.has(value.next_cursor) || !value.items.length)) throw new Error('INVALID_USAGE_PAGE');
    let charged = BigInt(result.charged);
    for (const item of value.items) {
      const record = summarizeUsageRecords([item])[0], started = Date.parse(record.started_at);
      if (!Number.isFinite(started)) throw new Error('INVALID_USAGE_PAGE');
      if (started < query.from || started >= query.until || query.model && record.model !== query.model || query.status && record.display_status !== query.status) continue;
      if (record.charged_microcredits == null) result.unknown++;
      else charged += BigInt(record.charged_microcredits);
    }
    result.charged = charged.toString();
    result.scanned += value.items.length;
    result.cursor = value.next_cursor ?? null;
  }
  return result;
}
