import React, { useId, useState } from 'react';
import { MonitorView } from '../monitor/MonitorView.jsx';
import { CmsSettingsPanel } from './CmsSettingsPanel.jsx';
import { useDeviceManagement } from './use-device-management.js';
import { deviceManagementTitle } from './device-management-state.js';

/** Runtime metrics and the existing device controls share one sidebar destination. */
export function DeviceManagementView({
  theme,
  t,
  bs,
  device,
  section,
  onSectionChange,
  onOpenModelStore,
  statusContent,
}) {
  const discovered = useDeviceManagement({ enabled: !device });
  const state = device || discovered;
  const [localSection, setLocalSection] = useState(null);
  const id = useId();
  const preferred = onSectionChange ? section : localSection;
  const active =
    state.managementAvailable && (preferred === 'management' || (!preferred && state.isCube && !statusContent))
      ? 'management'
      : 'status';
  const select = onSectionChange || setLocalSection;
  const title = deviceManagementTitle(state, t);
  const tabs = [
    ['status', statusContent ? t.deviceMgr : t.monitor],
    ['management', t.uiSettings.megacube],
  ];
  const moveTab = (event, index) => {
    const next =
      event.key === 'Home'
        ? 0
        : event.key === 'End'
          ? 1
          : event.key === 'ArrowLeft' || event.key === 'ArrowRight'
            ? 1 - index
            : null;
    if (next == null) return;
    event.preventDefault();
    select(tabs[next][0]);
    event.currentTarget.parentElement.querySelectorAll('[role="tab"]')[next]?.focus();
  };
  return (
    <div className="flex h-full min-h-0 w-full flex-col" data-testid="device-management-view">
      {state.managementAvailable && (
        <div
          role="tablist"
          aria-label={title}
          className="flex shrink-0 gap-1 border-b border-black/10 bg-white px-4 py-2 dark:border-white/10 dark:bg-[#18181b]"
        >
          {tabs.map(([key, label], index) => (
            <button
              type="button"
              role="tab"
              key={key}
              id={`${id}-${key}-tab`}
              aria-selected={active === key}
              aria-controls={`${id}-${key}-panel`}
              onClick={() => select(key)}
              tabIndex={active === key ? 0 : -1}
              onKeyDown={(event) => moveTab(event, index)}
              className={`min-h-10 rounded-lg px-4 py-2 text-sm font-medium focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-500 ${active === key ? 'bg-blue-500/15 text-blue-600 dark:text-blue-400' : 'text-black/60 dark:text-white/60'}`}
            >
              {label}
            </button>
          ))}
        </div>
      )}
      {tabs.map(([key]) => (
        <div
          key={key}
          role={state.managementAvailable ? 'tabpanel' : undefined}
          id={`${id}-${key}-panel`}
          hidden={active !== key}
          aria-labelledby={state.managementAvailable ? `${id}-${key}-tab` : undefined}
          className={active === key ? 'flex min-h-0 flex-1 flex-col' : 'hidden'}
        >
          {active === key &&
            (key === 'status' ? (
              statusContent ?? <MonitorView theme={theme} t={t} bs={bs} />
            ) : (
              <div className="h-full overflow-y-auto bg-[#f6f7f9] p-4 dark:bg-[#131314] sm:p-6 lg:p-8 custom-scrollbar">
                <div className="mx-auto max-w-6xl">
                  <h1 className="mb-2 text-2xl font-semibold tracking-tight text-[#1C1C1E] dark:text-[#F2F2F7]">
                    {title}
                  </h1>
                  <p className="mb-6 text-sm leading-relaxed text-black/60 dark:text-white/60">
                    {t.uiMegacube.overviewHint}
                  </p>
                  <CmsSettingsPanel copy={t.uiMegacube} onOpenModelStore={onOpenModelStore} />
                </div>
              </div>
            ))}
        </div>
      ))}
    </div>
  );
}
