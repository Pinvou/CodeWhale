import { organizationID, organizationVersion } from './organization-client.js';

const invalid = () => { throw new Error('INVALID_ORGANIZATION_AUTHORIZATION'); };
const permissions = value => Array.isArray(value) && value.every(item => typeof item === 'string' && item.startsWith('org.'));

// A unit binding is never folded into an organization permission collection.
// An authority read belongs to one already displayed actor/context/version.
export function requireOrganizationAuthorization(value, me) {
 const active = me?.active_context;
 try {
  organizationID(value?.user_id); organizationID(value.organization_id);
  organizationVersion(value.context_version); organizationVersion(value.organization_authorization_version);
  if (value.user_id !== me.user_id || value.context_type !== 'ORGANIZATION' || active?.context_type !== 'ORGANIZATION' || value.organization_id !== active.organization_id || value.context_version !== active.context_version || value.organization_authorization_version !== active.organization_authorization_version || typeof value.session_id !== 'string' || !value.session_id || !permissions(value.permissions) || !Array.isArray(value.unit_authorization_bindings)) invalid();
  const keys = new Set();
  for (const binding of value.unit_authorization_bindings) {
   organizationID(binding?.unit_id);
   if (typeof binding.role !== 'string' || !binding.role || !permissions(binding.permissions) || keys.has(`${binding.unit_id}:${binding.role}`)) invalid();
   keys.add(`${binding.unit_id}:${binding.role}`);
  }
 } catch { invalid(); }
 return value;
}

export function organizationScopeRoots(context) {
 return [...new Set((context?.unit_authorization_bindings || []).filter(binding => binding.permissions.includes('org.units.read') && (binding.permissions.includes('org.units.manage') || binding.permissions.includes('org.members.read'))).map(binding => binding.unit_id))];
}

export function hasUnitScopePermission(context, root, permission) {
 return Boolean(root) && (context?.unit_authorization_bindings || []).some(binding => binding.unit_id === root && binding.permissions.includes(permission));
}

// Prove every selectable object is in the selected tree. A broken parent chain,
// duplicate node or an unrelated response cannot supply mutation targets.
export function scopedUnitIDs(tree, root) {
 const nodes = new Map();
 for (const unit of tree?.items || []) {
  if (nodes.has(unit.unit_id) || unit.status !== 'ACTIVE') invalid();
  nodes.set(unit.unit_id, unit);
 }
 if (!nodes.has(root)) invalid();
 for (const id of nodes.keys()) {
  let current = id; const seen = new Set();
  while (current !== root) {
   if (seen.has(current) || !nodes.has(current)) invalid();
   seen.add(current); current = nodes.get(current).parent_id;
  }
 }
 return new Set(nodes.keys());
}

export function memberWithinUnitScope(member, ids) {
 return member?.status === 'ACTIVE' && Array.isArray(member.unit_ids) && member.unit_ids.length > 0 && member.unit_ids.every(id => ids.has(id));
}

export function requireScopedMemberMutation(context, root, ids, member, nextUnits = member?.unit_ids) {
 if (!hasUnitScopePermission(context, root, 'org.members.manage') || !memberWithinUnitScope(member, ids) || !Array.isArray(nextUnits) || !nextUnits.every(id => ids.has(id))) invalid();
 return member;
}
