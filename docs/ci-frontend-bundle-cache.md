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

## Host service

Install `scripts/ci-frontend-cache-server.py` with `scripts/lib/ci_frontend_cache.py`
under the same relative layout. Run the server as a dedicated service identity,
separate from runner accounts. Its configuration supplies `repository`, `root`,
`local_port`, `peer_bind`, `peer_port`, HTTPS `peers`, `ca`, `certificate` and
`private_key`. No deployment addresses or credentials belong in the repository.

The loopback interface accepts bounded content-addressed publication from local
jobs. Only peers holding an approved client certificate can use the LAN listener.
Certificates need server/client extended key usage, proper signing/key-usage
constraints, subject/authority key identifiers and SANs matching the configured
peer names or addresses. Keep certificate validation enabled.

Protect the data-root parent from runner writes. The service owns its data root,
and runners access it through the loopback API. The default budget is 20 GiB,
1000 references and seven days, with a 2 GiB free-space reserve and 128 MiB object
cap. Cleanup acts only on this dedicated cache. Existing job directories and
historical CI artifacts are not cache eviction targets.

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
