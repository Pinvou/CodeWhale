import { useEffect } from 'react';

export function ArchiveConfirmDialog({ theme, t, onCancel, onConfirm }) {
  const isDark = theme === 'dark';

  useEffect(() => {
    const onKey = (event) => {
      if (event.key === 'Escape') onCancel();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onCancel]);

  return (
    <div
      role="presentation"
      className="fixed inset-0 z-[200] flex items-center justify-center p-4"
      style={{
        background: 'rgba(0,0,0,.34)',
        backdropFilter: 'blur(14px) saturate(140%)',
        WebkitBackdropFilter: 'blur(14px) saturate(140%)',
        fontFamily: '-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Microsoft YaHei", sans-serif',
      }}
      onClick={onCancel}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="archive-confirm-title"
        className="w-[320px] max-w-[calc(100vw-48px)] overflow-hidden rounded-[16px] shadow-2xl bg-[rgba(250,250,250,.96)] dark:bg-[rgba(44,44,46,.96)] text-[#000] dark:text-[#F2F2F7]"
        style={{ boxShadow: isDark ? '0 24px 60px rgba(0,0,0,.55)' : '0 24px 60px rgba(0,0,0,.22)' }}
        onClick={event => event.stopPropagation()}
      >
        <div className="px-6 pt-6 pb-5 text-center">
          <div id="archive-confirm-title" className="text-[20px] font-semibold leading-[26px]">{t.archiveConfirmTitle}</div>
          <div className="mt-2.5 text-[15px] leading-[22px] text-[rgba(60,60,67,.72)] dark:text-[rgba(235,235,245,.72)]">
            <div>{t.archiveConfirmMessage}</div>
            {t.archiveConfirmDetail && <div className="mt-1">{t.archiveConfirmDetail}</div>}
          </div>
        </div>
        <div className="h-px bg-[rgba(60,60,67,.24)] dark:bg-[rgba(84,84,88,.65)]" />
        <div className="grid grid-cols-2">
          <button type="button" onClick={onCancel}
            className="h-[50px] text-[17px] active:opacity-70 text-[#007AFF] dark:text-[#0A84FF]">
            {t.cpCancel}
          </button>
          <button type="button" onClick={onConfirm}
            className="h-[50px] text-[17px] font-semibold active:opacity-70 text-[#007AFF] dark:text-[#0A84FF] border-l border-l-[rgba(60,60,67,.24)] dark:border-l-[rgba(84,84,88,.65)]">
            {t.archiveConfirmAction}
          </button>
        </div>
      </div>
    </div>
  );
}

export function ArchiveToast({ t, onClose, onView }) {
  return (
    <div
      className="fixed left-1/2 top-6 z-[210] -translate-x-1/2 px-2 py-2 rounded-[18px] flex items-center gap-1 shadow-2xl bg-[rgba(250,250,250,.94)] dark:bg-[rgba(44,44,46,.94)] text-[#1C1C1E] dark:text-[#F2F2F7] border border-[rgba(0,0,0,.08)] dark:border-[rgba(255,255,255,.10)]"
      style={{
        backdropFilter: 'blur(18px) saturate(150%)',
        WebkitBackdropFilter: 'blur(18px) saturate(150%)',
        fontFamily: '-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Microsoft YaHei", sans-serif',
      }}
    >
      <div className="pl-3 pr-2 text-[14px] leading-5 whitespace-nowrap">{t.archiveSuccess}</div>
      <button type="button" onClick={onView}
        className="h-8 min-w-[76px] px-3 rounded-[12px] text-[14px] font-semibold whitespace-nowrap active:opacity-70 text-[#007AFF] dark:text-[#0A84FF] bg-[rgba(0,122,255,.10)] dark:bg-[rgba(10,132,255,.12)]">
        {t.archiveSuccessView}
      </button>
      <button type="button" onClick={onClose}
        className="w-8 h-8 rounded-full text-[18px] leading-none active:opacity-70 text-[rgba(60,60,67,.62)] dark:text-[rgba(235,235,245,.62)]"
        aria-label={t.cpCancel}>
        ×
      </button>
    </div>
  );
}
