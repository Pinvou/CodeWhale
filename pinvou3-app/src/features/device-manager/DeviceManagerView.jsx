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
import { MonitorView, MONITOR_CLOCK_LOCALE } from '../monitor/MonitorView.jsx';
import { fetchLibrary, storeAvailability } from '../model-store/model-store-api.js';
import { libraryStatusOf } from '../model-store/model-store-logic.js';
import { buildSavedModel, findLinkedRows, isLocalGatewayUrl } from '../model-store/pinvou-model-link.js';
import { MsButton, MsPill, MsToast } from '../model-store/model-store-ui.jsx';

const TOAST_MS = 3200;
// Two-step confirmation dead-zone (ms): long enough to absorb a double-click's
// second click, short enough to feel instant (see armRemove).
const CONFIRM_GATE_MS = 400;
// Slow-cadence probe for model services (ollama/vllm/lmstudio/docker); the
// bridge documents the same 10s rhythm next to pollDeviceServices.
const SERVICES_POLL_MS = 10000;
// Ollama rows listed straight from the local service (not yet in the registry)
// carry no base_url, but the daemon is fixed on the loopback: linking can
// assume the canonical /v1 address (same one the registry examples document).
const OLLAMA_LINK_BASE_URL = 'http://127.0.0.1:11434/v1';
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

// sensor chipKey (from bridge/device.js sensorChipKey) -> i18n key. Unknown
// chipKey falls back to the raw chip text at render time.
const CHIP_NAME_KEYS = {
  nvme: 'chipNvme',
  wifi: 'chipWifi',
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

// Model service key (see the backend ServiceStatus docs for the key set)
// -> i18n label key. Unknown keys fall back to the raw key at render.
const SERVICE_LABEL_KEYS = {
  ollama: 'svcOllama',
  vllm: 'svcVllm',
  lmstudio: 'svcLmStudio',
  docker: 'svcDocker',
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

// Hottest temperature entry across all sensor groups. The header chip reuses
// the SAME bridge-computed warn/crit level as the sensor card — fixed header
// thresholds used to contradict the per-channel limit the card showed for the
// very same reading (round-4 review P3). "Top sensor temp" is the max across
// ALL temperature channels (NVMe, board, ...): the snapshot has no dedicated
// GPU temperature source, and a GPU-specific label would mislead.
const maxSensorTemperature = (sensors) => {
  let top = null;
  for (const group of Array.isArray(sensors) ? sensors : []) {
    for (const entry of Array.isArray(group?.entries) ? group.entries : []) {
      if (entry?.kind !== 'temperature') continue;
      const value = parseFloat(entry.valueText);
      if (Number.isFinite(value) && (top == null || value > top.value)) top = entry;
    }
  }
  return top == null ? null : { value: top.value, level: top.level };
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
      // The crit label only consumes `critical`: on the temperature side the
      // bridge's `max` is sysinfo's `tempN_highest` (a since-boot historical
      // peak), not a threshold — falling back to it rendered a one-off spike
      // as if it were the critical line (round-7 review P2). Peaks get their
      // own honest label; non-temperature maxes (hwmon thresholds for
      // fans/voltages) keep the original semantics.
      const limitValue = entry.critical != null ? entry.critical : (entry.kind !== 'temperature' ? entry.max : null);
      const limitText = limitValue != null
        ? copy.critLimit(`${Math.round(limitValue)}${limitUnitOfKind(entry.kind)}`)
        : null;
      const peakText = entry.kind === 'temperature' && entry.max != null
        ? copy.peakLabel(`${Math.round(entry.max)}${limitUnitOfKind(entry.kind)}`)
        : null;
      return (
        <div
          // Index is the final tiebreak: the backend sorts entries
          // deterministically (chip/kind/name/value), so kind+name+index is
          // stable across ticks. The formatted reading must NOT be part of the
          // key — it jitters every 2s tick and would remount the row (and
          // duplicate when two same-label entries round to the same text)
          // (round-8 review P2).
          key={`${entry.kind}-${entry.name}-${index}`}
          className="flex items-start justify-between gap-3"
        >
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
            {!limitText && peakText && <div className={`text-[11px] ${MUTED_TEXT}`}>{peakText}</div>}
          </div>
        </div>
      );
    })}
  </div>
);

