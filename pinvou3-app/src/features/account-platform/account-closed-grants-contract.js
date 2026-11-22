import { integerMoney, parseFinanceJSON } from './finance-contract.js';

// This int64 version is evidence from the lifecycle authority, not a JS counter.
export function parseClosedGrantsJSON(text) {
  let output = '';
  for (let index = 0; index < text.length;) {
    if (text[index] !== '"') { output += text[index++]; continue; }
    const start = index++;
    while (index < text.length) {
      if (text[index] === '\\') { index += 2; continue; }
      if (text[index++] === '"') break;
    }
    const token = text.slice(start, index);
    output += token;
    let cursor = index;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    if (text[cursor] !== ':' || JSON.parse(token) !== 'subject_lifecycle_version') continue;
    cursor++;
    while (cursor < text.length && /\s/.test(text[cursor])) cursor++;
    const match = text.slice(cursor).match(/^(?:0|[1-9]\d*)(?=\s*[,}\]])/);
    if (!match) continue;
    integerMoney(match[0]);
    output += text.slice(index, cursor) + JSON.stringify(match[0]);
    index = cursor + match[0].length;
  }
  return parseFinanceJSON(output);
}
