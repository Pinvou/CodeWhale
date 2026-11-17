// A line is one base64 UTF-8 path, never PowerShell source. Base64 has no line
// separators, so Unicode, quotes and literal metacharacters round-trip without
// loading a JSON PowerShell module in the security-critical child process.
export function encodeWindowsAncestorPaths(paths) {
  if (!Array.isArray(paths) || paths.length === 0
    || paths.some(path => typeof path !== 'string' || !path || path.includes('\0'))) {
    throw new TypeError('Windows ancestor paths must be nonempty strings');
  }
  return paths.map(path => Buffer.from(path, 'utf8').toString('base64')).join('\n');
}

export const WINDOWS_REPARSE_VERIFY_SCRIPT = `
$ErrorActionPreference = 'Stop'
if ($env:PINVOU_ANCESTOR_DIAGNOSTIC -eq '1') { [Console]::Out.WriteLine('pinvou_ancestor_start') }
$encodedPaths = $env:PINVOU_PATHS_B64_LINES.Split([char]10)
if ($encodedPaths.Length -eq 0) { throw 'empty ancestor list' }
if ($env:PINVOU_ANCESTOR_DIAGNOSTIC -eq '1') { [Console]::Out.WriteLine('pinvou_ancestor_payload_decoded') }
$paths = @(
  foreach ($encoded in $encodedPaths) {
    if ($encoded.Length -eq 0) { throw 'empty ancestor path' }
    [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded))
  }
)
if ($env:PINVOU_ANCESTOR_DIAGNOSTIC -eq '1') { [Console]::Out.WriteLine('pinvou_ancestor_decoded') }
foreach ($target in $paths) {
  $attributes = [System.IO.File]::GetAttributes([string]$target)
  if (($attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'reparse point rejected' }
}
if ($env:PINVOU_ANCESTOR_DIAGNOSTIC -eq '1') { [Console]::Out.WriteLine('pinvou_ancestor_checked') }
[Console]::Out.Write('ok')
`;