const DiskRow = ({ disk, copy }) => {
  const level = pctLevel(disk.usedPct, DISK_WARN_PCT, DISK_CRIT_PCT);
  // usageKnown comes from the bridge (total and available both present): the
  // row no longer couples to the em-dash display string. A disk with a total
  // but no available value used to render as a 0-width green bar that read as
  // "empty disk" (round-4 review P3).
  const known = disk.usageKnown === true;
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
      {known ? (
        <div
          className="h-2 overflow-hidden rounded-full bg-black/5 dark:bg-white/10"
          // Screen-reader usage semantics: non-root cards only show
          // "available / total" text, the percentage lives solely in the header
          // chip — give the bar progressbar semantics (round-5 review P3).
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(Math.max(0, Math.min(100, disk.usedPct)))}
          // fmtDisks exposes mountPoint only (no disk name) — the old
          // `disk.name` fallback was always undefined; unknown mounts read
          // "—" to screen readers instead of "undefined 45%" (round-6 review).
          aria-label={`${disk.mountPoint || '—'} ${Math.round(Math.max(0, disk.usedPct))}%`}
        >
          <div
            className="h-full rounded-full transition-all duration-500"
            style={{ width: `${Math.max(0, Math.min(100, disk.usedPct))}%`, backgroundColor: LEVEL_COLOR[level] }}
          />
        </div>
      ) : (
        // Unknown capacity: no usage bar at all — a 0-width green bar reads as "empty disk".
        <div className="h-2 rounded-full bg-black/5 dark:bg-white/10" data-testid="device-manager-disk-capacity-unknown" />
      )}
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

