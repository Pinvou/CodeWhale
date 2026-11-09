export const profileRepairQueueCopy = {
  "zh": {
    "kind": "变更类型",
    "userID": "用户 ID",
    "attempts": "已记录尝试次数",
    "lastError": "最近错误",
    "notReported": "未提供",
    "unknownKind": "未知类型",
    "unknownStatus": "未知状态",
    "unrecognizedError": "未识别的错误",
    "kinds": {
      "USERNAME": "用户名变更",
      "MOBILE": "手机号变更",
      "PASSWORD": "密码重置"
    },
    "statuses": {
      "RETRY": "等待重试",
      "REPAIR_REQUIRED": "需要处理"
    },
    "errors": {
      "IDENTITY_UPDATE_UNAVAILABLE": "身份服务暂不可用",
      "ACCOUNT_COMMIT_FAILED": "账号更新未完成",
      "IDENTITY_RESET_UNCERTAIN": "身份服务的重置结果未确认",
      "SESSION_REVOCATION_FAILED": "会话撤销未完成",
      "LEGACY_SESSION_REVOCATION_UNCERTAIN": "历史请求的会话撤销无法确认",
      "PASSWORD_SESSION_ORDER_UNPROVEN": "无法确认密码更新与会话撤销的顺序"
    },
    "details": "查看原请求",
    "requestID": "原请求 ID",
    "blocked": "此记录目前不能手动重试。可刷新以查看最新状态。",
    "confirmation": "继续此未完成请求？系统会重新安排重试，不会回退资料。原请求 ID：",
    "scheduling": "正在申请安排重试：",
    "scheduled": "服务已安排重试。请查看后续状态。原请求：",
    "uncertain": "暂时无法确认是否已安排重试。可刷新查看队列。原请求：",
    "rejected": "服务未接受此次重试。请查看错误提示。原请求："
  },
  "en": {
    "kind": "Change type",
    "userID": "User ID",
    "attempts": "Recorded attempts",
    "lastError": "Last error",
    "notReported": "Not reported",
    "unknownKind": "Unknown type",
    "unknownStatus": "Unknown status",
    "unrecognizedError": "Unrecognized error",
    "kinds": {
      "USERNAME": "Username change",
      "MOBILE": "Mobile number change",
      "PASSWORD": "Password reset"
    },
    "statuses": {
      "RETRY": "Awaiting retry",
      "REPAIR_REQUIRED": "Needs attention"
    },
    "errors": {
      "IDENTITY_UPDATE_UNAVAILABLE": "Identity service unavailable",
      "ACCOUNT_COMMIT_FAILED": "Account update did not complete",
      "IDENTITY_RESET_UNCERTAIN": "Identity service reset result is unconfirmed",
      "SESSION_REVOCATION_FAILED": "Session revocation did not complete",
      "LEGACY_SESSION_REVOCATION_UNCERTAIN": "Session revocation is unconfirmed for this older request",
      "PASSWORD_SESSION_ORDER_UNPROVEN": "The order of password update and session revocation could not be confirmed"
    },
    "details": "View original request",
    "requestID": "Original request ID",
    "blocked": "This record cannot currently be retried manually. Refresh to see the latest status.",
    "confirmation": "Continue this unfinished request? This asks the service to schedule a retry. It does not restore previous values. Original request ID:",
    "scheduling": "Requesting a retry for:",
    "scheduled": "The service scheduled a retry. Check the subsequent status. Original request:",
    "uncertain": "Could not confirm whether the service scheduled a retry. Refresh to check the queue. Original request:",
    "rejected": "The service did not accept this retry. Review the error message. Original request:"
  },
  "ja": {
    "kind": "変更の種類",
    "userID": "ユーザー ID",
    "attempts": "記録された試行回数",
    "lastError": "直近のエラー",
    "notReported": "情報なし",
    "unknownKind": "不明な種類",
    "unknownStatus": "不明な状態",
    "unrecognizedError": "未認識のエラー",
    "kinds": {
      "USERNAME": "ユーザー名の変更",
      "MOBILE": "携帯電話番号の変更",
      "PASSWORD": "パスワードの再設定"
    },
    "statuses": {
      "RETRY": "再試行待ち",
      "REPAIR_REQUIRED": "対応が必要"
    },
    "errors": {
      "IDENTITY_UPDATE_UNAVAILABLE": "ID サービスを利用できません",
      "ACCOUNT_COMMIT_FAILED": "アカウントの更新が完了しませんでした",
      "IDENTITY_RESET_UNCERTAIN": "ID サービスの再設定結果を確認できません",
      "SESSION_REVOCATION_FAILED": "セッションの無効化が完了しませんでした",
      "LEGACY_SESSION_REVOCATION_UNCERTAIN": "以前のリクエストのセッション無効化を確認できません",
      "PASSWORD_SESSION_ORDER_UNPROVEN": "パスワード更新とセッション無効化の順序を確認できません"
    },
    "details": "元のリクエストを表示",
    "requestID": "元のリクエスト ID",
    "blocked": "この記録は現在手動で再試行できません。更新して最新の状態を確認してください。",
    "confirmation": "未完了のリクエストを続行しますか？再試行を依頼します。以前の値に戻す操作ではありません。元のリクエスト ID：",
    "scheduling": "再試行を依頼中：",
    "scheduled": "再試行が予定されました。以降の状態を確認してください。元のリクエスト：",
    "uncertain": "再試行が予定されたか確認できません。キューを更新して確認してください。元のリクエスト：",
    "rejected": "サービスがこの再試行を受け付けませんでした。エラーを確認してください。元のリクエスト："
  }
};
