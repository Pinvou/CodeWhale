/**
 * 内部运行时消息判定（ESM 共享版）。
 *
 * CodeWhale 把内部运行时信封（子智能体交接 subagent_handoff / 后台 shell 完成
 * shell_completion / 运行时恢复提示 runtime）以 role=user 持久化供父模型上下文
 * 使用，展示层不得渲染为用户气泡。本模块识别三种形态：
 * - 完整信封：<codewhale:runtime_event ... visibility="internal">...</codewhale:runtime_event>
 * - 非权威 provenance：以前导 <turn_meta> 开头的块内 "Input provenance: <id>"，
 *   白名单 {runtime, subagent_handoff, shell_completion}
 * - 标准上下文增量：<context_update> 包裹且带固定运行时前言的完整消息
 *
 * 本模块供 ESM 车道（CodeX 原生车道 code-native-lane.js、子智能体 transcript
 * 面板 subagent-conversation.mjs）共享。tauri/web bridge 的同源判定已收拢到
 * src/shared/bridge-messages.js（经典脚本，index.html 先于两个 bridge 加载）。
 * 两份实现保持相同的 envelope、provenance、context update 与多 block 语义；
 * 扩展白名单或内部消息形态时须同步两处。
 */

const INTERNAL_PROVENANCE = new Set(['runtime', 'subagent_handoff', 'shell_completion']);

/** 单个文本块是否为完整内部运行时信封。 */
export function isInternalRuntimeEnvelopeText(value) {
  const text = String(value || '').trim();
  return /^<codewhale:runtime_event\b[^>]*\bvisibility=(["'])internal\1[^>]*>/i.test(text)
    && /<\/codewhale:runtime_event>\s*$/i.test(text);
}

/**
 * CodeWhale's canonical prompt-cache delta is durable model context rather
 * than a user-authored turn. Deliberately require the full wrapper and fixed
 * preamble so ordinary mentions or quoted fragments remain visible.
 */
export function isContextUpdateRuntimeMessage(value) {
  const text = String(value || '').trim().replace(/\r\n/g, '\n');
  const prefix = [
    '<context_update>',
    'Session context changed since it was pinned; the pinned system prompt is unchanged. Delta (+ added, - removed):',
    '',
  ].join('\n');
  return text.startsWith(prefix) && text.endsWith('\n</context_update>');
}

function consumeLeadingInternalRuntimeEnvelope(value) {
  const text = String(value || '').trim();
  const opening = text.match(/^<codewhale:runtime_event\b[^>]*\bvisibility=(["'])internal\1[^>]*>/i);
  if (!opening) return null;
  const remainder = text.slice(opening[0].length);
  const closing = remainder.match(/<\/codewhale:runtime_event\s*>/i);
  if (!closing) return null;
  return remainder.slice(closing.index + closing[0].length).trim();
}

function containsOnlyInternalRuntimeMetadata(value) {
  let remainder = String(value || '').trim();
  let foundEnvelope = false;
  while (remainder) {
    const afterEnvelope = consumeLeadingInternalRuntimeEnvelope(remainder);
    if (afterEnvelope === null) break;
    foundEnvelope = true;
    remainder = afterEnvelope;
  }
  if (!foundEnvelope) return false;
  if (!remainder || /^<turn_meta_unchanged\s*\/>$/i.test(remainder)) return true;
  const lowerRemainder = remainder.toLowerCase();
  return lowerRemainder.startsWith('<turn_meta>') && lowerRemainder.endsWith('</turn_meta>');
}

function inputProvenanceFromText(value) {
  const text = String(value || '').trim();
  const lowerText = text.toLowerCase();
  const openingTag = '<turn_meta>';
  const closingTag = '</turn_meta>';
  const openingIndex = lowerText.lastIndexOf(openingTag);
  const closingIndex = lowerText.indexOf(closingTag, openingIndex + openingTag.length);
  const hasTrailingMetadata = openingIndex > 0 && text[openingIndex - 1] === '\n'
    && closingIndex >= 0 && !text.slice(closingIndex + closingTag.length).trim();
  const metadata = openingIndex === 0
    ? text
    : (hasTrailingMetadata ? text.slice(openingIndex, closingIndex + closingTag.length) : '');
  if (!metadata) return '';
  const match = metadata.match(/(?:^|\n)Input provenance:\s*([a-z0-9_-]+)/i);
  return match && match[1] ? match[1].toLowerCase() : '';
}

/** 单个文本块是否完全由已知内部运行时内容构成。 */
export function isInternalRuntimeUserMessage(value) {
  return isContextUpdateRuntimeMessage(value)
    || containsOnlyInternalRuntimeMetadata(value)
    || INTERNAL_PROVENANCE.has(inputProvenanceFromText(value));
}

export function isKnownInternalMetadataBlock(value) {
  if (isInternalRuntimeUserMessage(value)) return true;
  const text = String(value || '').trim().toLowerCase();
  return /^<turn_meta_unchanged\s*\/>$/.test(text)
    || (text.startsWith('<turn_meta>') && text.endsWith('</turn_meta>'));
}

/** 从消息 blocks 解析稳定的 provenance 标识符（小写）；无则返回空串。 */
export function userMessageInputProvenance(blocks) {
  for (const block of blocks || []) {
    if (!block || block.type !== 'text') continue;
    const provenance = inputProvenanceFromText(block.text);
    if (provenance) return provenance;
  }
  return '';
}

/**
 * blocks 是否只含内部运行时内容。任一普通文本块都使消息保持可见；空文本块
 * 可以伴随内部元数据，但不能单独证明消息是内部消息。
 */
export function isInternalUserMessage(blocks) {
  const textBlocks = (Array.isArray(blocks) ? blocks : [])
    .filter(block => block && block.type === 'text');
  if (INTERNAL_PROVENANCE.has(userMessageInputProvenance(textBlocks))) return true;
  let foundInternal = false;
  for (const block of textBlocks) {
    if (!String(block.text || '').trim()) continue;
    if (!isKnownInternalMetadataBlock(block.text)) return false;
    foundInternal = true;
  }
  return foundInternal;
}
