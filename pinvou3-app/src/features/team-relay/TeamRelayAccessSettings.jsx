import React, { useEffect, useState } from 'react';
import { createPortal } from 'react-dom';
import { Check, Users, X } from '../../components/icons.jsx';

export function TeamRelayAccessSettings({ enabled, copy, onUnlock, onDisable }) {
  const [open, setOpen] = useState(false);
  const [accessCode, setAccessCode] = useState('');
  const [status, setStatus] = useState('idle');
  const [disableStatus, setDisableStatus] = useState('idle');

  const close = () => {
    if (status === 'submitting') return;
    setOpen(false);
    setAccessCode('');
    setStatus('idle');
  };

  useEffect(() => {
    if (!open) return undefined;
    const onKeyDown = event => {
      if (event.key === 'Escape') close();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [open, status]);

  const submit = async event => {
    event.preventDefault();
    if (!accessCode.trim() || status === 'submitting') return;
    setStatus('submitting');
    try {
      const unlocked = await onUnlock(accessCode);
      if (!unlocked) {
        setStatus('invalid');
        return;
      }
      setOpen(false);
      setAccessCode('');
      setStatus('idle');
    } catch {
      setStatus('unavailable');
    }
  };

  const disable = async () => {
    if (disableStatus === 'submitting') return;
    setDisableStatus('submitting');
    try {
      await onDisable();
      setDisableStatus('idle');
    } catch {
      setDisableStatus('unavailable');
    }
  };

  return (
    <>
      <section className="mb-6">
        <div className="px-3 mb-2 text-[12px] font-semibold text-[#8A8A8E] dark:text-[#8E8E93]">{copy.section}</div>
        <div className="overflow-hidden rounded-[18px] bg-white dark:bg-[#2C2C2E]">
          <div className="min-h-[66px] flex flex-wrap items-center gap-3 px-4 py-3 text-left text-[#1C1C1E] dark:text-[#F2F2F7]">
            <span className={`w-9 h-9 shrink-0 rounded-[12px] flex items-center justify-center ${enabled ? 'bg-[#34C759]/15 text-[#248A3D] dark:text-[#30D158]' : 'bg-[#007AFF]/10 text-[#007AFF] dark:text-[#0A84FF]'}`}>
              {enabled ? <Check size={19} /> : <Users size={19} />}
            </span>
            <div className="min-w-[160px] flex-1">
              <div className="text-[15px] leading-5 font-normal">{enabled ? copy.enabledTitle : copy.title}</div>
              <div className="mt-0.5 text-[13px] leading-5 text-[#8A8A8E] dark:text-[#98989D]">{enabled ? copy.enabledDesc : copy.desc}</div>
              {enabled && disableStatus === 'unavailable' && (
                <div aria-live="polite" className="mt-1 text-[13px] leading-5 text-[#FF3B30]">{copy.disableUnavailable}</div>
              )}
            </div>
            {enabled ? (
              <button type="button" onClick={disable} disabled={disableStatus === 'submitting'} className="h-9 px-4 rounded-full bg-[#FF3B30]/10 text-[#D70015] text-[14px] font-semibold hover:bg-[#FF3B30]/15 disabled:opacity-50 dark:text-[#FF453A]">
                {disableStatus === 'submitting' ? copy.disabling : copy.disable}
              </button>
            ) : (
              <button type="button" onClick={() => setOpen(true)} className="h-9 px-4 rounded-full bg-[#007AFF] text-white text-[14px] font-semibold hover:bg-[#006FE8]">
                {copy.open}
              </button>
            )}
          </div>
        </div>
      </section>

      {open && createPortal(
        <div className="fixed inset-0 z-[140] flex items-center justify-center bg-black/50 px-4" onClick={close}>
          <div role="dialog" aria-modal="true" aria-labelledby="team-relay-access-title" onClick={event => event.stopPropagation()} className="w-full max-w-[460px] overflow-hidden rounded-[22px] bg-white text-[#1C1C1E] shadow-2xl dark:bg-[#1C1C1E] dark:text-[#F2F2F7]">
            <div className="flex items-start justify-between gap-4 border-b border-black/[0.10] px-5 py-4 dark:border-white/[0.10]">
              <div>
                <h2 id="team-relay-access-title" className="text-[20px] leading-6 font-semibold">{copy.dialogTitle}</h2>
                <p className="mt-1 text-[13px] leading-5 text-[#8A8A8E] dark:text-[#98989D]">{copy.notice}</p>
              </div>
              <button type="button" onClick={close} aria-label={copy.close} className="h-9 w-9 shrink-0 rounded-full flex items-center justify-center bg-[#E5E5EA] text-[#636366] dark:bg-white/[0.08] dark:text-[#C7C7CC]">
                <X size={18} />
              </button>
            </div>
            <form onSubmit={submit} className="px-5 py-5">
              <label className="block">
                <span className="mb-2 block text-[13px] font-semibold text-[#636366] dark:text-[#C7C7CC]">{copy.passwordLabel}</span>
                <input
                  type="password"
                  autoComplete="off"
                  autoFocus
                  value={accessCode}
                  onChange={event => { setAccessCode(event.target.value); setStatus('idle'); }}
                  placeholder={copy.passwordPlaceholder}
                  className="w-full rounded-[14px] border border-black/[0.12] bg-[#F2F2F7] px-4 py-3 text-[15px] outline-none focus:border-[#007AFF] dark:border-white/[0.10] dark:bg-[#2C2C2E] dark:focus:border-[#0A84FF]"
                />
              </label>
              {(status === 'invalid' || status === 'unavailable') && (
                <div aria-live="polite" className="mt-2 text-[13px] leading-5 text-[#FF3B30]">{status === 'invalid' ? copy.invalid : copy.unavailable}</div>
              )}
              <div className="mt-6 flex justify-end gap-2">
                <button type="button" onClick={close} disabled={status === 'submitting'} className="h-10 px-4 rounded-full text-[14px] font-semibold text-[#007AFF] disabled:opacity-50 dark:text-[#0A84FF]">{copy.cancel}</button>
                <button type="submit" disabled={!accessCode.trim() || status === 'submitting'} className="h-10 px-5 rounded-full bg-[#007AFF] text-white text-[14px] font-semibold disabled:opacity-40">
                  {status === 'submitting' ? copy.opening : copy.confirm}
                </button>
              </div>
            </form>
          </div>
        </div>,
        document.body,
      )}
    </>
  );
}
