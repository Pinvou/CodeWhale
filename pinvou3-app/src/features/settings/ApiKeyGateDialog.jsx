import { PinvouLogo } from '../../components/PinvouLogo.jsx';

// 云端模型缺少用户凭据时必须在聊天页阻断发送：空 key 会得到 401，但旧流程只表现为
// 无回应。Keychain 授权被拒时 credential_state=unavailable，后果相同，也必须阻断。
// 设置页始终保持可达，否则“去配置”仍被 gate 覆盖，用户会陷入无法录入 Key 的死锁。
export function ApiKeyGateDialog({ onOpenModelSettings, t, theme }) {
  const dark = theme === 'dark';

  return (
    <div className="fixed inset-0 z-[57] flex items-center justify-center p-6" style={{ background: 'rgba(0,0,0,.5)' }}>
      <div className="w-full max-w-[400px] rounded-2xl p-6 ts-modal-in"
           style={{ background: dark ? '#1E1F20' : '#FFFFFF', color: dark ? '#E3E3E3' : '#1F1F1F', boxShadow: '0 12px 48px rgba(0,0,0,.35)' }}>
        <div className="flex items-center gap-2 mb-3">
          <PinvouLogo className="h-[22px] w-[22px] select-none" />
          <div className="text-[17px] font-semibold">{t.apiKeyGateTitle}</div>
        </div>
        <div className="text-[14px] leading-relaxed mb-4" style={{ opacity: .85 }}>{t.apiKeyGateDesc}</div>
        <div className="flex justify-end">
          <button onClick={onOpenModelSettings}
            className="h-9 px-4 rounded-lg text-[14px] font-medium text-white" style={{ background: '#0A84FF' }}>{t.apiKeyGateBtn}</button>
        </div>
      </div>
    </div>
  );
}
