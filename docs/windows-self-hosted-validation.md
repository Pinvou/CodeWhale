# Windows self-hosted runner validation

`Windows Self-hosted Runner Validation` targets a repository runner carrying
`self-hosted`, `Windows`, `X64`, and a unique Windows instance label. Run it
manually after merging, for example after a host restart, tool upgrade, or pool
quota change. It does not run on pushes or pull requests. Occupied instances
finish their current jobs before accepting these maintenance checks.

The matrix verifies all fourteen instances: non-administrator accounts, D-drive
work/cache/temp paths, native Windows file locking and process-resource limits,
authenticated checkout, Node 24 setup, version consistency, and existing
application contracts. Each job records its actual runner, pool, commit, limits,
and paths. Account isolation opens the other runner's existing `.env` in its
declared installation directory without reading its contents; no manually
created probe file is required. The peer is selected through an explicit
administrator-maintained identity/path
mapping. A missing declaration or an unverifiable path fails the check.

Provide these values in each runner process's environment through its existing
host provisioning mechanism:

| Variable | Required meaning |
| --- | --- |
| `CI_RUNNER_INSTANCE_ROOT` | This runner's absolute D-drive installation directory, directly containing `.runner` and `.env`. |
| `CI_RUNNER_PEER_ROOT` | Another registered runner's absolute D-drive installation directory, directly containing its `.runner` and `.env`. |
| `CI_RUNNER_PEER_NAME` | The other runner's registered name from the administrator's mapping. It must differ from the current `RUNNER_NAME`. |

For these variables, the installation directory is the directory holding the
GitHub runner configuration files. An outer instance directory may have any
layout; its cache and shared-tool paths do not establish the installation path.
Keep each peer name paired with its actual installation directory. The workflow
does not configure the host or supply a fallback peer; runners without this
mapping fail maintenance validation until the administrator provides it.

The original non-administrator, D-drive, file-lock, Job Object and shared-tool
checks run before checkout. After checkout, the workflow runs the peer helper's
behavior tests and the same production helper before Node setup or application
contracts. Missing tests, an empty selection, skipped tests or expected failures
do not pass that validation. The helper checks the current `.runner` JSON's
`agentName` against the runner-provided `RUNNER_NAME`. It normalizes case,
separators and dot segments, then rejects identical names, overlapping roots
and physical aliases after strict native path resolution. Both real directories
and the peer's existing regular `.runner`
and `.env` files must have verifiable metadata; peer configuration reparse
points are rejected. It then opens a read handle without reading any `.env`
contents. Native `CreateFileW`/`CloseHandle` calls preserve the Windows error
code; only `ERROR_ACCESS_DENIED` (5) from that open establishes the expected
read-ACL denial.
A sharing violation, missing file, unexpected error or readable handle fails.
If directory or file ACLs also hide the required metadata, validation remains
blocked rather than treating that uncertainty as isolation evidence.

The Python behavior tests also run on Linux with controlled Windows filesystem
outcomes. They cover path normalization, identity checks, aliases, missing files,
error handling and the absence of `.env` reads. They do not establish native
Windows path resolution or ACL behavior; a successful native run of this manual
workflow is still required. This change has not been tested on a native Windows
runner.

`Windows Artifact Storage Validation` is a separate, manually dispatched
workflow. It targets the configured Windows light pool, falling back to
`PINVOU_WINDOWS_RUNNER` and then `windows-latest`. Its small upload probe is
retained for one day. Upload failures, including an exhausted GitHub storage
quota, remain visible in that workflow and do not turn the runner-health
workflow red. It is not part of the PR required gate and is never triggered by
pushes or pull requests.

| Pool | Instances | CPU budget per instance | Aggregate memory ceiling per instance |
| --- | ---: | ---: | ---: |
| `pinvou3-windows-heavy` | 2 | 25% (about 24 logical CPUs) | 96 GiB |
| `pinvou3-windows-medium` | 2 | 6.25% (about 6 logical CPUs) | 32 GiB |
| `pinvou3-windows-light` | 10 | 2.08% (about 2 logical CPUs) | 8 GiB |

These budgets target a 96-logical-CPU host. Windows Job Objects apply a CPU hard
cap, aggregate memory ceiling, and child-process cleanup. Compiler parallelism
also follows the pool budget. Independent accounts and writable directories
separate the workers; tools are shared read-only. These controls are not a
replacement for disposable isolation when running untrusted code.

Provision tools and writable CI data under a dedicated D-drive root. Start the
runner and its required network connection automatically using a dedicated
non-administrator account. The configured startup tasks preserve each pool's
identity and native resource limits. The runner listener uses its bundled
runtime; SDKs and native build tools are provisioned separately. Preserve
certificate validation and keep
proxy configuration, credentials, keys, and enterprise certificates on the host.

This validation does not change required-check routing or enable Windows/macOS
product jobs. It remains available during maintenance, independently of the
product platform switches. Full native Rust builds and releases retain their
separate SDK, compiler, signing, and build acceptance. Fork pull requests
must retain isolated execution rather than being routed onto this persistent
host.
Passing this manual host check does not prove that repository or organization
runner groups prevent untrusted fork jobs from reaching persistent runners.
Those access restrictions and workflow routing require separate evidence.
