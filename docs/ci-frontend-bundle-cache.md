# Verified frontend bundle delivery

`frontend-setup` builds both frontend distributions once and publishes the tested
commit, compressed byte count and SHA-256 as job outputs. Consumers check their
actual checkout against that commit and verify the complete compressed bytes
before extracting an independent copy of the two distribution directories.

When `PINVOU_FRONTEND_CACHE_URL` is unset, the workflow uploads and retrieves the
normal current-run GitHub artifact. The shared downloader keeps API authorization
off storage redirects, resumes an interrupted immutable ZIP where supported and
uses a total transfer budget. HTTP status and transport failure remain separate.

Self-hosted operators can configure that variable as a loopback HTTP endpoint.
The host cache stores immutable objects addressed by SHA-256 and references bound
to the exact tested commit. The producer publishes to its local cache, which
replicates to configured peers over HTTPS with required client certificates.
Consumers receive verified local hits; a cold or corrupted host copy is refilled
from a peer. Publication without sufficient replicas selects the GitHub delivery
path instead. A LAN delivery selected by the producer must return the exact
published object; a missing or mismatched object is an error.
This fallback is a publication-time decision, not a consumer-time retry through
GitHub. LAN selection needs at least two successful copies; configure at least
two hosts with working peer replication. A single host still uses GitHub.

## Host service

Install `scripts/ci-frontend-cache-server.py` with `scripts/lib/ci_frontend_cache.py`
and `scripts/lib/ci_transfer_deadline.py`
under the same relative layout. Run the server as a dedicated service identity,
separate from runner accounts. Its configuration supplies `repository`, `root`,
`local_port`, `peer_bind`, `peer_port`, HTTPS `peers`, `ca`, `certificate` and
`private_key`. No deployment addresses or credentials belong in the repository.

The loopback interface accepts bounded content-addressed publication from local
jobs. Only peers holding an approved client certificate can use the LAN listener.
Certificates need server/client extended key usage, proper signing/key-usage
constraints, subject/authority key identifiers and SANs matching the configured
peer names or addresses. Keep certificate validation enabled.
Peer names resolve at startup, before request budgets begin; restart the service
to adopt DNS/address changes. A failed resolution fails startup explicitly.

Protect the data-root parent from runner writes. The service owns its data root,
and runners access it through the loopback API. The default budget is 20 GiB,
1000 references and seven days, with a 2 GiB free-space reserve and 128 MiB object
cap. Cleanup acts only on this dedicated cache. Existing job directories and
historical CI artifacts are not cache eviction targets.
Run exactly one service process per data root. Startup reclaims only its generated
staging names left by an interrupted previous process. Upload admission accounts
for concurrent reserved bytes and evicts owned cache data before declaring low
space. Other applications writing to the same filesystem can still exhaust space;
the reserve bounds this cache's admissions, not other applications' writes.

The unauthenticated loopback API is shared by jobs on the same host: directory
write protection is not per-job read isolation. All such jobs must be trusted to
read cached bundles and to affect cache availability/eviction. Do not use this
service for confidential artifacts across mutually untrusted runner accounts.
Peer handshakes and HTTP work share 16 admitted slots per listener. Requests beyond
that bound are disconnected; heavy bursts can fail even when capacity is healthy.
Object verification currently serializes under the storage/prune lock; these
bounded limits are not a throughput guarantee and should be load-tested before
increasing deployment concurrency.

Linux command budgets interrupt slow metadata, headers and body parsing as well
as idle reads. Service request/peer watchdogs interrupt their owned sockets on
absolute deadlines; expired transfers cannot publish a new object or job output.
The Linux slow-stream tests are the target-platform evidence; Windows unit-test
success does not prove asynchronous socket interruption on Windows.

Cache regression processes run inside the shared CI process supervisor and job
ledger; full test discovery runs them once and retirement remains a blocking
gate. Server test data uses a private generated directory in the physical test
workspace so the real 2 GiB reserve stays enabled. Normal fixture cleanup removes
it. A forcibly killed controller may leave that test-only directory until the
next workspace checkout cleanup; a process-retirement receipt does not prove
that every workspace file was removed.

Log records report source (`local-hit`, `peer-fill`, or `github`), verified commit,
digest, bytes and elapsed time. URLs, credentials and signed query strings are
excluded. Validate first publication, all-host local hits, peer cold-fill,
corruption recovery, wrong-commit rejection and rejection of missing peer client
certificates before enabling the repository variable. Validate an actual CI run
afterwards; a synthetic transport benchmark alone is not CI coverage.

Frontend logic and browser shards use the browser-capable heavy pool selected by
`PINVOU_LINUX_BROWSER_HEAVY_RUNNER`, with the existing heavy/general/hosted fallback
when that optional variable is absent. A capability label is assigned only after
the actual container can create an unprivileged private user/network namespace.
The namespace and resource-retirement gates remain mandatory.

## Checkout and test CPU costs

Bundle delivery timings exclude repository checkout and test execution. Logic
tests fetch only the tested commit. Browser shards fetch that commit and its
parents; a merge-queue base outside this shallow graph is fetched by its exact
immutable SHA. Selection still compares the tested merge or queue base/head,
and a missing or mismatched commit fails the gate. The checkout integration test
executes these workflow commands against PR and multi-commit queue fixtures.

The compatibility auditor indexes LF line starts once for marked source files.
AST suppression checks use this shared index instead of rescanning the source
prefix for every node, preserving the existing UTF-16/LF marker semantics and
all compatibility checks. More runner cores do not accelerate that serial scan.

Browser shard selection currently distributes smoke entries by count. An entry
that invokes a group of child smokes can therefore dominate one shard; inspect
individual smoke durations separately from cache and checkout timings.
