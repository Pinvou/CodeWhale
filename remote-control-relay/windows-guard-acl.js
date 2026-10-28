export const WINDOWS_ACL_VERIFY_SCRIPT = `
$ErrorActionPreference = 'Stop'
$target = $env:PINVOU_ACL_TARGET
$kind = $env:PINVOU_ACL_KIND
$current = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
$allowed = @($current.Value, 'S-1-5-18', 'S-1-5-32-544') | Microsoft.PowerShell.Utility\\Select-Object -Unique
$acl = Microsoft.PowerShell.Security\\Get-Acl -LiteralPath $target
if (-not $acl.AreAccessRulesProtected) { throw 'ACL inheritance enabled' }
if ($acl.Owner -ne $current.Value -and $acl.Owner -ne $current.Translate([System.Security.Principal.NTAccount]).Value) { throw 'unexpected ACL owner' }
$actual = @($acl.Access)
if ($actual.Count -ne $allowed.Count) { throw 'unexpected ACL entry count' }
foreach ($rule in $actual) {
  if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { throw 'deny ACL present' }
  $sid = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
  if ($allowed -notcontains $sid) { throw 'unexpected ACL principal' }
  if (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::FullControl) -ne [System.Security.AccessControl.FileSystemRights]::FullControl) { throw 'insufficient ACL rights' }
  if ($kind -eq 'directory' -and $rule.InheritanceFlags -ne [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit') { throw 'directory ACL inheritance mismatch' }
  if ($kind -eq 'file' -and $rule.InheritanceFlags -ne [System.Security.AccessControl.InheritanceFlags]::None) { throw 'file ACL inheritance mismatch' }
}
[Console]::Out.Write('ok')
`;

const INHERITANCE_CHECK = "if (-not $acl.AreAccessRulesProtected) { throw 'ACL inheritance enabled' }";
const OWNER_CHECK = "if ($acl.Owner -ne $current.Value -and $acl.Owner -ne $current.Translate([System.Security.Principal.NTAccount]).Value) { throw 'unexpected ACL owner' }";
const PRIVATE_OWNER_CHECK = `$owner = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
if ($allowed -notcontains $owner) { throw 'unexpected ACL owner' }`;

// Only the two reviewed checks may be relaxed for SQLite-owned inherited ACLs.
// A changed or duplicated template must fail closed instead of silently drifting.
export function buildGuardJournalAclScript(template = WINDOWS_ACL_VERIFY_SCRIPT) {
  if (typeof template !== "string" || template.split(INHERITANCE_CHECK).length !== 2
    || template.split(OWNER_CHECK).length !== 2) return null;
  return template.replace(INHERITANCE_CHECK, "").replace(OWNER_CHECK, PRIVATE_OWNER_CHECK);
}
