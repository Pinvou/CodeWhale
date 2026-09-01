(function () {
  "use strict";

  var shellCleanupFailed = {
    zh: "⚠️ 部分后台任务未能停止，可在后台任务列表中逐个停止。",
    en: "⚠️ Some background tasks could not be stopped. You can stop them individually from the background task list.",
    ja: "⚠️ 一部のバックグラウンドタスクを停止できませんでした。バックグラウンドタスク一覧から個別に停止できます。",
  };

  // 与 bridge 层 bt() 约定一致:settings.language 存的是 tag(zh-Hans/en/ja),
  // 非 en/ja 一律回退中文。
  function shellCleanupFailedText(language) {
    return language === "en" ? shellCleanupFailed.en
      : language === "ja" ? shellCleanupFailed.ja
      : shellCleanupFailed.zh;
  }

  // Runtime-owned user-role turns may arrive with their trailing turn metadata
  // flattened into the same text block. Keep that transport detail out of UI projections.
  function inputProvenanceFromText(value) {
    var text = String(value || "").trim();
    if (!text) return "";
    var lowerText = text.toLowerCase();
    var openingTag = "<turn_meta>";
    var closingTag = "</turn_meta>";
    var openingIndex = lowerText.lastIndexOf(openingTag);
    var closingIndex = lowerText.indexOf(closingTag, openingIndex + openingTag.length);
    var hasTrailingMetadata = openingIndex > 0 && text[openingIndex - 1] === "\n" &&
      closingIndex >= 0 && !text.slice(closingIndex + closingTag.length).trim();
    var metadata = openingIndex === 0
      ? text
      : (hasTrailingMetadata ? text.slice(openingIndex, closingIndex + closingTag.length) : "");
    if (!metadata) return "";
    var match = metadata.match(/(?:^|\n)Input provenance:\s*([a-z0-9_-]+)/i);
    return match && match[1] ? match[1].toLowerCase() : "";
  }

  function isInternalUserMessageProvenance(provenance) {
    return provenance === "runtime" || provenance === "subagent_handoff" || provenance === "shell_completion";
  }

  function consumeLeadingInternalRuntimeEnvelope(value) {
    var text = String(value || "").trim();
    var opening = text.match(/^<codewhale:runtime_event\b[^>]*\bvisibility=(["'])internal\1[^>]*>/i);
    if (!opening) return null;
    var remainder = text.slice(opening[0].length);
    var closing = remainder.match(/<\/codewhale:runtime_event\s*>/i);
    if (!closing) return null;
    return remainder.slice(closing.index + closing[0].length).trim();
  }

  function containsOnlyInternalRuntimeMetadata(value) {
    var remainder = String(value || "").trim();
    var foundEnvelope = false;
    while (remainder) {
      var afterEnvelope = consumeLeadingInternalRuntimeEnvelope(remainder);
      if (afterEnvelope === null) break;
      foundEnvelope = true;
      remainder = afterEnvelope;
    }
    if (!foundEnvelope) return false;
    if (!remainder || /^<turn_meta_unchanged\s*\/>$/i.test(remainder)) return true;
    var lowerRemainder = remainder.toLowerCase();
    return lowerRemainder.indexOf("<turn_meta>") === 0 && lowerRemainder.endsWith("</turn_meta>");
  }

  function userMessageInputProvenance(blocks) {
    var textBlocks = Array.isArray(blocks) ? blocks : [];
    for (var i = 0; i < textBlocks.length; i++) {
      var block = textBlocks[i];
      if (!block || block.type !== "text") continue;
      var provenance = inputProvenanceFromText(block.text);
      if (provenance) return provenance;
    }
    return "";
  }

  function isInternalRuntimeUserMessage(value) {
    return containsOnlyInternalRuntimeMetadata(value) ||
      isInternalUserMessageProvenance(inputProvenanceFromText(value));
  }

  window.PinvouBridgeMessages = Object.freeze({
    isInternalRuntimeUserMessage: isInternalRuntimeUserMessage,
    isInternalUserMessageProvenance: isInternalUserMessageProvenance,
    userMessageInputProvenance: userMessageInputProvenance,
    showShellCleanupFailure: function (payload, state, addSystemItem) {
      if (!payload || !payload.shell_cleanup_failed) return;
      var notice = shellCleanupFailedText(state.settings && state.settings.language);
      var existing = state.chatItems.find(function (item) {
        return item && item.turnErrorNotice && item.text === notice;
      });
      if (existing) {
        existing.legacyConversationOnly = true;
      } else {
        addSystemItem(notice, {
          turnErrorNotice: true,
          legacyConversationOnly: true,
        });
      }
    },
  });
})();
