import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { flushSync } from 'react-dom';
import { ConversationTimeline } from '../../src/features/conversation/ConversationTimeline.jsx';

const host = document.getElementById('root');
const root = createRoot(host);
let updateTurns = null;
const scrollElementRef = { current: null };

function PerformanceTimeline() {
  const [visibleTurns, setVisibleTurns] = useState([]);
  updateTurns = setVisibleTurns;
  return (
    <div
      ref={scrollElementRef}
      style={{ height: 800, overflowY: 'auto', overflowAnchor: 'none' }}
    >
      <ConversationTimeline
        turns={visibleTurns}
        sessionId="performance-session"
        scrollElementRef={scrollElementRef}
      />
    </div>
  );
}

flushSync(() => root.render(<PerformanceTimeline />));

function turns(count, suffix = '') {
  return Array.from({ length: count }, (_, index) => ({
    id: `turn-${index}`,
    status: 'completed',
    userText: `用户消息 ${index}`,
    items: [{
      id: `answer-${index}`,
      type: 'agent_message',
      status: 'completed',
      text: `回答 ${index}：这是用于性能实验的 Markdown 文本，包含 **加粗**、\`code\` 和列表。${suffix}`,
    }],
  }));
}

function commit(nextTurns) {
  const startedAt = performance.now();
  flushSync(() => updateTurns(nextTurns));
  return performance.now() - startedAt;
}

function nextPaint() {
  return new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
}

function renderedTurnIndexes() {
  return [...host.querySelectorAll('[data-conversation-turn]')]
    .map(element => Number(String(element.dataset.conversationTurn || '').replace('turn-', '')))
    .filter(Number.isFinite);
}

window.__PINVOU_TIMELINE_PERFORMANCE__ = {
  async run(count) {
    flushSync(() => updateTurns([]));
    scrollElementRef.current.scrollTop = 0;
    const baseTurns = turns(count);
    const mountMs = commit(baseTurns);
    scrollElementRef.current.scrollTop = scrollElementRef.current.scrollHeight;
    await nextPaint();
    const nodes = host.querySelectorAll('*').length;
    const updatedTurns = baseTurns.slice();
    const tail = updatedTurns[updatedTurns.length - 1];
    updatedTurns[updatedTurns.length - 1] = {
      ...tail,
      items: [{ ...tail.items[0], text: `${tail.items[0].text} tail-update` }],
    };
    const tailUpdateMs = commit(updatedTurns);
    await nextPaint();
    const tailUpdated = [...host.querySelectorAll('[data-conversation-turn]')]
      .some(element => element.dataset.conversationTurn === `turn-${count - 1}` && element.textContent.includes('tail-update'));
    const bottomIndexes = renderedTurnIndexes();
    let maxResidentNodes = host.querySelectorAll('*').length;
    scrollElementRef.current.scrollTop = 0;
    await nextPaint();
    const topScrollTop = scrollElementRef.current.scrollTop;
    const topIndexes = renderedTurnIndexes();
    maxResidentNodes = Math.max(maxResidentNodes, host.querySelectorAll('*').length);
    scrollElementRef.current.scrollTop = scrollElementRef.current.scrollHeight / 2;
    await nextPaint();
    const middleScrollTop = scrollElementRef.current.scrollTop;
    const middleIndexes = renderedTurnIndexes();
    maxResidentNodes = Math.max(maxResidentNodes, host.querySelectorAll('*').length);
    return {
      count,
      mountMs,
      tailUpdateMs,
      tailUpdated,
      nodes,
      maxResidentNodes,
      virtualRows: Number(host.querySelector('[data-conversation-virtual-timeline]')?.dataset.conversationVirtualRowCount || 0),
      scrollHeight: scrollElementRef.current.scrollHeight,
      topIndexes,
      topScrollTop,
      middleIndexes,
      middleScrollTop,
      bottomIndexes,
    };
  },
};
