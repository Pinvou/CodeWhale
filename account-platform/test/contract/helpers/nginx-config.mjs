// Test-only parser for the directive/block subset used by these two templates.
// Comments start at a token boundary; quoted/escaped # and quoted punctuation
// are data. Unsupported/malformed syntax fails rather than supplying policy.
// Lexical reference: nginx release-1.27.5 src/core/ngx_conf_file.c:473-768.
export function nginxConfig(text) {
  let at = 0;
  const tokens = [];
  while (at < text.length) {
    if (/\s/.test(text[at])) { at++; continue; }
    if (text[at] === '#') { while (at < text.length && text[at] !== '\n') at++; continue; }
    if ('{};'.includes(text[at])) { tokens.push({ kind: text[at++] }); continue; }
    let word = ''; const quote = ['"', "'"].includes(text[at]) ? text[at++] : '';
    let closed = !quote;
    while (at < text.length) {
      const ch = text[at];
      if (ch === '\\') {
        if (++at === text.length) throw new Error('dangling nginx escape');
        const next = text[at++];
        word += ({ t: '\t', r: '\r', n: '\n', '"': '"', "'": "'", '\\': '\\' })[next] ?? `\\${next}`;
      } else if (quote && ch === quote) { at++; closed = true; break; }
      else if (!quote && (/\s/.test(ch) || '{};'.includes(ch))) break;
      else { word += ch; at++; }
    }
    if (!closed || (quote && at < text.length && !/[\s;{)]/.test(text[at]))) throw new Error('invalid nginx quote boundary');
    tokens.push({ kind: 'word', value: word });
  }
  let pos = 0;
  function level(nested) {
    const result = [];
    while (pos < tokens.length) {
      if (tokens[pos].kind === '}') { if (!nested) throw new Error('unexpected nginx block end'); pos++; return result; }
      const args = [];
      while (tokens[pos]?.kind === 'word') args.push(tokens[pos++].value);
      const end = tokens[pos++]?.kind;
      if (!args.length || ![';', '{'].includes(end)) throw new Error('unterminated nginx directive');
      result.push({ args, children: end === '{' ? level(true) : null });
    }
    if (nested) throw new Error('unterminated nginx block');
    return result;
  }
  return level(false);
}

export function nginxBlock(nodes, args) {
  const matches = [];
  function visit(items) {
    for (const item of items) {
      if (item.children && JSON.stringify(item.args) === JSON.stringify(args)) matches.push(item.children);
      if (item.children) visit(item.children);
    }
  }
  visit(nodes);
  if (matches.length !== 1) throw new Error('missing or ambiguous nginx block');
  return matches[0];
}

export const nginxSourceHeaders = nodes => nodes.filter(node => !node.children && node.args[0] === 'proxy_set_header' && node.args[1] === 'X-Pinvou-Client-Source').map(node => {
  if (node.args.length !== 3) throw new Error('invalid source header arity');
  return node.args[2];
});