// `placeholder` replaces the em-dash while the first snapshot is still in
// flight, so a slow first frame shows the loading line instead of "no data".
const ProcList = ({ title, rows, metricKey, testId, placeholder = null }) => (
  <div className={`${CARD_CLASS} p-5`} data-testid={testId}>
    <div className="mb-3 text-[12px] font-bold uppercase tracking-wider text-black/50 dark:text-white/50">{title}</div>
    {(rows || []).length === 0 ? (
      <div className={`text-[12px] ${MUTED_TEXT}`}>{placeholder || '\u2014'}</div>
    ) : (
      <div className="space-y-2">
        {rows.map((proc) => (
          <div key={`${proc.pid}`} className="flex items-center justify-between gap-3 text-[13px]">
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

// One muted chip per model service in the models section header: green dot
// while the probe answers, gray otherwise, detail as the title tooltip.
const ServiceChip = ({ label, running, detail, stateText }) => (
  <span
    data-testid="device-manager-service-chip"
    title={detail || undefined}
    className="inline-flex items-center gap-1.5 rounded-full bg-black/[0.04] px-2.5 py-1 text-[11px] font-semibold dark:bg-white/[0.06]"
  >
    {/* Running state used to be conveyed by the dot color alone (round-7
        review P3): the dot carries an aria-label so screen-reader and
        color-blind users can tell running from stopped. */}
    <span
      aria-label={stateText || undefined}
      role="img"
      className={`h-1.5 w-1.5 rounded-full ${running ? LEVEL_DOT.ok : 'bg-black/25 dark:bg-white/30'}`}
    />
    <span className={MUTED_TEXT}>{label}</span>
  </span>
);

// Effective link identity of an external-model row. Registry rows carry their
// own base_url/model; Ollama rows listed straight from the service have
// neither, so fall back to the fixed loopback daemon address and the listing
// name (which is exactly the model id the daemon serves). Keyed on `kind`, not
// `source`: the backend flips source to "registry" on registration (round-8
// review P2) while ollama is the only kind whose registrations may legally omit
// base_url — keying on source stranded those rows with no link control.
const linkTargetOf = (row) => ({
  model: row.model || (row.kind === 'ollama' ? row.name : null),
  baseUrl: row.base_url || (row.kind === 'ollama' ? OLLAMA_LINK_BASE_URL : null),
});

// Store model_name never contains ':' (the store's name generator folds it to
// '-') while daemon-side names always carry a ':tag'. Strip the tag before
// store comparisons so the store's own ollama cards, listed as `name:latest`,
// are recognized as store-managed instead of removable external rows
// (round-8 review).
const stripModelTag = (value) => String(value || '').toLowerCase().split(':')[0].trim();

const DeviceManagerView = ({ theme, t, bs, onOpenChat, onOpenModelStore }) => {
  const copy = t.uiDeviceMgr;
  const deviceFmt = bs?.device?._fmt || null;
  const deviceError = bs?.deviceError || null;
  const monitorFmt = bs?.monitor?._fmt || null;
  const external = bs?.externalModels || null;
  const externalModels = Array.isArray(external?.models) ? external.models : [];
  const externalWarnings = Array.isArray(external?.warnings) ? external.warnings : [];
  // Model-service probe result: null until the first round answers, then one
  // { key, running, detail } per service.
  const deviceServices = Array.isArray(bs?.deviceServices) ? bs.deviceServices : null;

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

  // Polling lifecycle mirrors MonitorView: start on mount (with one immediate
  // services probe + external-model fetch), stop on unmount. The services
  // probe keeps its slow 10s cadence for as long as the page stays mounted.
  const deviceApiReady = !!((window.TauriBridge || bridge)?.device?.startDevicePolling);
  // Display strings go through a ref: the mount effect must not depend on
  // `copy`, or a language switch would stop/restart polling, re-fetch external
  // models and re-toast a failure (round-5 review P3). showToast is a stable
  // useCallback, safe as a dependency.
  const copyRef = useRef(copy);
  useEffect(() => {
    copyRef.current = copy;
  }, [copy]);
  useEffect(() => {
    const liveBridge = window.TauriBridge || bridge;
    const dev = liveBridge?.device;
    if (!dev || typeof dev.startDevicePolling !== 'function') {
      console.warn('[device-manager] TauriBridge device API unavailable');
      return undefined;
    }
    // The web host's device domain is a reject-style stub: without a catch here,
    // a gating regression would surface as unhandled-rejection noise (round-5
    // review P3; every other call in this effect has one).
    Promise.resolve(dev.startDevicePolling()).catch(() => {});
    const pollServices = () => {
      if (typeof dev.pollDeviceServices === 'function') dev.pollDeviceServices().catch(() => {});
    };
    pollServices();
    const servicesTimer = setInterval(pollServices, SERVICES_POLL_MS);
    // The first fetch failing silently would land the models area on the
    // "nothing installed" hint, which reads as a fact instead of the read
    // failure it is (same surface rule as the manual Refresh button).
    if (typeof dev.fetchExternalModels === 'function') {
      dev.fetchExternalModels()
        .then(() => setExternalFetchError(null))
        .catch((error) => {
          setExternalFetchError(String(error?.message || error));
          showToast(copyRef.current.refreshFailed(String(error?.message || error)));
        });
    }
    return () => {
      clearInterval(servicesTimer);
      // Same reject-style-stub defense as startDevicePolling above (round-6
      // review: the comment claimed "every other call has one" — this one
      // didn't).
      if (typeof dev.stopDevicePolling === 'function') {
        Promise.resolve(dev.stopDevicePolling()).catch(() => {});
      }
    };
  }, [showToast]);

  // Read-failure retry: stop+start makes the facade poll immediately (the raw
  // pollDevice stays feature-internal), then refresh the slow services probe.
  const retryPoll = useCallback(() => {
    const dev = (window.TauriBridge || bridge)?.device;
    if (!dev) return;
    if (typeof dev.stopDevicePolling === 'function') {
      Promise.resolve(dev.stopDevicePolling()).catch(() => {});
    }
    if (typeof dev.startDevicePolling === 'function') {
      Promise.resolve(dev.startDevicePolling()).catch(() => {});
    }
    if (typeof dev.pollDeviceServices === 'function') dev.pollDeviceServices().catch(() => {});
  }, []);

  const refreshExternal = useCallback(() => {
    const dev = (window.TauriBridge || bridge)?.device;
    if (!dev?.fetchExternalModels) return;
    // Swallowing the rejection makes Refresh look dead: surface a human-readable failure.
    dev.fetchExternalModels()
      .then(() => setExternalFetchError(null))
      .catch((error) => {
        setExternalFetchError(String(error?.message || error));
        showToast(copy.refreshFailed(String(error?.message || error)));
      });
  }, [copy, showToast]);

  // ── Model store (read-only here) ─────────────────────────────────
  // The store is a pure Linux feature; on targets without it this page hides
  // the store group silently and still manages external models.
  const [storeState, setStoreState] = useState('loading');
  const [storeError, setStoreError] = useState(null);
  const [storeReloadKey, setStoreReloadKey] = useState(0);
  const [library, setLibrary] = useState([]);
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const status = await storeAvailability();
        if (!alive) return;
        if (!status?.available) {
          // The platform genuinely has no store: hiding the whole group is correct.
          setStoreState('unavailable');
          return;
        }
        const lib = await fetchLibrary();
        if (!alive) return;
        setLibrary(Array.isArray(lib.models) ? lib.models : []);
        setStoreState('available');
      } catch (error) {
        // "Store exists but the read failed" must stay distinct from unavailable:
        // human-readable failure + retry + a standalone store entry, instead of
        // silently dropping the whole store group (same as the store page).
        if (!alive) return;
        setStoreError(String(error?.message || error));
        setStoreState('error');
      }
    })();
    return () => {
      alive = false;
    };
  }, [storeReloadKey]);
  const storeRows = storeState === 'available' ? library : [];

  // ── External models: link into Pinvou / guarded removal ──────────
  const [busyKey, setBusyKey] = useState(null);
  const [armedRemoveId, setArmedRemoveId] = useState(null);
  const [deleteWeights, setDeleteWeights] = useState(false);
  // Two-step confirmation gate: the armed cluster replaces the Remove button in
  // place, so the second click of a plain double-click would land on whatever
  // now occupies those coordinates — the confirm button (immediate removal on
  // rows without weights) or the delete-weights checkbox (silently pre-arming
  // the destructive option). A short dead-zone after arming turns that gesture
  // back into two deliberate steps (round-4 review P2).
  const [confirmGateReady, setConfirmGateReady] = useState(true);
  const confirmGateTimerRef = useRef(null);
  useEffect(() => () => { if (confirmGateTimerRef.current) clearTimeout(confirmGateTimerRef.current); }, []);
  // Initial fetch failure must not read as the "nothing installed" fact: keep
  // an error slot so the empty area can show the real read failure + retry
  // (round-4 review P3).
  const [externalFetchError, setExternalFetchError] = useState(null);

  // Identity for armed/busy keys: source+kind-qualified row name. The NAME (not
  // the id) stays the user-facing identity through the quiet re-register id swap
  // (round-3 review P2); the source qualifier keeps a registry row and an
  // Ollama-listing row that share a name from arming/busying together, and the
  // kind qualifier keeps two registry rows that share a name across kinds
  // separate (the registry does not enforce name uniqueness — round-4/5 review
  // P3). Kind never changes for a row, so the identity survives the id swap.
  const rowKey = (row) => `${row.source || 'registry'}:${row.kind || ''}:${row.name}`;

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
    const target = linkTargetOf(row);
    // Same loopback ruler as the cleanup side (pinvou-model-link only recycles
    // local-gateway rows): linking a remote endpoint would make it the default
    // model — every new chat (and the AI doctor prompts) would go there — and
    // the row could never be recycled on removal. Remote endpoints must be
    // configured explicitly in Settings instead (round-3 review P1).
    if (!target.baseUrl || !isLocalGatewayUrl(target.baseUrl)) {
      showToast(copy.linkDisabledRemote);
      return;
    }
    // Cross-row mutex entry guard (round-6 review P2): mirror of the
    // otherRowBusy disable — keeps a stale-closure click from overwriting
    // the busy slot while another row's saveModel/remove is still in flight.
    if (busyKey) return;
    setBusyKey(`link:${rowKey(row)}`);
    try {
      // findLinkedRows takes (...names): pass both the registry model and the
      // listing name so a relink updates the already-saved row in place
      // (existingId) instead of duplicating it in Pinvou's model list.
      const existing = findLinkedRows(bs?.savedModels, row.model, row.name)[0] || null;
      const saved = buildSavedModel({
        modelName: target.model,
        displayName: row.name,
        baseUrl: target.baseUrl,
        existingId: existing?.id || null,
      });
      await liveBridge.models.saveModel(saved);
      if (liveBridge.models.setActiveModel) await liveBridge.models.setActiveModel(saved.id);
      showToast(copy.linkDone);
    } catch (error) {
      // Same localized-prefix discipline as removeFailed/refreshFailed: a raw
      // backend string alone reads as a foreign system error (round-4 review P3).
      showToast(copy.linkFailed(String(error?.message || error)));
    } finally {
      // Compare-and-set clear, same contract as confirmRemove (round-5 review
      // P2): linkExternal's own finally cleared unconditionally, which would
      // revive the other row's buttons if the two guards ever raced
      // (round-7 review P3 alignment).
      setBusyKey((current) => (current === `link:${rowKey(row)}` ? null : current));
    }
  }, [bs, busyKey, copy, showToast, t]);

  // Identity for busy/armed keys: the row NAME, not the id. Unregistered
  // Ollama rows swap their synthetic `ollama-*` id for a registry id the
  // moment confirmRemove's quiet registration refetches the list — id-keyed
  // state would silently disarm the armed panel and re-enable the buttons
  // mid-removal (round-3 review P2). The confirm-name flow already makes the
  // name the user-facing identity of a row.
  const armRemove = useCallback((row) => {
    setDeleteWeights(false);
    setConfirmGateReady(false);
    if (confirmGateTimerRef.current) clearTimeout(confirmGateTimerRef.current);
    confirmGateTimerRef.current = setTimeout(() => setConfirmGateReady(true), CONFIRM_GATE_MS);
    setArmedRemoveId(rowKey(row));
  }, []);
  const cancelRemove = useCallback(() => {
    setArmedRemoveId(null);
    setDeleteWeights(false);
    setConfirmGateReady(true);
    if (confirmGateTimerRef.current) clearTimeout(confirmGateTimerRef.current);
  }, []);

  // Guarded removal through the backend command. Ollama rows that were listed
  // straight from the local service (not yet in the registry) have no usable
  // id: register them quietly first, then remove with the returned record id
  // so the same guard chain (store boundary, weights sandbox, confirm name)
  // applies to them too.
  const confirmRemove = useCallback(async (row) => {
    const dev = (window.TauriBridge || bridge)?.device;
    // Bridge missing is a silent no-op otherwise: the armed panel already
    // collapsed on click, so the user would watch the row stay with no
    // feedback at all (round-7 review P3).
    if (!dev?.removeExternalModel) {
      showToast(t.uiModelStore.linkBridgeMissing);
      return;
    }
    // Cross-row mutex entry guard (round-6 review P2): see linkExternal.
    if (busyKey) return;
    setBusyKey(`remove:${rowKey(row)}`);
    try {
      let id = row.id;
      if (row.source === 'ollama' && !row.registered) {
        const record = await dev.registerExternalModel({
          name: row.name,
          kind: 'ollama',
          model: row.model || row.name,
        });
        // Backend contract break (register ok but no id): a localized message
        // instead of the raw 'missing id' token (round-7 review P3).
        if (!record?.id) throw new Error(copy.registerNoId);
        id = record.id;
      }
      const outcome = await dev.removeExternalModel({
        id,
        deleteWeights,
        confirmName: row.name,
      });
      // Recycle rows linked into Pinvou (same contract as
      // ModelStoreView.cleanLinkedRows): the backend deletes ollama models from
      // the daemon unconditionally, so a leftover Pinvou row (possibly the
      // default model for new chats) silently breaks the next conversation.
      // Distinguish "could not check" from "nothing to clean": without the
      // snapshot/delete capability, treat cleanup as failed — better an
      // explicit "please delete manually" than a false all-clear.
      let cleanupOk = true;
      let cleanupErrorText = null;
      if (Array.isArray(bs?.savedModels)) {
        const linkedRows = findLinkedRows(bs.savedModels, row.model, row.name);
        const liveBridge = window.TauriBridge || bridge;
        // Assign the OUTER variable (an inner `let` here used to shadow it and
        // every cleanup failure surfaced as "unknown" — round-3 review P2).
        // Reasons are localized (round-7 review P3): the raw tokens used to
        // leak into the zh/ja toast on this failure path.
        cleanupErrorText = liveBridge?.models?.deleteModel ? null : copy.linkReasonBridge;
        for (const linked of cleanupErrorText ? [] : linkedRows) {
          try {
            await liveBridge.models.deleteModel(linked.id);
          } catch (error) {
            cleanupErrorText = String(error?.message || error);
            break;
          }
        }
        if (cleanupErrorText) cleanupOk = false;
      } else {
        cleanupErrorText = copy.linkReasonBridge;
        cleanupOk = false;
      }
      const completedKey = rowKey(row);
      if (armedRemoveId === completedKey) {
        setArmedRemoveId(null);
        setDeleteWeights(false);
      }
      // One aggregated toast: the single-slot toast used to show only the LAST
      // warning, drowning both the backend warnings and the cleanup failure
      // (round-3 review P2). Outcome + warnings travel together now.
      const warnings = Array.isArray(outcome?.warnings) ? outcome.warnings : [];
      const notices = warnings.map((warning) => copy.warn(warning));
      if (cleanupOk) {
        notices.push(copy.removeDone(row.name));
      } else {
        // Cleanup failure must be surfaced, not drowned by a plain success toast.
        notices.push(copy.linkRowStale(row.name, cleanupErrorText || copy.linkReasonUnknown));
      }
      showToast(notices.join('\n'));
    } catch (error) {
      // Backend guard failures carry a human-readable message already.
      showToast(copy.removeFailed(String(error?.message || error)));
    } finally {
      // Compare-and-set clear (round-5 review P2): busyKey is a single slot for
      // the whole page — an unconditional clear would revive the other row's
      // buttons while its own operation is still in flight, allowing a concurrent
      // second saveModel / removeExternalModel.
      setBusyKey((current) => (current === `remove:${rowKey(row)}` ? null : current));
    }
  }, [armedRemoveId, bs, busyKey, copy, deleteWeights, showToast, t]);

  // ── AI doctor actions ────────────────────────────────────────────
  const [doctorBusy, setDoctorBusy] = useState(null);
  const [question, setQuestion] = useState('');
  const runDoctor = useCallback(async (kind, questionText = null, onDone = null) => {
    const dev = (window.TauriBridge || bridge)?.device;
    // Same surface discipline as confirmRemove/linkExternal: a bridge-missing
    // entry is toasted, not a silent no-op the user watches do nothing
    // (round-8 review P3).
    if (!dev?.buildDoctorPrompt || typeof onOpenChat !== 'function') {
      showToast(t.uiModelStore.linkBridgeMissing);
      return;
    }
    // Entry guard mirroring busyKey/linkExternal: the disabled buttons are the
    // first line of defense, this keeps a stale-closure double click from
    // building two doctor prompts / two sessions (round-7 review P3).
    if (doctorBusy) return;
    setDoctorBusy(kind);
    try {
      const prompt = await dev.buildDoctorPrompt(kind, questionText);
      const sent = await onOpenChat(prompt);
      // onOpenChat returns a boolean by contract: false = not sent (bridge
      // unavailable / navigated away / send failed). Surface it and skip onDone —
      // the custom question stays in the input instead of being silently cleared.
      if (sent === false) {
        showToast(copy.doctorSendFailed);
        return;
      }
      if (onDone) onDone();
    } catch (error) {
      // Same localized-prefix discipline as removeFailed/refreshFailed: a raw
      // zh backend string must not surface verbatim to en/ja users (round-5
      // review P3).
      showToast(copy.doctorFailed(String(error?.message || error)));
    } finally {
      setDoctorBusy(null);
    }
  }, [copy, doctorBusy, onOpenChat, showToast, t]);
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
  const topTemp = maxSensorTemperature(deviceFmt?.sensors);
  const locale = MONITOR_CLOCK_LOCALE[t.langTag] || 'zh-CN';
  // updatedAt is when the data last changed (frozen under display-equivalence
  // suppression) — exactly what the label should say. No more 10s tick that
  // re-renders the whole tree without changing a single displayed value.
  const updatedAtText = deviceFmt && Number.isFinite(deviceFmt.updatedAt)
    ? new Date(deviceFmt.updatedAt).toLocaleTimeString(locale, { hour12: false })
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
              {rootDisk?.usageKnown === true && (
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
              {topTemp != null && (
                <HealthChip
                  label={copy.chipMaxTemp}
                  value={`${topTemp.value}\u00B0C`}
                  // Same bridge-computed level as the sensor card; the fixed
                  // thresholds only apply when the level is missing.
                  level={topTemp.level || pctLevel(topTemp.value, GPU_TEMP_WARN_C, GPU_TEMP_CRIT_C)}
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
            {/* First frames (snapshot not yet arrived) show the loading line;
                the empty state is only truthful once a real snapshot landed. */}
            {!deviceFmt ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.loading}</div>
            ) : deviceFmt.sensors.length === 0 ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.sensorEmpty}</div>
            ) : (
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                {deviceFmt.sensors.map((group) => (
                  <SensorGroupCard key={group.chip} group={group} copy={copy} />
                ))}
              </div>
            )}
          </SectionShell>

          {/* ── Storage ────────────────────────────────────────────── */}
          <SectionShell id="storage" icon={Database} title={copy.sectionStorage}>
            {!deviceFmt ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.loading}</div>
            ) : deviceFmt.disks.length === 0 ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.storageEmpty}</div>
            ) : (
              <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
                {deviceFmt.disks.map((disk, index) => (
                  // A bind-mounted path can be listed twice, colliding
                  // mountPoint keys (round-7 review P3): the index is only a
                  // tiebreak — single-disk lists still key off the mount point.
                  <DiskRow key={`${disk.mountPoint || '—'}-${index}`} disk={disk} copy={copy} />
                ))}
              </div>
            )}
          </SectionShell>

          {/* ── Network ────────────────────────────────────────────── */}
          <SectionShell id="network" icon={Radio} title={copy.sectionNetwork}>
            {!deviceFmt ? (
              <div className={`${CARD_CLASS} p-6 text-center text-[13px] ${MUTED_TEXT}`}>{copy.loading}</div>
            ) : deviceFmt.network.length === 0 ? (
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
                  placeholder={deviceFmt ? null : copy.loading}
                />
                <ProcList
                  testId="device-manager-proc-mem"
                  title={copy.procMemTitle}
                  rows={deviceFmt?.processes?.topMemory}
                  metricKey="memText"
                  placeholder={deviceFmt ? null : copy.loading}
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

              {/* Stale-baseline hint (round-6 review P2): probes failed
                  repeatedly while an old list is still on display — the chips
                  below may not reflect reality anymore. */}
              {bs?.deviceServicesError && deviceServices ? (
                <div className={`text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-services-stale">
                  {copy.servicesStale}
                </div>
              ) : null}

              {/* Model services probe: one subtle line of chips (null = the
                  first probe is still in flight). */}
              {bs?.deviceServicesError && !deviceServices ? (
                <div className={`text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-services-failed">
                  {copy.servicesFailed(String(bs.deviceServicesError))}
                </div>
              ) : !deviceServices ? (
                <div className={`text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-services-loading">
                  {copy.servicesLoading}
                </div>
              ) : deviceServices.length > 0 && (
                <div className="flex flex-wrap items-center gap-1.5" data-testid="device-manager-services">
                  {deviceServices.map((service) => (
                    <ServiceChip
                      key={service.key}
                      label={copy[SERVICE_LABEL_KEYS[service.key]] || service.key}
                      running={!!service.running}
                      detail={service.detail}
                      stateText={service.running ? copy.statusRunning : copy.statusUnreachable}
                    />
                  ))}
                </div>
              )}

              {externalWarnings.length > 0 && (
                <div className={`space-y-0.5 text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-model-warnings">
                  {externalWarnings.map((warning, index) => (
                    <div key={index}>{copy.warn(warning)}</div>
                  ))}
                </div>
              )}

              {/* Store reachable but read failed: human-readable failure +
                  retry + a standalone path to the store (the per-row jump
                  buttons only exist inside store rows). */}
              {storeState === 'error' && (
                <div
                  className="flex flex-wrap items-center justify-between gap-2 rounded-2xl bg-amber-50 px-4 py-3 dark:bg-amber-400/10"
                  data-testid="device-manager-store-error"
                >
                  <span className="min-w-0 break-words text-[12px] text-[#A15C07] dark:text-[#FDD663]">
                    {t.uiModelStore.storeLoadFailed(storeError || '')}
                  </span>
                  <span className="flex shrink-0 items-center gap-2">
                    <MsButton tone="ghost" onClick={() => setStoreReloadKey((key) => key + 1)}>
                      {copy.retry}
                    </MsButton>
                    <MsButton tone="ghost" onClick={onOpenModelStore}>{copy.gotoModelStore}</MsButton>
                  </span>
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
                    // Unregistered Ollama listing rows resolve through the
                    // fixed loopback fallback, so the link button shows for
                    // them too (see linkTargetOf). Remote endpoints are
                    // neither: linking them is blocked (see linkExternal).
                    const target = linkTargetOf(row);
                    const linkable = !!(target.model && target.baseUrl && isLocalGatewayUrl(target.baseUrl));
                    const remoteOnly = !!(target.model && target.baseUrl && !isLocalGatewayUrl(target.baseUrl));
                    const rowIdentity = rowKey(row);
                    const armed = armedRemoveId === rowIdentity;
                    const kindLabel = copy[KIND_LABEL_KEYS[row.kind] || 'kindOther'];
                    const rowBusy = busyKey === `link:${rowIdentity}` || busyKey === `remove:${rowIdentity}`;
                    // Cross-row mutex (round-6 review P2): busyKey is a single
                    // slot for the whole page. Without this, confirming row B
                    // while row A's remove is in flight overwrites the slot and
                    // revives A's buttons mid-flight (the CAS clear only guards
                    // the finish path) — a second remove could then hit the same
                    // id. Disabling every row's actions while any row is busy is
                    // the honest expression of that single slot.
                    const otherRowBusy = !!busyKey && !rowBusy;
                    // Daemon rows whose model name is store-managed only fail at
                    // confirm time today (backend boundary rejects the quiet
                    // registration); pre-disable with the same managed hint so
                    // the user learns before arming (round-4 review P3). For
                    // ollama rows the backend also checks the effective deletion
                    // name (record.model, falling back to name, case-insensitive)
                    // — mirror that arm so an alias registry row whose MODEL is
                    // store-managed pre-disables too instead of arming into a
                    // guaranteed rejection (round-6 review).
                    const managedByStore = storeRows.some((record) => {
                      const storeName = String(record.model_name || '').toLowerCase();
                      if (storeName === stripModelTag(row.name)) return true;
                      return (
                        row.kind === 'ollama' &&
                        !!row.model &&
                        storeName === stripModelTag(row.model)
                      );
                    });
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
                          <div className="mt-2 space-y-1">
                            <p className={`text-[12px] leading-5 ${MUTED_TEXT}`}>{copy.removeConfirmIntro(row.name)}</p>
                            {row.weights_path && (
                              <p className={`break-all font-mono text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-remove-weights-path">
                                {copy.removeWeightsPath(row.weights_path)}
                              </p>
                            )}
                          </div>
                        )}
                        <div className="mt-2 flex flex-wrap items-center gap-2">
                          {linked ? (
                            <span
                              data-testid="device-manager-linked-badge"
                              className="inline-flex items-center rounded-full bg-[#E6F4EA] px-2.5 py-1 text-[11px] font-semibold text-[#137333] dark:bg-[#1E3A2A] dark:text-[#81C995]"
                            >
                              {copy.linked}
                            </span>
                          ) : remoteOnly ? (
                            <MsButton
                              tone="ghost"
                              disabled
                              title={copy.linkDisabledRemote}
                              data-testid="device-manager-link-remote"
                            >
                              {copy.link}
                            </MsButton>
                          ) : (linkable && (
                            <MsButton
                              tone="ghost"
                              busy={busyKey === `link:${rowIdentity}`}
                              // Never link an unreachable model: linking also
                              // makes it the default model, and an unreachable
                              // default breaks every new chat (same reasoning
                              // as pinvou-model-link.checkLinkable).
                              disabled={rowBusy || otherRowBusy || row.status === 'unreachable'}
                              title={row.status === 'unreachable' ? copy.linkDisabledUnreachable : undefined}
                              onClick={() => linkExternal(row)}
                              data-testid="device-manager-link"
                            >
                              {copy.link}
                            </MsButton>
                          ))}
                          {armed ? (
                            <>
                              {/* Checkbox follows the registered weights_path
                                  (any kind): the backend only deletes the dir
                                  when this is checked, so hiding it on an
                                  ollama row that DOES carry weights_path would
                                  echo a deletion that can never happen. */}
                              {!!row.weights_path && (
                                <label className="flex items-center gap-1.5 text-[12px] text-black/60 dark:text-white/60">
                                  <input
                                    type="checkbox"
                                    className="accent-[#0B57D0]"
                                    data-testid="device-manager-delete-weights"
                                    checked={deleteWeights}
                                    // Gated by the same dead-zone as the confirm
                                    // button: the armed cluster replaces the Remove
                                    // button in place, so a double-click's second
                                    // click landing on the checkbox coordinates must
                                    // not silently pre-arm deletion (round-5 review P3).
                                    disabled={!confirmGateReady}
                                    onChange={(event) => setDeleteWeights(event.target.checked)}
                                  />
                                  {copy.deleteWeights}
                                </label>
                              )}
                              {deleteWeights && (
                                <span className="text-[11px] text-[#A15C07] dark:text-[#FDD663]">
                                  {copy.removeIrreversible}
                                </span>
                              )}
                              <MsButton
                                tone="danger"
                                busy={busyKey === `remove:${rowIdentity}`}
                                disabled={rowBusy || otherRowBusy || !confirmGateReady}
                                onClick={() => confirmRemove(row)}
                                data-testid="device-manager-remove-confirm"
                              >
                                {copy.removeArmed}
                              </MsButton>
                              <MsButton
                                tone="ghost"
                                disabled={rowBusy}
                                onClick={cancelRemove}
                                data-testid="device-manager-remove-cancel"
                              >
                                {copy.removeCancel}
                              </MsButton>
                            </>
                          ) : (
                            <MsButton
                              tone="danger"
                              disabled={rowBusy || otherRowBusy || managedByStore}
                              title={managedByStore ? copy.storeManaged : undefined}
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

              {/* The listing comes from the local daemon ports; any local
                  process can impersonate them (nothing authenticates on
                  loopback). One quiet caption keeps that honest. */}
              {externalModels.length > 0 && (
                <p className={`text-[11px] ${MUTED_TEXT}`} data-testid="device-manager-local-source-hint">
                  {copy.localSourceHint}
                </p>
              )}

              {/* Empty guidance: nothing installed anywhere. A failed read must
                  not degrade into the "nothing installed" fact — show the real
                  failure with a retry (round-4 review P3). */}
              {storeRows.length === 0 && externalModels.length === 0 && (
                storeState === 'loading' && !external ? (
                  <div className={`py-4 text-center text-[13px] ${MUTED_TEXT}`}>{copy.loading}</div>
                ) : externalFetchError ? (
                  <div
                    data-testid="device-manager-models-error"
                    className="flex flex-wrap items-center justify-center gap-2 py-4 text-center text-[13px] text-[#A15C07] dark:text-[#FDD663]"
                  >
                    <span className="min-w-0 break-words">{copy.refreshFailed(externalFetchError)}</span>
                    <MsButton tone="ghost" onClick={refreshExternal}>{copy.retry}</MsButton>
                  </div>
                ) : (
                  <div
                    data-testid="device-manager-models-empty"
                    className={`py-4 text-center text-[13px] leading-6 ${MUTED_TEXT}`}
                  >
                    {copy.ollamaOfflineHint}
                  </div>
                )
              )}
              {/* Store rows still render but the external read failed: the
                  empty-state branch above does not apply, and without a persistent
                  row the failure would only live in a 3.2s toast (round-5 review P3). */}
              {storeRows.length > 0 && externalModels.length === 0 && externalFetchError && (
                <div
                  data-testid="device-manager-external-error"
                  className="flex flex-wrap items-center justify-center gap-2 py-3 text-center text-[13px] text-[#A15C07] dark:text-[#FDD663]"
                >
                  <span className="min-w-0 break-words">{copy.refreshFailed(externalFetchError)}</span>
                  <MsButton tone="ghost" onClick={refreshExternal}>{copy.retry}</MsButton>
                </div>
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
                  aria-label={copy.doctorCustomPlaceholder}
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
