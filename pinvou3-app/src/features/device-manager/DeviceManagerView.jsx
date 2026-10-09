// MegaCube Device Manager view (features/device-manager).
//
// One scrolling page for the MegaCube all-in-one device: header health chips,
// the embedded runtime monitor (MonitorView with embedded), sensors, storage,
// network, processes, model management and the AI doctor quick actions.
//
// Data sources:
// - bs.device / bs.deviceError come from the device bridge polling
//   (platform/tauri/bridge/device.js); this view only starts/stops the poll
//   lifecycle and reads the already-formatted `_fmt` snapshot.
// - bs.monitor._fmt is polled by the embedded MonitorView itself; the header
//   memory chip reuses it when present and omits the chip otherwise.
// - Model management mixes the model store library (model-store-api.js) with
//   the device external-model registry (bs.externalModels). Store models are
//   read-only here: install/remove stays in the Model Store, this page shows
//   the locked state and a jump link.
//
// Dependencies: features -> components/shared/platform/features. No import
// from src/app/** and no direct Tauri global object access (bridge facade only).
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  AlertTriangle,
  Cpu,
  Database,
  GitBranch,
  Globe,
  Layers,
  Link,
  Lock,
  Radio,
  RefreshCw,
  RotateCcw,
  Send,
  Server,
  Wrench,
} from '../../components/icons.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { MonitorView } from '../monitor/MonitorView.jsx';
import { fetchLibrary, storeAvailability } from '../model-store/model-store-api.js';
import { libraryStatusOf } from '../model-store/model-store-logic.js';
import { buildSavedModel, findLinkedRows } from '../model-store/pinvou-model-link.js';
import { MsButton, MsPill, MsToast } from '../model-store/model-store-ui.jsx';

const TOAST_MS = 3200;
// Disk usage thresholds: >=85% amber, >=95% red (same family as the sensor
// near-critical bands the bridge computes).
const DISK_WARN_PCT = 85;
const DISK_CRIT_PCT = 95;
// Header GPU temperature chip thresholds (degrees Celsius).
const GPU_TEMP_WARN_C = 80;
const GPU_TEMP_CRIT_C = 90;
const QUESTION_MAX = 2000;
// The counter only appears near the limit so it never becomes noise.
const COUNTER_THRESHOLD = 1800;

// Locale map for the "updated at" clock, same convention as MonitorView.
const CLOCK_LOCALE = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' };

// sensor chipKey (from bridge/device.js sensorChipKey) -> i18n key. Unknown
// chipKey falls back to the raw chip text at render time.
const CHIP_NAME_KEYS = {
  nvme: 'chipNvme',
  mt7925: 'chipWifi',
  acpi_fan: 'chipFan',
  acpitz: 'chipBoard',
  ina3221: 'chipPower',
};

// Network interface typeKey -> i18n key + icon (icons.jsx only).
const NET_TYPE_KEYS = {
  ethernet: 'netTypeEthernet',
  wifi: 'netTypeWifi',
  loopback: 'netTypeLoopback',
  bridge: 'netTypeBridge',
  virtual: 'netTypeVirtual',
  other: 'netTypeOther',
};
const NET_TYPE_ICONS = {
  ethernet: Link,
  wifi: Radio,
  loopback: RotateCcw,
  bridge: GitBranch,
  virtual: Layers,
  other: Globe,
};

// External model kind -> i18n key (openai_endpoint and vllm both speak the
// "endpoint" dialect for a noob audience; llamacpp is the local-weights one).
const KIND_LABEL_KEYS = {
  ollama: 'kindOllama',
  openai_endpoint: 'kindEndpoint',
  vllm: 'kindEndpoint',
  llamacpp: 'kindWeights',
  other: 'kindOther',
};

// AI doctor quick actions (kind values are the backend doctor.rs contract).
const DOCTOR_CARDS = [
  { kind: 'packages', icon: RefreshCw, titleKey: 'doctorPackages', descKey: 'doctorPackagesDesc' },
  { kind: 'dependencies', icon: Wrench, titleKey: 'doctorDependencies', descKey: 'doctorDependenciesDesc' },
  { kind: 'cuda', icon: Cpu, titleKey: 'doctorCuda', descKey: 'doctorCudaDesc' },
  { kind: 'services', icon: Server, titleKey: 'doctorServices', descKey: 'doctorServicesDesc' },
  { kind: 'hogs', icon: Database, titleKey: 'doctorHogs', descKey: 'doctorHogsDesc' },
];

const SECTION_IDS = ['overview', 'sensors', 'storage', 'network', 'processes', 'models', 'doctor'];
const SECTION_TITLE_KEYS = {
  overview: 'sectionOverview',
  sensors: 'sectionSensors',
  storage: 'sectionStorage',
  network: 'sectionNetwork',
  processes: 'sectionProcesses',
  models: 'sectionModels',
  doctor: 'sectionDoctor',
};

