# Real Git and git-lfs, with all filters/hooks/objects scoped to this fixture.
$lfsRoot = Join-Path $testRoot 'lfs-identity'
New-Item -ItemType Directory -Path $lfsRoot -ErrorAction Stop | Out-Null
foreach ($arguments in @(
  @('init', '--quiet'), @('config', 'user.name', 'Runtime Fixture'),
  @('config', 'user.email', 'runtime-fixture@example.invalid'),
  @('lfs', 'install', '--local', '--skip-smudge')
)) { Invoke-Git -WorkingDirectory $lfsRoot -Arguments $arguments -FailureMessage 'Unable to initialize LFS identity fixture' | Out-Null }
[IO.File]::WriteAllText((Join-Path $lfsRoot '.gitattributes'), "*.bin filter=lfs diff=lfs merge=lfs -text`n", [Text.Encoding]::ASCII)
$lfsPayload = Join-Path $lfsRoot 'asset.bin'
[IO.File]::WriteAllText($lfsPayload, 'materialized runtime bytes', [Text.Encoding]::ASCII)
Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('add', '--', '.gitattributes', 'asset.bin') -FailureMessage 'Unable to clean LFS fixture' | Out-Null
Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=.no-hooks', 'commit', '--quiet', '-m', 'LFS identity fixture') -FailureMessage 'Unable to commit LFS fixture' | Out-Null
$pointer = Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('show', 'HEAD:asset.bin') -FailureMessage 'Unable to inspect LFS pointer'
Assert-True (($pointer -join "`n") -match 'version https://git-lfs.github.com/spec/v1') 'Fixture must commit a real LFS pointer, not payload bytes'
Assert-RuntimeTrackedFilesClean -Root $lfsRoot

# A configured-but-unavailable executable is an inspection error, not an
# instruction to commit the already-correct materialized payload.
Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('config', '--local', 'filter.lfs.process', 'pinvou-nonexistent-lfs-filter') -FailureMessage 'Unable to inject missing filter' | Out-Null
# Force a real content/filter comparison rather than Git's valid stat-cache
# shortcut. No sleep or assumed filesystem timestamp resolution is needed.
[IO.File]::SetLastWriteTimeUtc($lfsPayload, [DateTime]::UtcNow.AddYears(-2))
$diagnosis = ''
try { Assert-RuntimeTrackedFilesClean -Root $lfsRoot } catch { $diagnosis = $_.Exception.Message }
Assert-True ($diagnosis -match 'Unable to inspect' -and $diagnosis -match 'git-lfs' -and $diagnosis -notmatch 'commit and update') 'Missing Git filter must have an accurate static diagnostic'

# Absent clean configuration can yield ordinary diff exit 1 without stderr.
# It must still fail closed and direct the operator to filters before commits.
foreach ($key in @('filter.lfs.process', 'filter.lfs.clean')) {
  Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('config', '--local', $key, '') -FailureMessage 'Unable to inject absent filter' | Out-Null
}
[IO.File]::SetLastWriteTimeUtc($lfsPayload, [DateTime]::UtcNow.AddYears(-3))
$diagnosis = ''
try { Assert-RuntimeTrackedFilesClean -Root $lfsRoot } catch { $diagnosis = $_.Exception.Message }
Assert-True ($diagnosis -match 'Verify required Git filters') 'Missing clean configuration must not imply that payload edits are intentional'
Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('lfs', 'install', '--local', '--skip-smudge', '--force') -FailureMessage 'Unable to restore LFS fixture' | Out-Null
Assert-RuntimeTrackedFilesClean -Root $lfsRoot
[IO.File]::WriteAllText($lfsPayload, 'changed runtime bytes', [Text.Encoding]::ASCII)
$rejected = $false; try { Assert-RuntimeTrackedFilesClean -Root $lfsRoot } catch { $rejected = $true }
Assert-True $rejected 'Real LFS unstaged edits must be rejected'
Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('add', '--', 'asset.bin') -FailureMessage 'Unable to stage LFS edit' | Out-Null
$rejected = $false; try { Assert-RuntimeTrackedFilesClean -Root $lfsRoot } catch { $rejected = $true }
Assert-True $rejected 'Real LFS staged edits must be rejected'
[IO.File]::WriteAllText($lfsPayload, 'materialized runtime bytes', [Text.Encoding]::ASCII)
$rejected = $false; try { Assert-RuntimeTrackedFilesClean -Root $lfsRoot } catch { $rejected = $true }
Assert-True $rejected 'A clean LFS worktree must not conceal an edited index'
Invoke-Git -WorkingDirectory $lfsRoot -Arguments @('reset', '--quiet', 'HEAD', '--', 'asset.bin') -FailureMessage 'Unable to restore LFS index' | Out-Null
Assert-RuntimeTrackedFilesClean -Root $lfsRoot
Write-Host 'Real LFS pointer/materialized cleanliness, unavailable/absent filters, staged/unstaged edits and recovery passed.'
