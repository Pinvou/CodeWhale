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
    }
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
    }
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
    }
  }
};
