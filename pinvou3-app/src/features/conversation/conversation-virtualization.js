// Up to 80 turns, normal flow keeps short sessions simple and preserves native
// find-in-page. Above it, resident DOM grows enough in measured mixed-content
// sessions to justify virtualization. This count-based threshold is deliberately
// predictable; unusually tool-heavy shorter sessions remain a follow-up target.
export const CONVERSATION_VIRTUALIZATION_THRESHOLD = 80;

/**
 * @param {number} turnCount
 * @param {{ current: HTMLElement | null } | null | undefined} scrollElementRef
 * @param {number} threshold
 */
export function shouldVirtualizeConversationTurns(turnCount, scrollElementRef, threshold = CONVERSATION_VIRTUALIZATION_THRESHOLD) {
  return Boolean(scrollElementRef && turnCount > threshold);
}

/**
 * @template T extends { status?: string, completedAt?: number | null }
 * @param {T[]} turns
 * @param {boolean} virtualized
 * @param {boolean} busy
 */
export function splitConversationLiveTail(turns, virtualized, busy = false) {
  const tail = turns[turns.length - 1];
  const tailIsLive = tail?.status === 'running'
    || (busy && !tail?.completedAt);
  if (!virtualized || !turns.length || !tailIsLive) {
    return { historyTurns: turns, liveTurn: null, liveTurnIndex: null };
  }
  return {
    historyTurns: turns.slice(0, -1),
    liveTurn: turns[turns.length - 1],
    liveTurnIndex: turns.length - 1,
  };
}
