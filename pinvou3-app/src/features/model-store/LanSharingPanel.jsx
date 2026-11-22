import { useEffect, useRef, useState } from 'react';
import { fetchLanStatus, setLanSharing } from './model-store-api.js';
import { lanError, lanSample } from './lan-access.js';
import { MsButton, SUBTLE_SURFACE, TEXT_PRIMARY, TEXT_SECONDARY, TEXT_DANGER } from './model-store-ui.jsx';

export function LanSharingPanel({ cardId, copy, keys, keysKnown = true, onCopy, flat = false }) {
  const [status, setStatus] = useState(null);
  const [address, setAddress] = useState('');
  const [port, setPort] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [actionError, setActionError] = useState('');
  const request = useRef(0);
  const changing = useRef(false);
  useEffect(() => {
    let disposed = false;
    let timer;
    const refresh = async () => {
      if (changing.current) { timer = setTimeout(refresh, 3000); return; }
      const token = ++request.current;
      try {
        const next = await fetchLanStatus(cardId);
        if (!disposed && token === request.current) { setStatus(next); setError(''); }
      } catch (e) {
        if (!disposed && token === request.current) setError(lanError(copy, e));
      } finally {
        if (!disposed) timer = setTimeout(refresh, 3000);
      }
    };
    refresh();
    return () => { disposed = true; request.current += 1; clearTimeout(timer); };
  }, [cardId, copy]);

  const selectedAddress = address || status?.address || '';
  const selectedPort = port ?? String(status?.port ?? 8090);
  const validAddress = status?.interfaces.some((item) => item.address === selectedAddress);
  const validPort = Number.isInteger(Number(selectedPort)) && Number(selectedPort) >= 1024 && Number(selectedPort) <= 65535;
  const toggle = async () => {
    changing.current = true;
    setActionError('');
    const token = ++request.current;
    setBusy(true);
    try {
      const next = await setLanSharing(cardId, !status?.enabled, selectedAddress || null, Number(selectedPort));
      if (token === request.current) { setStatus(next); setError(''); }
    } catch (e) {
      if (token === request.current) setActionError(lanError(copy, e));
    } finally { changing.current = false; setBusy(false); }
  };
  const activeKeys = keys.filter((key) => key.card_id === cardId).length;
  const sample = lanSample(status);
  return (
    // flat：详情页分享面板里的子卡已经提供了边框外壳和「局域网共享」标题，
    // 这时不再自带 section 外壳/标题（双层卡片叠出来就是上一版的糊成一团）。
    // 独立使用（旧入口/测试）保留原样。
    <section
      className={flat ? 'space-y-2' : `mt-4 space-y-2 rounded-[12px] p-3 ${SUBTLE_SURFACE}`}
      data-testid="model-store-lan-sharing"
    >
      {flat ? null : <h4 className={`text-[13px] font-medium ${TEXT_PRIMARY}`}>{copy.lanTitle}</h4>}
      <p className={`text-[12px] ${TEXT_SECONDARY}`}>{copy.lanHint}</p>
      <p className={`text-[12px] ${TEXT_SECONDARY}`} role="status">{error ? copy.lanUnknown : !status ? copy.lanLoading : status.enabled ? copy.lanOn : copy.lanOff}</p>
      {status?.interfaces.length === 0 ? <p className={`text-[12px] ${TEXT_SECONDARY}`}>{copy.lanNoAddress}</p> : null}
      {status && !status.enabled ? (
        <div className="flex flex-wrap gap-2">
          <label className={`text-[12px] ${TEXT_SECONDARY}`}>
            {copy.lanAddress}
            <select aria-label={copy.lanAddress} disabled={busy} value={selectedAddress} onChange={(e) => { setAddress(e.target.value); setActionError(''); }} className="ml-2 rounded px-2 py-1 bg-transparent border">
              <option value="">{copy.lanChoose}</option>
              {status.interfaces.map((item) => <option key={`${item.name}:${item.address}`} value={item.address}>{item.name} · {item.address}</option>)}
            </select>
          </label>
          <label className={`text-[12px] ${TEXT_SECONDARY}`}>{copy.lanPort}<input aria-label={copy.lanPort} disabled={busy} type="number" min="1024" max="65535" value={selectedPort} onChange={(e) => { setPort(e.target.value); setActionError(''); }} className="ml-2 w-20 rounded px-2 py-1 bg-transparent border" /></label>
        </div>
      ) : null}
      <MsButton tone="ghost" busy={busy} disabled={!status || (!status.enabled && (!validAddress || !status.available || !validPort))} onClick={toggle}>
        {status?.enabled ? copy.lanDisable : copy.lanEnable}
      </MsButton>
      {!status?.available && status?.reason ? <p className={`text-[12px] ${TEXT_SECONDARY}`}>{lanError(copy, status.reason)}</p> : null}
      {error || actionError ? <p role="alert" className={`text-[12px] ${TEXT_DANGER}`}>{error || actionError}</p> : null}
      <p className={`text-[12px] ${TEXT_SECONDARY}`}>{keysKnown ? copy.lanKeyStatus(activeKeys) : copy.lanKeyUnknown}</p>
      {status?.enabled && !error ? (
        <div className="space-y-2 text-[12px]">
          {[[copy.accessModelId, status.model_id], [copy.lanBaseUrl, status.base_url], [copy.lanEndpoint, status.endpoint]].map(([label, value]) => (
            <div key={label}><span className={TEXT_SECONDARY}>{label}: </span><code className="break-all">{value}</code> <MsButton tone="ghost" onClick={() => onCopy(value)}>{copy.copy}</MsButton></div>
          ))}
          <pre className="overflow-x-auto whitespace-pre-wrap break-all">{sample}</pre>
          <MsButton tone="ghost" onClick={() => onCopy(sample)}>{copy.lanCopySample}</MsButton>
          <p className={TEXT_SECONDARY}>{copy.lanSampleHint}</p>
        </div>
      ) : null}
      <p className={`text-[11px] ${TEXT_SECONDARY}`}>{copy.lanSupport}</p>
    </section>
  );
}
