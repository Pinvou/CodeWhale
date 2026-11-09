// Tiny recovery copy stays ready while a different backend language chunk loads.
export const CHAT_RECOVERY_COPY = {
  "en": {
    "modelRuntimeProtocolUnavailable": "This platform model has no published runtime protocol supported by this client. Ask an administrator to publish its capability, or choose another model. Signing in again will not resolve this issue.",
    "migrationIncomplete": "Migration cleanup has not been confirmed. If the OS keyring is unavailable, restore access to it, then restart Pinvou3 to retry. Reinstalling alone cannot confirm cleanup.",
    "migrationInvalid": "Pinvou3 could not read the migration report. If it is locked or unreadable, restore read access and restart. If it is corrupt, move migrations/account-platform-model-v1.json out of the Pinvou3 data directory and restart to regenerate it. Reinstalling alone cannot confirm cleanup. Configuration backups cannot restore deleted legacy credentials.",
    "steerFailedLost": "Interrupt failed (turn interrupted); the message was not delivered and could not be restored — send it again"
  },
  "ja": {
    "modelRuntimeProtocolUnavailable": "このプラットフォームモデルには対応する実行プロトコルが公開されていません。管理者にモデル機能の公開を依頼するか、別のモデルを選択してください。再ログインでは解決しません。",
    "migrationIncomplete": "移行時のクリーンアップはまだ確認できていません。OS のキーリングを利用できない場合は、アクセスを復旧してから Pinvou3 を再起動して移行を再試行してください。再インストールだけではクリーンアップを確認できません。",
    "migrationInvalid": "移行レポートを読み込めませんでした。ロックや権限の問題がある場合は、読み取りアクセスを復旧して Pinvou3 を再起動してください。破損している場合は、Pinvou3 のデータディレクトリから migrations/account-platform-model-v1.json を別の場所に移し、再起動して再生成してください。再インストールだけではクリーンアップを確認できません。設定のバックアップから削除済みの旧認証情報は復元できません。",
    "steerFailedLost": "割り込みに失敗しました（ターン中断）。メッセージは未達のまま復元できません。もう一度送信してください"
  },
  "zh": {
    "modelRuntimeProtocolUnavailable": "当前平台模型尚未发布客户端支持的运行协议。请联系管理员发布模型能力，或切换其他模型。重复登录不会修复此问题。",
    "migrationIncomplete": "尚未确认迁移清理完成。如果操作系统密钥库不可用，请恢复访问后重启 Pinvou3 以重试迁移。仅重装无法确认清理完成。",
    "migrationInvalid": "无法读取迁移报告。若文件被锁定或缺少读取权限，请恢复读取后重启 Pinvou3。若报告损坏，请把 migrations/account-platform-model-v1.json 移出 Pinvou3 数据目录，再重启以重新生成。仅重装无法确认清理完成。配置备份无法恢复已删除的旧凭据。",
    "steerFailedLost": "插队失败（回合中断），消息未送达且无法恢复；请重新发送"
  }
};