const LEVEL_DOT = {
  ok: 'bg-[#34C759] dark:bg-[#30D158]',
  warn: 'bg-[#FF9500] dark:bg-[#FF9F0A]',
  crit: 'bg-[#FF3B30] dark:bg-[#FF453A]',
};
const LEVEL_COLOR = { ok: '#34C759', warn: '#FF9500', crit: '#FF3B30' };

const CARD_CLASS =
  'rounded-[28px] bg-white/70 dark:bg-[#1C1C1E] border border-black/[0.05] dark:border-white/[0.05] shadow-[0_10px_28px_rgba(15,23,42,0.05)] dark:shadow-[0_18px_44px_rgba(0,0,0,0.4)]';
const PILL_CLASS =
  'inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-[11px] font-semibold bg-black/[0.05] dark:bg-white/[0.08] text-black/55 dark:text-white/60';
const MUTED_TEXT = 'text-black/45 dark:text-white/45';

const pctLevel = (pct, warnAt, critAt) => {
  if (typeof pct !== 'number' || !Number.isFinite(pct)) return 'ok';
  if (pct >= critAt) return 'crit';
  if (pct >= warnAt) return 'warn';
  return 'ok';
};

// Largest temperature across all sensor groups, parsed from the formatted
// value text (the bridge only exposes display strings).
const maxSensorTemperature = (sensors) => {
  let max = null;
  for (const group of Array.isArray(sensors) ? sensors : []) {
    for (const entry of Array.isArray(group?.entries) ? group.entries : []) {
      if (entry?.kind !== 'temperature') continue;
      const value = parseFloat(entry.valueText);
      if (Number.isFinite(value) && (max == null || value > max)) max = value;
    }
  }
  return max;
};

const friendlyChipName = (group, copy) => {
  const chipKey = String(group?.chipKey || '');
  if (CHIP_NAME_KEYS[chipKey]) return copy[CHIP_NAME_KEYS[chipKey]];
  const prefixKey = Object.keys(CHIP_NAME_KEYS).find((key) => chipKey.startsWith(key));
  if (prefixKey) return copy[CHIP_NAME_KEYS[prefixKey]];
  return group?.chip || chipKey;
};

const limitUnitOfKind = (kind) => (kind === 'temperature' ? '\u00B0C' : (kind === 'fan' ? ' RPM' : ''));

const HealthChip = ({ label, value, level = 'ok' }) => (
  <span
    data-testid="device-manager-health-chip"
    className="inline-flex items-center gap-2 rounded-full bg-black/[0.04] dark:bg-white/[0.06] px-3.5 py-1.5 text-[12px] font-semibold"
  >
    <span className={`h-2 w-2 rounded-full ${LEVEL_DOT[level] || LEVEL_DOT.ok}`} />
    <span className={MUTED_TEXT}>{label}</span>
    <span className="font-mono text-black/80 dark:text-white/80">{value}</span>
  </span>
);

const SectionShell = ({ id, icon: Icon, title, children }) => (
  <section id={`device-manager-${id}`} data-testid={`device-manager-${id}`} className="scroll-mt-24">
    <div className="mb-4 flex items-center gap-2.5">
      <div className="rounded-full bg-black/[0.04] p-1.5 dark:bg-white/[0.06]">
        <Icon size={16} strokeWidth={2} className="text-black/50 dark:text-white/50" />
      </div>
      <h2 className="text-[15px] font-bold tracking-tight text-black/80 dark:text-white/80">{title}</h2>
    </div>
    {children}
  </section>
);

