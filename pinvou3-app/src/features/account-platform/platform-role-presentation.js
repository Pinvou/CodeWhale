function labelFor(labels, key) { return typeof labels[key] === 'string' ? labels[key] : key; }
// This chooses the existing exact lookup endpoint; the server still validates
// the checksum, current actor and lookup limits.
export function looksLikeUserCode(value) { return /^USR[0123456789ABCDEFGHJKMNPQRSTVWXYZ]{18}$/.test(value.trim().replaceAll('-', '').toUpperCase()); }
export function platformRoleName(words, code) { return labelFor(words.names, code); }
export function platformRoleDescription(words, role) { return typeof words.descriptions[role?.code] === 'string' ? words.descriptions[role.code] : typeof role?.description === 'string' ? role.description : ''; }

export function platformPermissionGroups(words, permissions) {
  const groups = new Map();
  for (const code of permissions) {
    const domain = /billing|payment|pricing|product|wallet|usage|quota/.test(code) ? 'finance'
      : /token|model|credential|route|provision/.test(code) ? 'models'
        : /audit|system|event|approval|security/.test(code) ? 'governance' : 'identity';
    const parts = code.split('.'), action = parts[parts.length - 1];
    const resource = parts.slice(1, -1).map(part => labelFor(words.permissionTokens, part).replaceAll('-', ' ')).join(' / ');
    const label = resource ? `${labelFor(words.permissionActions, action)} · ${resource}` : labelFor(words.permissionActions, action);
    if (!groups.has(domain)) groups.set(domain, []);
    groups.get(domain).push({ code, label });
  }
  return ['identity', 'finance', 'models', 'governance'].filter(domain => groups.has(domain))
    .map(domain => ({ domain, label: words.navGroups[domain], items: groups.get(domain).sort((a, b) => a.code.localeCompare(b.code)) }));
}
