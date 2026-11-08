const copies = {
  "zh": {
    "usageTitle": "模型用量",
    "usageLead": "查看当前身份的模型调用、Token 用量与积分消费。",
    "usageUnavailable": "用量记录暂时不可用，请稍后重试。",
    "noUsage": "当前身份暂无模型用量记录。",
    "unknownModel": "未知模型",
    "inputTokens": "输入",
    "outputTokens": "输出",
    "cachedTokens": "缓存",
    "usageRecord": "记录号",
    "completedAt": "完成于",
    "usagePagination": "模型用量分页",
    "previousPage": "上一页",
    "nextPage": "下一页",
    "usageStatuses": {
      "IN_PROGRESS": "处理中",
      "SETTLED": "已完成",
      "SETTLEMENT_PENDING": "结算处理中",
      "RELEASED": "未产生消费",
      "MANUAL_REVIEW": "核对中",
      "DISPLAY_STOPPED_EXECUTION_CONTINUES": "已停止显示，后台仍在执行并按实际用量结算",
      "CANCELLED_BEFORE_DISPATCH": "派发前已取消"
    },
    "points": "积分",
    "executionState": "执行",
    "settlementState": "结算",
    "executionStatuses": {
      "RECEIVED": "已接收",
      "HOLD_RESERVING": "额度预占中",
      "HOLD_RESERVED": "额度已预占",
      "DISPATCH_INTENT_PERSISTED": "投递意图已保存",
      "UPSTREAM_DISPATCHING": "正在投递上游",
      "STREAMING": "流式响应中",
      "SETTLEMENT_PENDING": "待结算",
      "SUCCEEDED": "已成功",
      "FAILED": "执行失败",
      "CANCELLED_BEFORE_DISPATCH": "投递前已取消",
      "REVIEW": "待人工核对"
    },
    "settlementStatuses": {
      "HOLD_PENDING": "额度预占待确认",
      "HOLD_ACTIVE": "额度已锁定",
      "USAGE_PENDING": "用量待结算",
      "SETTLED": "已结算",
      "RELEASED": "已释放",
      "REVIEW": "待人工核对"
    },
    "stopDelivery": "停止显示并请求取消",
    "permissionDenied": "当前账号未被授予此操作权限。",
    "refresh": "刷新账号信息",
    "sections": {
      "usage": "用量记录"
    }
  },
  "en": {
    "usageTitle": "Model usage",
    "usageLead": "Review model calls, token usage, and point consumption for the current identity.",
    "usageUnavailable": "Usage history is temporarily unavailable. Try again later.",
    "noUsage": "No model usage is recorded for the current identity.",
    "unknownModel": "Unknown model",
    "inputTokens": "Input",
    "outputTokens": "Output",
    "cachedTokens": "Cached",
    "usageRecord": "Record",
    "completedAt": "Completed",
    "usagePagination": "Model usage pages",
    "previousPage": "Previous",
    "nextPage": "Next",
    "usageStatuses": {
      "IN_PROGRESS": "Processing",
      "SETTLED": "Completed",
      "SETTLEMENT_PENDING": "Settlement pending",
      "RELEASED": "No charge",
      "MANUAL_REVIEW": "Under review",
      "DISPLAY_STOPPED_EXECUTION_CONTINUES": "Display stopped; background execution and actual-usage billing continue",
      "CANCELLED_BEFORE_DISPATCH": "Cancelled before dispatch"
    },
    "points": "points",
    "executionState": "Execution",
    "settlementState": "Settlement",
    "executionStatuses": {
      "RECEIVED": "Received",
      "HOLD_RESERVING": "Reserving hold",
      "HOLD_RESERVED": "Hold reserved",
      "DISPATCH_INTENT_PERSISTED": "Dispatch intent saved",
      "UPSTREAM_DISPATCHING": "Dispatching upstream",
      "STREAMING": "Streaming",
      "SETTLEMENT_PENDING": "Settlement pending",
      "SUCCEEDED": "Succeeded",
      "FAILED": "Failed",
      "CANCELLED_BEFORE_DISPATCH": "Cancelled before dispatch",
      "REVIEW": "Manual review"
    },
    "settlementStatuses": {
      "HOLD_PENDING": "Hold pending",
      "HOLD_ACTIVE": "Hold active",
      "USAGE_PENDING": "Usage settlement pending",
      "SETTLED": "Settled",
      "RELEASED": "Released",
      "REVIEW": "Manual review"
    },
    "stopDelivery": "Stop display and request cancellation",
    "permissionDenied": "Your account has not been granted this permission.",
    "refresh": "Refresh account information",
    "sections": {
      "usage": "Usage"
    }
  },
  "ja": {
    "usageTitle": "モデル利用履歴",
    "usageLead": "現在の ID におけるモデル呼び出し、Token 使用量、ポイント消費を確認できます。",
    "usageUnavailable": "利用履歴は一時的に利用できません。後でもう一度お試しください。",
    "noUsage": "現在の ID にはモデル利用履歴がありません。",
    "unknownModel": "不明なモデル",
    "inputTokens": "入力",
    "outputTokens": "出力",
    "cachedTokens": "キャッシュ",
    "usageRecord": "記録番号",
    "completedAt": "完了日時",
    "usagePagination": "モデル利用履歴のページ",
    "previousPage": "前のページ",
    "nextPage": "次のページ",
    "usageStatuses": {
      "IN_PROGRESS": "処理中",
      "SETTLED": "完了",
      "SETTLEMENT_PENDING": "決済処理中",
      "RELEASED": "消費なし",
      "MANUAL_REVIEW": "確認中",
      "DISPLAY_STOPPED_EXECUTION_CONTINUES": "表示停止済み。バックグラウンド実行と実使用量による決済は継続",
      "CANCELLED_BEFORE_DISPATCH": "送信前にキャンセル済み"
    },
    "points": "ポイント",
    "executionState": "実行",
    "settlementState": "決済",
    "executionStatuses": {
      "RECEIVED": "受信済み",
      "HOLD_RESERVING": "枠を確保中",
      "HOLD_RESERVED": "枠を確保済み",
      "DISPATCH_INTENT_PERSISTED": "送信予定を保存済み",
      "UPSTREAM_DISPATCHING": "上流へ送信中",
      "STREAMING": "ストリーミング中",
      "SETTLEMENT_PENDING": "決済待ち",
      "SUCCEEDED": "成功",
      "FAILED": "失敗",
      "CANCELLED_BEFORE_DISPATCH": "送信前にキャンセル",
      "REVIEW": "要確認"
    },
    "settlementStatuses": {
      "HOLD_PENDING": "枠の確保待ち",
      "HOLD_ACTIVE": "枠を確保済み",
      "USAGE_PENDING": "使用量の決済待ち",
      "SETTLED": "決済済み",
      "RELEASED": "解放済み",
      "REVIEW": "要確認"
    },
    "stopDelivery": "表示を停止してキャンセルを要求",
    "permissionDenied": "この操作に必要な権限が付与されていません。",
    "refresh": "アカウント情報を更新",
    "sections": {
      "usage": "利用履歴"
    }
  }
};
copies.zh.pageNumber = page => `第 ${page} 页`;
copies.en.pageNumber = page => `Page ${page}`;
copies.ja.pageNumber = page => `${page} ページ`;
export const modelUsageCopy = copies;
