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
and paths. Account isolation opens the other runner's existing `runner/.env`
without reading its contents; no manually created probe file is required. A
missing peer configuration or another unexpected filesystem error fails the
check instead of being treated as proof of isolation.

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
