// FFE4-1：实现移入 shared/tool-store-events.js（平台 classic 桥接层与 feature
// 模块共用同一实现，事件名是跨层契约）；此处保留 feature 侧具名导出，消费方不变。
const { notifyComposerToolsChanged, notifyChatRoundCommitted } = window.PinvouToolStoreEvents;

// biome-ignore assist/source/organizeImports: 上游 #701 重导出逐字形态，导出顺序被 web_access_contract 契约测试钉定
export { notifyComposerToolsChanged, notifyChatRoundCommitted };