const SensorGroupCard = ({ group, copy }) => (
  <div data-testid="device-manager-sensor-chip" className={`${CARD_CLASS} space-y-3 p-5`}>
    <div className="text-[13px] font-bold text-black/80 dark:text-white/80">{friendlyChipName(group, copy)}</div>
    {(group.entries || []).map((entry, index) => {
      const limitValue = entry.critical != null ? entry.critical : entry.max;
      const limitText = limitValue != null
        ? copy.critLimit(`${Math.round(limitValue)}${limitUnitOfKind(entry.kind)}`)
        : null;
      return (
        <div key={`${entry.name}-${index}`} className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${LEVEL_DOT[entry.level] || LEVEL_DOT.ok}`} />
              <span className="truncate text-[13px] text-black/70 dark:text-white/70">{entry.name}</span>
            </div>
            {entry.level !== 'ok' && (
              <div
                className={`mt-0.5 pl-3.5 text-[11px] leading-4 ${
                  entry.level === 'crit' ? 'text-[#FF3B30] dark:text-[#FF453A]' : 'text-[#A15C07] dark:text-[#FDD663]'
                }`}
              >
                {entry.level === 'crit' ? copy.sensorCritAdvice : copy.sensorWarnAdvice}
              </div>
            )}
          </div>
          <div className="shrink-0 text-right">
            <div className="font-mono text-[13px] font-semibold text-black/80 dark:text-white/80">{entry.valueText}</div>
            {limitText && <div className={`text-[11px] ${MUTED_TEXT}`}>{limitText}</div>}
          </div>
        </div>
      );
    })}
  </div>
);

const DiskRow = ({ disk, copy }) => {
  const level = pctLevel(disk.usedPct, DISK_WARN_PCT, DISK_CRIT_PCT);
  const known = disk.totalText !== '\u2014';
  return (
    <div data-testid="device-manager-disk-row" className={`${CARD_CLASS} p-5`}>
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="text-[13px] font-semibold text-black/80 dark:text-white/80">{disk.mountPoint}</span>
          {disk.kindLabel && <span className={PILL_CLASS}>{disk.kindLabel}</span>}
          {disk.isRoot && (
            <span
              data-testid="device-manager-disk-root-badge"
              className="inline-flex shrink-0 items-center rounded-full bg-[#E8F0FE] px-2 py-0.5 text-[11px] font-semibold text-[#0B57D0] dark:bg-[#1F3760] dark:text-[#A8C7FA]"
            >
              {copy.diskSystemBadge}
            </span>
          )}
          <span className={`text-[11px] ${MUTED_TEXT}`}>{disk.fsType}</span>
        </div>
        <span className={`shrink-0 font-mono text-[12px] ${MUTED_TEXT}`}>
          {known ? `${disk.availableText} / ${disk.totalText}` : '\u2014'}
        </span>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-black/5 dark:bg-white/10">
        <div
          className="h-full rounded-full transition-all duration-500"
          style={{ width: `${Math.max(0, Math.min(100, disk.usedPct))}%`, backgroundColor: LEVEL_COLOR[level] }}
        />
      </div>
      {level !== 'ok' && (
        <div
          className={`mt-2 text-[11px] ${
            level === 'crit' ? 'text-[#FF3B30] dark:text-[#FF453A]' : 'text-[#A15C07] dark:text-[#FDD663]'
          }`}
        >
          {level === 'crit' ? copy.diskCritAdvice : copy.diskWarnAdvice}
        </div>
      )}
    </div>
  );
};

const NetworkRow = ({ iface }) => {
  const Icon = NET_TYPE_ICONS[iface.typeKey] || Globe;
  return (
    <div data-testid="device-manager-net-row" className={`${CARD_CLASS} flex items-start gap-4 p-5`}>
      <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-black/[0.04] dark:bg-white/[0.06]">
        <Icon size={17} className="text-black/55 dark:text-white/55" />
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className={`h-2 w-2 shrink-0 rounded-full ${iface.isUp ? LEVEL_DOT.ok : 'bg-black/25 dark:bg-white/30'}`} />
          <span className="truncate text-[13px] font-semibold text-black/80 dark:text-white/80">{iface.name}</span>
          <span className={PILL_CLASS}>{iface.typeLabel}</span>
        </div>
        {iface.mac && <div className={`mt-0.5 font-mono text-[11px] ${MUTED_TEXT}`}>{iface.mac}</div>}
        <div className={`mt-2 flex flex-wrap gap-x-5 gap-y-1 font-mono text-[12px] text-black/60 dark:text-white/60`}>
          <span>{'\u2193'} {iface.rxTotalText} ({iface.rxRateText})</span>
          <span>{'\u2191'} {iface.txTotalText} ({iface.txRateText})</span>
        </div>
      </div>
    </div>
  );
};

const ProcList = ({ title, rows, metricKey, testId }) => (
  <div className={`${CARD_CLASS} p-5`} data-testid={testId}>
    <div className="mb-3 text-[12px] font-bold uppercase tracking-wider text-black/50 dark:text-white/50">{title}</div>
    {(rows || []).length === 0 ? (
      <div className={`text-[12px] ${MUTED_TEXT}`}>{'\u2014'}</div>
    ) : (
      <div className="space-y-2">
        {rows.map((proc, index) => (
          <div key={`${proc.pid}-${index}`} className="flex items-center justify-between gap-3 text-[13px]">
            <span className="min-w-0 truncate text-black/75 dark:text-white/75">
              {proc.name} <span className={`font-mono text-[11px] ${MUTED_TEXT}`}>#{proc.pid}</span>
            </span>
            <span className="shrink-0 font-mono font-semibold text-black/80 dark:text-white/80">{proc[metricKey]}</span>
          </div>
        ))}
      </div>
    )}
  </div>
);

const DeviceManagerView = ({ theme, t, bs, onOpenChat, onOpenModelStore }) => {
  const copy = t.uiDeviceMgr;
  const deviceFmt = bs?.device?._fmt || null;
  const deviceError = bs?.deviceError || null;
  const monitorFmt = bs?.monitor?._fmt || null;
  const external = bs?.externalModels || null;
  const externalModels = Array.isArray(external?.models) ? external.models : [];
  const externalWarnings = Array.isArray(external?.warnings) ? external.warnings : [];

  // The bridge suppresses notify for display-equal snapshots (including the
  // updatedAt clock field), so the "updated at" label needs its own slow tick
  // to stay fresh.
  const [nowTick, setNowTick] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNowTick(Date.now()), 10000);
    return () => clearInterval(timer);
  }, []);

  // Polling lifecycle mirrors MonitorView: start on mount (with one immediate
  // services probe + external-model fetch), stop on unmount.
  const deviceApiReady = !!((window.TauriBridge || bridge)?.device?.startDevicePolling);
  useEffect(() => {
    const liveBridge = window.TauriBridge || bridge;
    const dev = liveBridge?.device;
    if (!dev || typeof dev.startDevicePolling !== 'function') {
      console.warn('[device-manager] TauriBridge device API unavailable');
      return undefined;
    }
    dev.startDevicePolling();
    if (typeof dev.pollDeviceServices === 'function') dev.pollDeviceServices().catch(() => {});
    if (typeof dev.fetchExternalModels === 'function') dev.fetchExternalModels().catch(() => {});
    return () => {
      if (typeof dev.stopDevicePolling === 'function') dev.stopDevicePolling();
    };
  }, []);

  const [toast, setToast] = useState(null);
  const toastTimerRef = useRef(null);
  const showToast = useCallback((text) => {
    setToast({ text });
    if (toastTimerRef.current) clearTimeout(toastTimerRef.current);
    toastTimerRef.current = setTimeout(() => setToast(null), TOAST_MS);
  }, []);
  useEffect(() => () => {
    if (toastTimerRef.current) clearTimeout(toastTimerRef.current);
  }, []);

  // Read-failure retry: stop+start makes the facade poll immediately (the raw
  // pollDevice stays feature-internal), then refresh the slow services probe.
  const retryPoll = useCallback(() => {
    const dev = (window.TauriBridge || bridge)?.device;
    if (!dev) return;
    if (typeof dev.stopDevicePolling === 'function') dev.stopDevicePolling();
    if (typeof dev.startDevicePolling === 'function') dev.startDevicePolling();
    if (typeof dev.pollDeviceServices === 'function') dev.pollDeviceServices().catch(() => {});
  }, []);

  const refreshExternal = useCallback(() => {
    const dev = (window.TauriBridge || bridge)?.device;
    if (dev?.fetchExternalModels) dev.fetchExternalModels().catch(() => {});
  }, []);

  // ── Model store (read-only here) ─────────────────────────────────
  // The store is a pure Linux feature; on targets without it this page hides
  // the store group silently and still manages external models.
  const [storeState, setStoreState] = useState('loading');
  const [library, setLibrary] = useState([]);
  useEffect(() => {
    let alive = true;
    storeAvailability()
      .then(async (status) => {
        if (!alive) return null;
        if (!status?.available) {
          setStoreState('unavailable');
          return null;
        }
        setStoreState('available');
        return fetchLibrary();
      })
      .then((lib) => {
        if (alive && lib) setLibrary(Array.isArray(lib.models) ? lib.models : []);
      })
      .catch(() => {
        if (alive) setStoreState('unavailable');
      });
    return () => {
      alive = false;
    };
  }, []);
  const storeRows = storeState === 'available' ? library : [];

  // ── External models: link into Pinvou / guarded removal ──────────
  const [busyKey, setBusyKey] = useState(null);
  const [armedRemoveId, setArmedRemoveId] = useState(null);
  const [deleteWeights, setDeleteWeights] = useState(false);

  const isRowLinked = (row) => findLinkedRows(bs?.savedModels, row.model, row.name).length > 0;

  // Same shape as ModelDetailDialog.useInPinvou: build a SavedModel via the
  // shared pinvou-model-link helpers, save it, and make it the active model
  // (only affects new chats).
  const linkExternal = useCallback(async (row) => {
    const liveBridge = window.TauriBridge || bridge;
    if (!liveBridge?.models?.saveModel) {
      showToast(t.uiModelStore.linkBridgeMissing);
      return;
    }
    setBusyKey(`link:${row.id}`);
    try {
      const saved = buildSavedModel({
        modelName: row.model,
        displayName: row.name,
        baseUrl: row.base_url,
      });
      await liveBridge.models.saveModel(saved);
      if (liveBridge.models.setActiveModel) await liveBridge.models.setActiveModel(saved.id);
      showToast(copy.linkDone);
    } catch (error) {
      showToast(String(error?.message || error));
    } finally {
      setBusyKey(null);
    }
  }, [copy, showToast, t]);

  const armRemove = useCallback((row) => {
    setDeleteWeights(false);
    setArmedRemoveId(row.id || row.name);
  }, []);
  const cancelRemove = useCallback(() => setArmedRemoveId(null), []);

  // Guarded removal through the backend command. Ollama rows that were listed
  // straight from the local service (not yet in the registry) have no usable
  // id: register them quietly first, then remove with the returned record id
  // so the same guard chain (store boundary, weights sandbox, confirm name)
  // applies to them too.
  const confirmRemove = useCallback(async (row) => {
    const dev = (window.TauriBridge || bridge)?.device;
    if (!dev?.removeExternalModel) return;
    setBusyKey(`remove:${row.id}`);
    try {
      let id = row.id;
      if (row.source === 'ollama' && !row.registered) {
        const record = await dev.registerExternalModel({
          name: row.name,
          kind: 'ollama',
          model: row.model || row.name,
        });
        if (!record?.id) throw new Error('missing id');
        id = record.id;
      }
      const outcome = await dev.removeExternalModel({
        id,
        deleteWeights,
        confirmName: row.name,
      });
      setArmedRemoveId(null);
      setDeleteWeights(false);
      showToast(copy.removeDone(row.name));
      const warnings = Array.isArray(outcome?.warnings) ? outcome.warnings : [];
      for (const warning of warnings) showToast(copy.warn(warning));
    } catch (error) {
      // Backend guard failures carry a human-readable message already.
      showToast(copy.removeFailed(String(error?.message || error)));
    } finally {
      setBusyKey(null);
    }
  }, [copy, deleteWeights, showToast]);

  // ── AI doctor actions ────────────────────────────────────────────
  const [doctorBusy, setDoctorBusy] = useState(null);
  const [question, setQuestion] = useState('');
  const runDoctor = useCallback(async (kind, questionText = null, onDone = null) => {
    const dev = (window.TauriBridge || bridge)?.device;
    if (!dev?.buildDoctorPrompt || typeof onOpenChat !== 'function') return;
    setDoctorBusy(kind);
    try {
      const prompt = await dev.buildDoctorPrompt(kind, questionText);
      await onOpenChat(prompt);
      if (onDone) onDone();
    } catch (error) {
      showToast(String(error?.message || error));
    } finally {
      setDoctorBusy(null);
    }
  }, [onOpenChat, showToast]);
  const questionInvalid = question.trim().length === 0 || question.length > QUESTION_MAX;
  const sendCustom = useCallback(() => {
    const text = question.trim();
    if (!text || text.length > QUESTION_MAX) return;
    runDoctor('custom', text, () => setQuestion(''));
  }, [question, runDoctor]);

  // ── Header data ──────────────────────────────────────────────────
  const deviceName = deviceFmt?.osProductName || 'MegaCube';
  const subtitleParts = [deviceFmt?.osPrettyName, deviceFmt?.kernel, deviceFmt?.arch].filter(Boolean);
  const rootDisk = (deviceFmt?.disks || []).find((disk) => disk.isRoot) || null;
  const ramPct = typeof monitorFmt?.ramPct === 'number' ? monitorFmt.ramPct : null;
  const gpuTemp = maxSensorTemperature(deviceFmt?.sensors);
  const locale = CLOCK_LOCALE[t.langTag] || 'zh-CN';
  const updatedAtText = deviceFmt
    ? new Date(deviceFmt.updatedAt || nowTick).toLocaleTimeString(locale, { hour12: false })
    : null;

  const gotoSection = useCallback((id) => {
    document.getElementById(`device-manager-${id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, []);
  const sectionList = SECTION_IDS.map((id) => ({ id, label: copy[SECTION_TITLE_KEYS[id]] }));

  const storeStatusLabel = (record) => {
    const statusKey = { running: 'statusRunning', deployed: 'statusDeployed', pending: 'statusPending' }[libraryStatusOf(record)];
    return copy[statusKey];
  };

  return (
    <div data-testid="device-manager-view" className="flex-1 w-full h-full overflow-y-auto custom-scrollbar">
      <div className="min-h-full bg-white dark:bg-[#131314] text-[#1F1F1F] dark:text-[#E3E3E3] p-4 sm:p-6 lg:p-10 font-sans selection:bg-blue-500/30 relative transition-colors duration-500">
        <div className="max-w-[1400px] mx-auto relative z-10 space-y-8">

          {/* ── Header: device name, os line, health chips ─────────── */}
          <header>
            <h1 className="text-[26px] font-normal tracking-tight text-black/90 dark:text-white/90">{deviceName}</h1>
            <div className={`mt-1 text-[13px] ${MUTED_TEXT}`}>
              {subtitleParts.length > 0 ? subtitleParts.join(' \u00B7 ') : copy.loading}
            </div>
            <div className="mt-3 flex flex-wrap items-center gap-2">
              {rootDisk && (
                <HealthChip
                  label={copy.chipDisk}
                  value={`${rootDisk.usedPct}%`}
                  level={pctLevel(rootDisk.usedPct, DISK_WARN_PCT, DISK_CRIT_PCT)}
                />
              )}
              {ramPct != null && (
                <HealthChip
                  label={copy.chipMemory}
                  value={`${Math.round(ramPct)}%`}
                  level={pctLevel(ramPct, DISK_WARN_PCT, DISK_CRIT_PCT)}
                />
              )}
              {gpuTemp != null && (
                <HealthChip
                  label={copy.chipGpuTemp}
                  value={`${gpuTemp}\u00B0C`}
                  level={pctLevel(gpuTemp, GPU_TEMP_WARN_C, GPU_TEMP_CRIT_C)}
                />
              )}
              {deviceFmt?.gpuExt?.driverVersion && (
                <HealthChip label={copy.chipDriver} value={deviceFmt.gpuExt.driverVersion} />
              )}
              {deviceFmt?.gpuExt?.cudaVersion && (
                <HealthChip label={copy.chipCuda} value={deviceFmt.gpuExt.cudaVersion} />
              )}
              {updatedAtText && (
                <span className={`text-[11px] ${MUTED_TEXT}`}>{copy.updatedAtLabel(updatedAtText)}</span>
              )}
            </div>
          </header>

          {/* ── Sticky section nav ─────────────────────────────────── */}
          <nav className="sticky top-0 z-20 flex flex-wrap items-center gap-1 rounded-full bg-white/80 px-2 py-1.5 ring-1 ring-black/[0.04] backdrop-blur-xl dark:bg-[#131314]/80 dark:ring-white/[0.06]">
            {sectionList.map((section) => (
              <button
                key={section.id}
                type="button"
                onClick={() => gotoSection(section.id)}
                className="rounded-full px-3 py-1 text-[12px] font-semibold text-black/55 transition-colors hover:bg-black/5 dark:text-white/55 dark:hover:bg-white/10"
              >
                {section.label}
              </button>
            ))}
          </nav>

          {/* ── Read-failure banner + retry ────────────────────────── */}
          {(deviceError || !deviceApiReady) && (
            <div
              data-testid="device-manager-error"
              className="flex flex-wrap items-center justify-between gap-2 rounded-[14px] bg-rose-50 px-3 py-2 dark:bg-rose-500/10"
            >
              <span className="flex min-w-0 items-center gap-2 text-[12px] text-rose-700 dark:text-rose-300">
                <AlertTriangle size={14} className="shrink-0 opacity-80" />
                <span className="min-w-0 break-words">{deviceApiReady ? copy.errorBanner : copy.bridgeNotReady}</span>
              </span>
              {deviceApiReady && (
                <MsButton tone="ghost" onClick={retryPoll} data-testid="device-manager-retry">{copy.retry}</MsButton>
              )}
            </div>
          )}

          {/* ── Overview: the full runtime monitor, header-less ────── */}
          <SectionShell id="overview" icon={Globe} title={copy.sectionOverview}>
            <div className={CARD_CLASS}>
              <MonitorView theme={theme} t={t} bs={bs} embedded />
            </div>
          </SectionShell>

          {/* ── Sensors ────────────────────────────────────────────── */}
          <SectionShell id="sensors" icon={Cpu} title={copy.sectionSensors}>
            {(deviceFmt?.sensors || []).length === 0 ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.sensorEmpty}</div>
            ) : (
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                {deviceFmt.sensors.map((group, index) => (
                  <SensorGroupCard key={`${group.chip}-${index}`} group={group} copy={copy} />
                ))}
              </div>
            )}
          </SectionShell>

          {/* ── Storage ────────────────────────────────────────────── */}
          <SectionShell id="storage" icon={Database} title={copy.sectionStorage}>
            {(deviceFmt?.disks || []).length === 0 ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.storageEmpty}</div>
            ) : (
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
                {deviceFmt.disks.map((disk) => (
                  <DiskRow key={disk.mountPoint} disk={disk} copy={copy} />
                ))}
              </div>
            )}
          </SectionShell>

          {/* ── Network ────────────────────────────────────────────── */}
          <SectionShell id="network" icon={Radio} title={copy.sectionNetwork}>
            {(deviceFmt?.network || []).length === 0 ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.networkEmpty}</div>
            ) : (
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
                {deviceFmt.network.map((iface) => (
                  <NetworkRow
                    key={iface.name}
                    iface={{ ...iface, typeLabel: copy[NET_TYPE_KEYS[iface.typeKey] || 'netTypeOther'] }}
                  />
                ))}
              </div>
            )}
          </SectionShell>

          {/* ── Processes ──────────────────────────────────────────── */}
          <SectionShell id="processes" icon={Server} title={copy.sectionProcesses}>
            <div className="space-y-4">
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                <ProcList
                  testId="device-manager-proc-cpu"
                  title={copy.procCpuTitle}
                  rows={deviceFmt?.processes?.topCpu}
                  metricKey="cpuPctText"
                />
                <ProcList
                  testId="device-manager-proc-mem"
                  title={copy.procMemTitle}
                  rows={deviceFmt?.processes?.topMemory}
                  metricKey="memText"
                />
              </div>
              {(deviceFmt?.gpuExt?.computeProcesses || []).length > 0 && (
                <ProcList
                  testId="device-manager-proc-gpu"
                  title={copy.procGpuTitle}
                  rows={deviceFmt.gpuExt.computeProcesses.map((proc) => ({
                    pid: proc.pid,
                    name: proc.name,
                    usedText: proc.usedText,
                  }))}
                  metricKey="usedText"
                />
              )}
              <p className={`text-[11px] ${MUTED_TEXT}`}>{copy.procNote}</p>
            </div>
          </SectionShell>

          {/* ── Model management ───────────────────────────────────── */}
          <SectionShell id="models" icon={Wrench} title={copy.sectionModels}>
            <div className={`${CARD_CLASS} space-y-4 p-5`}>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <h3 className="text-[13px] font-bold text-black/75 dark:text-white/75">{copy.modelsTitle}</h3>
                <div className="flex flex-wrap items-center gap-2">
                  <MsButton
                    tone="neutral"
                    onClick={refreshExternal}
                    data-testid="device-manager-models-refresh"
                  >
                    <RefreshCw size={14} className="opacity-70" />
                    {copy.refresh}
                  </MsButton>
                  <MsButton
                    tone="primary"
                    busy={doctorBusy === 'install_model'}
                    disabled={!!doctorBusy && doctorBusy !== 'install_model'}
                    onClick={() => runDoctor('install_model')}
                    data-testid="device-manager-install-model"
                  >
                    {copy.install}
                  </MsButton>
                </div>
              </div>

              {externalWarnings.length > 0 && (
                <div className={`space-y-0.5 text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-model-warnings">
                  {externalWarnings.map((warning, index) => (
                    <div key={index}>{copy.warn(warning)}</div>
                  ))}
                </div>
              )}

              {/* Store models: locked management + jump to the store. */}
              {storeRows.length > 0 && (
                <div className="space-y-2" data-testid="device-manager-store-rows">
                  <div className={`text-[12px] font-bold uppercase tracking-wider ${MUTED_TEXT}`}>{copy.storeSubtitle}</div>
                  {storeRows.map((record) => (
                    <div
                      key={record.card_id}
                      data-testid="device-manager-store-row"
                      className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-2xl bg-white/60 px-4 py-3 dark:bg-white/[0.04]"
                    >
                      <span className="min-w-0 truncate text-[13px] font-semibold text-black/80 dark:text-white/80">
                        {record.card_name}
                      </span>
                      <MsPill tone={libraryStatusOf(record) === 'running' ? 'ok' : 'neutral'}>
                        {storeStatusLabel(record)}
                      </MsPill>
                      {record.running && record.endpoint_url && (
                        <span className={`max-w-[260px] truncate font-mono text-[11px] ${MUTED_TEXT}`}>
                          {record.endpoint_url}
                        </span>
                      )}
                      <span className="ml-auto flex shrink-0 items-center gap-2">
                        <button
                          type="button"
                          disabled
                          data-testid="device-manager-store-managed"
                          title={copy.storeManaged}
                          className="inline-flex h-9 shrink-0 cursor-not-allowed items-center gap-1.5 rounded-full bg-black/[0.04] px-4 text-[13px] font-semibold opacity-50 dark:bg-white/[0.06]"
                        >
                          <Lock size={13} />
                          {copy.storeManaged}
                        </button>
                        <MsButton tone="ghost" onClick={onOpenModelStore} data-testid="device-manager-goto-store">
                          {copy.gotoModelStore}
                        </MsButton>
                      </span>
                    </div>
                  ))}
                </div>
              )}

              {/* External models: registry + Ollama listing. */}
              {externalModels.length > 0 && (
                <div className="space-y-2" data-testid="device-manager-external-rows">
                  <div className={`text-[12px] font-bold uppercase tracking-wider ${MUTED_TEXT}`}>{copy.externalSubtitle}</div>
                  {externalModels.map((row) => {
                    const linked = isRowLinked(row);
                    const linkable = !!(row.base_url && row.model);
                    const armed = armedRemoveId === (row.id || row.name);
                    const kindLabel = copy[KIND_LABEL_KEYS[row.kind] || 'kindOther'];
                    const rowBusy = busyKey === `link:${row.id}` || busyKey === `remove:${row.id}`;
                    return (
                      <div
                        key={row.id || row.name}
                        data-testid="device-manager-external-row"
                        className="rounded-2xl bg-white/60 px-4 py-3 dark:bg-white/[0.04]"
                      >
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="min-w-0 truncate text-[13px] font-semibold text-black/80 dark:text-white/80">
                            {row.name}
                          </span>
                          <MsPill tone="neutral">{kindLabel}</MsPill>
                          {row.registered && <MsPill tone="accent">{copy.registeredBadge}</MsPill>}
                          <MsPill tone={row.status === 'available' ? 'ok' : 'warn'}>
                            {row.status === 'available' ? copy.statusAvailable : copy.statusUnreachable}
                          </MsPill>
                        </div>
                        {row.base_url && (
                          <div className={`mt-1 truncate font-mono text-[11px] ${MUTED_TEXT}`}>
                            {row.base_url}{row.model ? ` \u00B7 ${row.model}` : ''}
                          </div>
                        )}
                        {armed && (
                          <p className={`mt-2 text-[12px] leading-5 ${MUTED_TEXT}`}>{copy.removeConfirmIntro(row.name)}</p>
                        )}
                        <div className="mt-2 flex flex-wrap items-center gap-2">
                          {linked ? (
                            <span
                              data-testid="device-manager-linked-badge"
                              className="inline-flex items-center rounded-full bg-[#E6F4EA] px-2.5 py-1 text-[11px] font-semibold text-[#137333] dark:bg-[#1E3A2A] dark:text-[#81C995]"
                            >
                              {copy.linked}
                            </span>
                          ) : (linkable && (
                            <MsButton
                              tone="ghost"
                              busy={busyKey === `link:${row.id}`}
                              disabled={rowBusy}
                              onClick={() => linkExternal(row)}
                              data-testid="device-manager-link"
                            >
                              {copy.link}
                            </MsButton>
                          ))}
                          {armed ? (
                            <>
                              <label className="flex items-center gap-1.5 text-[12px] text-black/60 dark:text-white/60">
                                <input
                                  type="checkbox"
                                  className="accent-[#0B57D0]"
                                  data-testid="device-manager-delete-weights"
                                  checked={deleteWeights}
                                  onChange={(event) => setDeleteWeights(event.target.checked)}
                                />
                                {copy.deleteWeights}
                              </label>
                              <MsButton
                                tone="danger"
                                busy={busyKey === `remove:${row.id}`}
                                disabled={rowBusy}
                                onClick={() => confirmRemove(row)}
                                data-testid="device-manager-remove-confirm"
                              >
                                {copy.removeArmed}
                              </MsButton>
                              <MsButton tone="ghost" disabled={rowBusy} onClick={cancelRemove}>
                                {copy.removeCancel}
                              </MsButton>
                            </>
                          ) : (
                            <MsButton
                              tone="danger"
                              disabled={rowBusy}
                              onClick={() => armRemove(row)}
                              data-testid="device-manager-remove"
                            >
                              {copy.remove}
                            </MsButton>
                          )}
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}

              {/* Empty guidance: nothing installed anywhere. */}
              {storeRows.length === 0 && externalModels.length === 0 && (
                storeState === 'loading' && !external ? (
                  <div className={`py-4 text-center text-[13px] ${MUTED_TEXT}`}>{copy.loading}</div>
                ) : (
                  <div
                    data-testid="device-manager-models-empty"
                    className={`py-4 text-center text-[13px] leading-6 ${MUTED_TEXT}`}
                  >
                    {copy.ollamaOfflineHint}
                  </div>
                )
              )}
            </div>
          </SectionShell>

          {/* ── AI doctor ──────────────────────────────────────────── */}
          <SectionShell id="doctor" icon={AlertTriangle} title={copy.sectionDoctor}>
            <div className="space-y-4">
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3" data-testid="device-manager-doctor-cards">
                {DOCTOR_CARDS.map(({ kind, icon: Icon, titleKey, descKey }) => (
                  <button
                    key={kind}
                    type="button"
                    data-testid={`device-manager-doctor-${kind}`}
                    disabled={!!doctorBusy}
                    onClick={() => runDoctor(kind)}
                    className={`${CARD_CLASS} flex flex-col items-start gap-2 p-5 text-left transition-all duration-300 hover:-translate-y-0.5 disabled:cursor-not-allowed disabled:opacity-60`}
                  >
                    <span className="flex h-9 w-9 items-center justify-center rounded-full bg-black/[0.04] dark:bg-white/[0.06]">
                      {doctorBusy === kind
                        ? <RefreshCw size={16} className="animate-spin text-black/55 dark:text-white/55 motion-reduce:animate-none" />
                        : <Icon size={16} className="text-black/55 dark:text-white/55" />}
                    </span>
                    <span className="text-[14px] font-semibold text-black/80 dark:text-white/80">{copy[titleKey]}</span>
                    <span className={`text-[12px] leading-5 ${MUTED_TEXT}`}>{copy[descKey]}</span>
                  </button>
                ))}
              </div>
              <div className={`${CARD_CLASS} p-5`}>
                <textarea
                  data-testid="device-manager-doctor-input"
                  value={question}
                  onChange={(event) => setQuestion(event.target.value)}
                  placeholder={copy.doctorCustomPlaceholder}
                  rows={3}
                  className="w-full resize-none rounded-[14px] border border-black/[0.08] bg-white/70 px-3.5 py-2.5 text-[13px] outline-none transition-colors placeholder:text-black/30 focus:border-[#0B57D0] dark:border-white/[0.08] dark:bg-white/[0.04] dark:placeholder:text-white/30 dark:focus:border-[#A8C7FA]"
                />
                <div className="mt-2 flex items-center justify-end gap-3">
                  {question.length >= COUNTER_THRESHOLD && (
                    <span
                      data-testid="device-manager-doctor-counter"
                      className={`text-[11px] font-mono ${question.length > QUESTION_MAX ? 'text-[#FF3B30] dark:text-[#FF453A]' : MUTED_TEXT}`}
                    >
                      {copy.doctorCharCounter(question.length)}
                    </span>
                  )}
                  <MsButton
                    tone="primary"
                    busy={doctorBusy === 'custom'}
                    disabled={questionInvalid || (!!doctorBusy && doctorBusy !== 'custom')}
                    onClick={sendCustom}
                    data-testid="device-manager-doctor-send"
                  >
                    <Send size={14} />
                    {copy.doctorSend}
                  </MsButton>
                </div>
              </div>
            </div>
          </SectionShell>
        </div>

        <MsToast toast={toast} />
      </div>
    </div>
  );
};

export { DeviceManagerView };
