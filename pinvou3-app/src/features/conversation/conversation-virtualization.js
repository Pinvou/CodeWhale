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
 * @template T extends { status?: string }
 * @param {T[]} turns
 * @param {boolean} virtualized
 */
export function splitConversationLiveTail(turns, virtualized) {
  if (!virtualized || !turns.length || turns[turns.length - 1]?.status !== 'running') {
    return { historyTurns: turns, liveTurn: null, liveTurnIndex: null };
  }
  return {
    historyTurns: turns.slice(0, -1),
    liveTurn: turns[turns.length - 1],
    liveTurnIndex: turns.length - 1,
  };
}
