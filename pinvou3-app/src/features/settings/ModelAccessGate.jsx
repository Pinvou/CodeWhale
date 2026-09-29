import { PinvouLogo } from '../../components/PinvouLogo.jsx';
import { Settings, User } from '../../components/icons.jsx';

export function ModelAccessGate({ onLogin, onUseExternalModels, t, theme }) {
  const dark = theme === 'dark';

  return (
    <section
      data-testid="model-access-gate"
      aria-labelledby="model-access-title"
      className="flex w-full max-w-[440px] flex-col items-center px-5 text-center"
      style={{ color: dark ? '#F3F5F7' : '#17212B' }}
    >
      <div
        className="mb-7 flex h-[76px] w-[76px] items-center justify-center rounded-[24px] border"
        style={{
          background: dark ? '#20262B' : '#F0F8F6',
          borderColor: dark ? 'rgba(255,255,255,.08)' : 'rgba(15,118,110,.10)',
          boxShadow: dark ? '0 18px 44px rgba(0,0,0,.28)' : '0 18px 44px rgba(15,118,110,.10)',
        }}
      >
        <PinvouLogo className="h-11 w-11 select-none" />
      </div>

      <h1 id="model-access-title" className="text-[30px] font-semibold tracking-[-0.025em] sm:text-[34px]">
        {t.modelAccessTitle}
      </h1>
      <p
        className="mt-3 max-w-[400px] text-[14px] leading-6 sm:text-[15px]"
        style={{ color: dark ? '#AEB6BE' : '#66727D' }}
      >
        {t.modelAccessDesc}
      </p>

      <div className="mt-8 flex w-full max-w-[320px] flex-col gap-3">
        <button
          type="button"
          data-testid="model-access-login"
          data-modal-initial-focus="true"
          onClick={onLogin}
          className="flex h-12 w-full items-center justify-center gap-2 rounded-[14px] bg-[#0F766E] px-5 text-[15px] font-semibold text-white transition-colors hover:bg-[#0B655E] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[#14B8A6] focus-visible:ring-offset-2 dark:bg-[#2DD4BF] dark:text-[#073B37] dark:hover:bg-[#5EEAD4] dark:focus-visible:ring-offset-[#111416]"
        >
          <span aria-hidden="true"><User size={18} /></span>
          {t.modelAccessLogin}
        </button>
        <button
          type="button"
          data-testid="model-access-external"
          onClick={onUseExternalModels}
          className="flex h-11 w-full items-center justify-center gap-2 rounded-[13px] border px-5 text-[14px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[#14B8A6] focus-visible:ring-offset-2 dark:focus-visible:ring-offset-[#111416]"
          style={{
            color: dark ? '#C6CDD3' : '#52606B',
            borderColor: dark ? 'rgba(255,255,255,.10)' : 'rgba(23,33,43,.12)',
            background: dark ? '#191D20' : '#F8FAFA',
          }}
        >
          <span aria-hidden="true"><Settings size={17} /></span>
          {t.modelAccessExternal}
        </button>
      </div>
    </section>
  );
}
