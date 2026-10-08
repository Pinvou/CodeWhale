// Semantic validation of signed reports. No filesystem, network, signing or deployment.
import {
  canonicalEvidenceJSON, evidenceRejected as reject, requiredEvidenceCases,
  observationStages, observationMetrics,
} from './account-platform-model-token-evidence-policy.mjs';

const time = value => {
  const result = Date.parse(value);
  if (!Number.isFinite(result)) reject('time_invalid');
  return result;
};
function window(startedAt, endedAt, now) {
  const start = time(startedAt); const end = time(endedAt);
  if (start >= end || end > now) reject('window_invalid');
  return { start, end };
}
function stableVersion(version) {
  if (typeof version !== 'string' || !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version)) reject('legacy_version_invalid');
  const parts = version.split('.').map(Number);
  if (parts.some(part => !Number.isSafeInteger(part))) reject('legacy_version_invalid');
  return parts;
}
function laterVersion(next, previous) {
  const n = stableVersion(next); const p = stableVersion(previous);
  for (let index = 0; index < 3; index += 1) {
    if (n[index] !== p[index]) return n[index] > p[index];
  }
  return false;
}

export function validateLegacyRetention(plan, legacy, now) {
  if (!legacy.compatibility_retained) reject('legacy_compatibility_required');
  if (plan.legacy_links.ga_at === null) {
    if (legacy.stable_releases.length !== 0) reject('legacy_ga_missing');
    return 'RETAIN_REQUIRED';
  }
  const ga = time(plan.legacy_links.ga_at);
  if (ga > now) reject('legacy_ga_future');
  stableVersion(plan.legacy_links.ga_version);
  let lastTime = ga; let lastVersion = plan.legacy_links.ga_version;
  for (const release of legacy.stable_releases) {
    const releasedAt = time(release.released_at);
    if (release.environment !== 'PRODUCTION' || release.channel !== 'STABLE'
      || releasedAt <= lastTime || releasedAt > now || !laterVersion(release.version, lastVersion)) reject('legacy_history_invalid');
    lastTime = releasedAt; lastVersion = release.version;
  }
  return now - ga >= plan.legacy_links.minimum_days * 86400000
    && legacy.stable_releases.length >= plan.legacy_links.minimum_stable_versions ? 'MINIMUM_MET_COMPATIBILITY_RETAINED' : 'RETAIN_REQUIRED';
}

export function evaluateObservation(plan, report, stage, now) {
  const stageIndex = observationStages.indexOf(stage);
  const observed = report.observation.stages;
  if (stageIndex < 0 || observed.length !== stageIndex + 1) reject('observation_coverage_invalid');
  let decision = 'EVIDENCE_VERIFIED'; let previousEnd = null;
  const reportWindow = window(report.started_at, report.ended_at, now);
  for (const [index, actual] of observed.entries()) {
    const expected = plan.stages[index];
    if (actual.stage !== expected.stage || actual.cohort !== expected.cohort
      || actual.traffic_percent !== expected.traffic_percent || actual.approval_reference !== expected.approval_reference) reject('observation_binding_invalid');
    const { start, end } = window(actual.started_at, actual.ended_at, now);
    if (start < reportWindow.start || end > reportWindow.end || (previousEnd !== null && start !== previousEnd)) reject('observation_order_invalid');
    previousEnd = end;
    if (!Number.isSafeInteger(actual.samples) || actual.samples < 0) reject('observation_samples_invalid');
    if ((actual.samples < expected.minimum_samples || end - start < expected.observation_seconds * 1000) && decision !== 'NO_GO') decision = 'HOLD';
    for (const metric of observationMetrics) {
      const value = actual.metrics[metric]; const threshold = expected.thresholds[metric];
      if (!Number.isFinite(value) || value < 0) reject('observation_metric_invalid');
      if (value > threshold.pause_above || (metric === 'unauthorized_effects' && value > 0)) decision = 'NO_GO';
      else if (value > threshold.advance_max && decision !== 'NO_GO') decision = 'HOLD';
    }
  }
  if (previousEnd !== reportWindow.end) reject('observation_freshness_invalid');
  return decision;
}

export function validateReportSemantics(report, envelope, plan, stage, now) {
  for (const field of ['candidate', 'environment', 'kind', 'status', 'started_at', 'ended_at', 'executor_reference', 'reviewer_reference', 'approval_reference']) {
    if (canonicalEvidenceJSON(report[field]) !== canonicalEvidenceJSON(envelope[field])) reject('report_binding_invalid');
  }
  if (report.validation_level !== 'TARGET_ENVIRONMENT') reject('report_level_invalid');
  if ((report.kind === 'OBSERVATION') !== (report.observation !== null)
    || (report.kind === 'LEGACY_LINKS') !== (report.legacy_links !== null)) reject('report_details_invalid');
  const required = requiredEvidenceCases[report.kind];
  const actual = new Set(report.cases.map(item => item.id));
  if (actual.size !== report.cases.length || actual.size !== required.length || required.some(id => !actual.has(id))) reject('case_coverage_invalid');
  const artifacts = new Set(report.artifacts.map(item => item.path));
  if (artifacts.size !== report.artifacts.length) reject('artifact_duplicate');
  const used = new Set();
  for (const item of report.cases) {
    for (const artifactPath of item.artifact_paths) {
      if (!artifacts.has(artifactPath)) reject('case_artifact_missing');
      used.add(artifactPath);
    }
  }
  if (used.size !== artifacts.size) reject('artifact_unreferenced');
  let decision = report.status === 'FAIL' || report.exit_code !== 0 || report.cases.some(item => item.status === 'FAIL') ? 'NO_GO'
    : report.cases.some(item => item.status === 'SKIP') ? 'HOLD' : 'EVIDENCE_VERIFIED';
  if (report.observation) {
    const observation = evaluateObservation(plan, report, stage, now);
    if (observation === 'NO_GO' || decision === 'NO_GO') decision = 'NO_GO';
    else if (observation === 'HOLD') decision = 'HOLD';
  }
  const retention = report.legacy_links ? validateLegacyRetention(plan, report.legacy_links, now) : null;
  return { decision, retention };
}
