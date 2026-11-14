import { organizationID, organizationVersion } from './organization-client.js';

const ROOT_TARGET = '@root';
const states = new Set(['ACTIVE', 'DISABLED', 'DELETED']);
// Quote literal separator characters and escape quoted names so a single unit
// cannot be mistaken for two levels in the human-readable path.
const pathSegment = ({ name, id }) => {
  // Legal historical names can be invisible in the control, but their
  // validated key and parent still identify this branch of the tree.
  if (!name.replace(/\p{Default_Ignorable_Code_Point}/gu, '').trim()) return '[' + id + ']';
  if (/\p{Default_Ignorable_Code_Point}/u.test(name)) {
    const quoted = JSON.stringify(name).replace(/[\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/g, char => '\\u' + char.charCodeAt(0).toString(16).padStart(4, '0'));
    return quoted + ' [' + id + ']';
  }
  return name.includes('[') || name.includes(']') || /[›"\\\u0000-\u001f\u007f]/.test(name) ? JSON.stringify(name) : name;
};

// The complete admin UnitTree emits an explicit null parent for a root unit.
// Missing or unresolvable parent keys never become a root or a selectable path.
export function organizationUnitMovePlan(organization, detail, unitID, rootLabel) {
  if (detail?.summary?.organization_id !== organization?.organization_id || detail?.unitsError !== null || !Array.isArray(detail?.units?.items)) return null;
  let organizationKey, structureVersion;
  const units = new Map(), children = new Map();
  try {
    organizationKey = organizationID(organization.organization_id);
    structureVersion = organizationVersion(detail.units.structure_version);
    for (const unit of detail.units.items) {
      const id = organizationID(unit.unit_id);
      if (units.has(id) || unit.organization_id !== organizationKey || typeof unit.name !== 'string' || unit.name.length === 0 || !states.has(unit.status)) return null;
      const parent = unit.parent_id === null ? null : organizationID(unit.parent_id);
      units.set(id, { ...unit, id, parent });
      if (parent !== null) {
        if (!children.has(parent)) children.set(parent, []);
        children.get(parent).push(id);
      }
    }
  } catch { return null; }
  const source = units.get(unitID);
  if (!source) return null;
  const labels = new Map([[null, rootLabel]]);
  const optionLabels = new Map([[null, rootLabel]]);
  const pathFor = id => {
    const chain = [], seen = new Set();
    let cursor = id;
    while (!labels.has(cursor)) {
      const unit = units.get(cursor);
      if (!unit || seen.has(cursor)) { for (const item of chain) { labels.set(item.id, null); optionLabels.set(item.id, null); } return null; }
      seen.add(cursor); chain.push(unit); cursor = unit.parent;
    }
    let path = labels.get(cursor), optionPath = optionLabels.get(cursor);
    while (chain.length) {
      const unit = chain.pop(), part = pathSegment(unit);
      path = path === null ? null : path + ' › ' + part;
      // A native option cannot contain bidi-isolated elements.
      optionPath = optionPath === null ? null : optionPath + ' › \u2068' + part + '\u2069';
      labels.set(unit.id, path); optionLabels.set(unit.id, optionPath);
    }
    return path;
  };
  const path = pathFor(unitID), forbidden = new Set();
  const segmentsFor = id => {
    const parts = [];
    for (let item = units.get(id); item; item = units.get(item.parent)) parts.push({ id: item.id, label: pathSegment(item) });
    parts.push({ id: null, label: rootLabel });
    return parts.reverse();
  };
  const segments = path === null ? null : segmentsFor(unitID);
  const pending = [unitID];
  while (pending.length) {
    const id = pending.pop();
    if (forbidden.has(id)) continue;
    forbidden.add(id);
    for (const child of children.get(id) || []) pending.push(child);
  }
  const active = organization.status === 'ACTIVE' && source.status === 'ACTIVE' && path !== null;
  const reachable = [...units.values()].filter(item => active && item.status === 'ACTIVE' && !forbidden.has(item.id)).flatMap(item =>
    pathFor(item.id) === null ? [] : [{ value: item.id, label: optionLabels.get(item.id) }]);
  // This tree does not declare the active database's name equivalence.
  // Exact duplicates prove a conflict; ASCII case variants are advisory.
  const foldAscii = name => name.replace(/[A-Z]/g, letter => letter.toLowerCase());
  const foldedSourceName = foldAscii(source.name);
  const conflicts = new Set(), possibleConflicts = new Set();
  for (const item of units.values()) {
    if (item.id === source.id || item.status === 'DELETED') continue;
    if (item.name === source.name) conflicts.add(item.parent);
    else if (foldAscii(item.name) === foldedSourceName) possibleConflicts.add(item.parent);
  }
  const eligibility = parent => ({ conflict: conflicts.has(parent), possibleConflict: !conflicts.has(parent) && possibleConflicts.has(parent) });
  const options = active ? [
    { value: ROOT_TARGET, label: rootLabel, ...eligibility(null) },
    ...reachable.map(item => ({ ...item, label: item.label + ' · ' + item.value, ...eligibility(item.value) }))
      .sort((a, b) => a.label.localeCompare(b.label) || a.value.localeCompare(b.value)),
  ] : [];
  const allowed = new Map(options.map(item => [item.value, item]));
  const hasAlternative = options.some(item => !item.conflict && (item.value === ROOT_TARGET ? null : item.value) !== source.parent);
  // AdminUnits reads the unpaged tree. Missing ancestors or cycles mean the
  // loaded rows cannot prove a complete impact count for this source.
  let impact;
  const descendantImpact = () => {
    if (impact) return impact;
    const rooted = new Set(), queue = [...units.values()].filter(unit => unit.parent === null).map(unit => unit.id);
    while (queue.length) {
      const id = queue.pop();
      if (rooted.has(id)) continue;
      rooted.add(id);
      for (const child of children.get(id) || []) queue.push(child);
    }
    const known = [...forbidden].filter(id => id !== source.id && units.get(id)?.status !== 'DELETED').length;
    impact = { known, complete: rooted.size === units.size };
    return impact;
  };
  const reason = value => {
    if (!active) return 'moveUnavailable';
    if (!value) return hasAlternative ? 'moveChooseTarget' : 'moveNoTargets';
    if (!allowed.has(value)) return 'moveInvalidTarget';
    if ((value === ROOT_TARGET ? null : value) === source.parent) return 'moveNoChange';
    if (allowed.get(value).conflict) return 'moveNameConflict';
    return '';
  };
  return { path, segments, currentParent: source.parent, sourceID: source.id, context: JSON.stringify([organizationKey, source.id, source.parent, structureVersion]), options, reason, preview(value) {
    if (reason(value)) return null;
    const target = value === ROOT_TARGET ? null : value;
    const parentPath = pathFor(target);
    if (parentPath === null) return null;
    return {
      path: parentPath + ' › ' + pathSegment(source),
      segments: [...segmentsFor(target), { id: source.id, label: pathSegment(source) }],
      targetID: target,
      impact: descendantImpact(),
    };
  }, prepare(value) {
    if (reason(value)) throw Error('INVALID_ORGANIZATION_UNIT_MOVE');
    return { parent_id: value === ROOT_TARGET ? null : value, expected_structure_version: structureVersion };
  } };
}
