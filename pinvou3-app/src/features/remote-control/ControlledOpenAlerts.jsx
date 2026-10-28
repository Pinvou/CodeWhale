export default function ControlledOpenAlerts({ chat, session, status, generation, intent, navigationEpoch, bridgeNavigationEpoch, sessionId, view, busy, copy, onChat, onSession }) {
  return <>
    {status === 'connected' && chat?.generation === generation && chat?.owner?.intent === intent && chat?.owner?.navigationEpoch === navigationEpoch && chat?.owner?.bridgeNavigationEpoch === bridgeNavigationEpoch
      && (view === chat.originView || view === 'chat') && <div role="alert" className="flex gap-2 px-4 py-2">
        <span>{copy.chatOpenFailed}</span>
        <button type="button" disabled={busy} onClick={onChat}>{copy.openAgain}</button>
      </div>}
    {status === 'connected' && session?.generation === generation && session?.owner?.intent === intent && session?.owner?.navigationEpoch === navigationEpoch && session?.owner?.bridgeNavigationEpoch === bridgeNavigationEpoch
      && session?.sessionId === sessionId && <div role="alert" className="flex gap-2 px-4 py-2">
        <span>{copy.sessionOpenFailed}</span>
        <button type="button" disabled={busy} onClick={onSession}>{copy.openAgain}</button>
      </div>}
  </>;
}
