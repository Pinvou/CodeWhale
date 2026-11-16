import { PinvouLogo } from '../../components/PinvouLogo.jsx';
import { Settings, User } from '../../components/icons.jsx';
import { modelAccessCopy } from '../../shared/i18n.js';
const FIRST_USE_ACCESS = { kind: 'first_use', canLogin: true };

export function ModelAccessGate({ onLogin, onUseExternalModels, onRetry, onDismiss, t, theme, language = 'zh', access = FIRST_USE_ACCESS, variant = 'screen', busy = false }) {
  const dark = theme === 'dark';
  const words = modelAccessCopy[language];
  const notice = variant === 'notice';
  const retry = ['credential_unavailable', 'account_error', 'read_error'].includes(access.kind);
  const login = access.canLogin && (access.kind === 'first_use' || access.kind === 'sign_in');
  if (notice) return <section data-testid="model-access-notice" role="status" className={`relative z-10 m-3 flex shrink-0 flex-wrap items-center justify-between gap-3 rounded-xl border p-4 ${dark ? 'border-[#39434C] bg-[#20262B] text-[#E5EBEF]' : 'border-[#DDE3E8] bg-[#F7FAF9] text-[#202A36]'}`}>
    <div className="min-w-0 flex-1"><h2 className="text-[14px] font-semibold">{words.titles[access.kind]}</h2><p className="mt-1 text-[13px] leading-5">{access.kind === 'select_model' && access.accountStatus === 'authenticated' ? words.signedInSelection : words.descriptions[access.kind]}</p></div>
    <div className="flex flex-wrap gap-2">
      {login && <button type="button" data-testid="model-access-login" disabled={busy} onClick={onLogin} className="rounded-lg bg-[#0F766E] px-4 py-2 text-[13px] font-semibold text-white focus-visible:outline-2 focus-visible:outline-offset-2">{t.modelAccessLogin}</button>}
      {retry && <button type="button" data-testid="model-access-retry" disabled={busy} onClick={onRetry} className="rounded-lg bg-[#0F766E] px-4 py-2 text-[13px] font-semibold text-white focus-visible:outline-2 focus-visible:outline-offset-2">{busy ? words.checking : words.retry}</button>}
      <button type="button" data-testid="model-access-external" onClick={onUseExternalModels} className="rounded-lg border border-current px-4 py-2 text-[13px] focus-visible:outline-2 focus-visible:outline-offset-2">{['select_model', 'switch_model'].includes(access.kind) ? words.select : words.settings}</button>
    </div>
  </section>;

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
        {login ? t.modelAccessDesc : words.externalDescription}
      </p>

      <div className="mt-8 flex w-full max-w-[320px] flex-col gap-3">
        {login && <button
          type="button"
          data-testid="model-access-login"
          data-modal-initial-focus="true"
          onClick={onLogin}
          className="flex h-12 w-full items-center justify-center gap-2 rounded-[14px] bg-[#0F766E] px-5 text-[15px] font-semibold text-white transition-colors hover:bg-[#0B655E] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[#14B8A6] focus-visible:ring-offset-2 dark:bg-[#2DD4BF] dark:text-[#073B37] dark:hover:bg-[#5EEAD4] dark:focus-visible:ring-offset-[#111416]"
        >
          <span aria-hidden="true"><User size={18} /></span>
          {t.modelAccessLogin}
        </button>}
        <button
          type="button"
          data-testid="model-access-external"
          data-modal-initial-focus={!login ? 'true' : undefined}
          onClick={onUseExternalModels}
          className="flex h-11 w-full items-center justify-center gap-2 rounded-[13px] border px-5 text-[14px] font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[#14B8A6] focus-visible:ring-offset-2 dark:focus-visible:ring-offset-[#111416]"
          style={{
            color: !login ? '#FFFFFF' : dark ? '#C6CDD3' : '#52606B',
            borderColor: dark ? 'rgba(255,255,255,.10)' : 'rgba(23,33,43,.12)',
            background: !login ? '#0F766E' : dark ? '#191D20' : '#F8FAFA',
          }}
        >
          <span aria-hidden="true"><Settings size={17} /></span>
          {t.modelAccessExternal}
        </button>
      </div>
      {onDismiss && <button type="button" data-testid="model-access-dismiss" onClick={onDismiss} className="mt-5 rounded-lg px-4 py-2 text-[13px] underline underline-offset-4 focus-visible:outline-2 focus-visible:outline-offset-2">{words.later}</button>}
    </section>
  );
}
