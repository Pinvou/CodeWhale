# Controller lock protocol upgrade and offline downgrade

Opening the controller state store requires Node.js 24 or newer with the built-in
`node:sqlite` module available. This includes the enabled controller server, the
enrollment gateway and management commands that open the store. A missing runtime
fails with `relay_state_lock_runtime_unavailable` before preparing the directory
or reading authorization state. The disabled, legacy-only server does not load
SQLite, and help/configuration-only commands do not acquire a controller lock.
This does not change the whole package's `engines` requirement.

The extra OS-backed lock protects the lifetime of a store, including inspection
and recovery of the existing owner file. JSON primary/backup, namespace revisions,
enrollment credentials, revocations and the separate legacy authority retain their
existing format. The lock files are not an authorization database and are not
evidence that a staged JSON backup was committed. This change alone does not
repair a mismatched or missing JSON candidate (#1617 and #1578).

## Guard storage and recovery

`.pinvou-controller-state.guard` has application ID `0x50564c32` (`PVL2`), protocol
version 2, one SQLite page (a supported power-of-two page size from 512 to 65536
bytes), and no schema objects or business data. Its inode is never removed,
renamed or recreated during normal opening, stale-owner recovery or close.
Only an empty bootstrap file may initialize this header. SQLite commits that
header, then the store reacquires `BEGIN EXCLUSIVE` before inspecting the old
owner, reading JSON or delivering the store. No authorization operation occurs
in the interval between initialization COMMIT and the final EXCLUSIVE lock.
Initialized stores only read the guard format; they do not rewrite its header.
SQLite itself creates a missing guard; the application never opens an ordinary
bootstrap descriptor. The verified private parent protects the new inode while
its own permissions are tightened, before any owner or JSON operation.
If that opener exits before protection, a later opener may finish tightening only
an owned, single-link, zero-byte bootstrap with no journal in the verified private
directory. On POSIX it removes only default extra read bits from an owner-readable
and writable file; on Windows it first checks owner, allowed principals and rights
of the inherited ACL. Only for the empty bootstrap and exact SQLite journal,
the owner may be the running User, SYSTEM or Administrators, the same principals
already granted FullControl by the private directory. This is not proof of the
file's creator and does not admit any foreign ACL principal. It permits the same
User to recover after its elevation token changes; no same-token requirement is
imposed. Empty bootstrap protection then assigns the running User as owner.
The store revalidates the fixed identity and full private permissions; ready or
nonempty guards, JSON and owner files still require the strict User owner. Nonempty protocols, shared write/execute bits, foreign ownership,
unsafe identities and unknown sidecars are never repaired this way.

The exact `.pinvou-controller-state.guard-journal` is a registered default-DELETE
rollback sidecar during initialization or its recovery. This precise private
SQLite protocol family is subject to regular-file, single-link, owner, permission,
ancestor, legacy-alias and 64 KiB limits; unrelated journals, WAL and SHM are
foreign entries and remain rejected. On Windows a SQLite-created journal may
inherit only the verified private directory's permitted principals and rights;
its inherited ACL is checked without requiring SQLite to disable inheritance.
An orphan journal without its guard is rejected.

SQLite manages this non-authorizing protocol family's transaction recovery and
non-hot journal disposal. The application does not classify every journal byte,
manually delete journals, disable journaling or use MEMORY/WAL to evade recovery.
Recovery may change the guard or remove its own sidecar. Unknown guard protocols,
unsafe objects, library failures and unexpected journals in an initialized ready
store fail before JSON state is opened or mutated. This delegation does not relax
unknown owner-lock/JSON versions or promote any JSON backup. Initializing guard
metadata is separate from initializing the authorization JSON pair.

## Upgrade

1. Install Node.js 24 or newer on every server and management entry point that
   opens this directory. Check the actual service executable and its environment,
   not just an interactive shell's `node --version`.
2. Stop the server, gateway and management writers. Disable/cancel their automatic
   restart and scheduled launch sources, and wait for in-flight opens, mutations
   and stale-lock reclaimers to finish. Stopping one service does not stop another
   terminal, old process or gateway. Do not mix old and new writers.
3. While all writers remain stopped, privately archive the complete dedicated
   directory and its metadata, preserving primary, backup, marker, owner, temporary
   and stale files; retain the independent legacy state separately. Record the
   predecessor version and how quiescence was established. An ordinary live copy
   is not a coordinated authorization snapshot.
4. Install the new program without rewriting JSON or revision values. Start just
   one designated owner. Verify its authorized configuration, singleton startup,
   preserved state and the next legitimate commit, then close/restart it and
   verify the committed revision before restoring scheduled entry points.

The production and test deployment scripts stop the service before changing its
code. The first upgrade of an existing directory from a release without this lock
requires `PINVOU_CONFIRM_CONTROLLER_LOCK_UPGRADE=1`; set it only after completing
the independently launched writer and restart-source checks above. The scripts
retain a private offline controller-state archive. A failed stop must not be
treated as a successful backup or upgrade.

## Rollback after activation

Once the fixed `.pinvou-controller-state.guard` exists, an old release does not
understand the directory protocol. Returning only its code and restarting is not
a safe automatic rollback. Both deployment scripts keep the service stopped and
report `controller_lock_offline_downgrade_required` when their predecessor lacks
protocol support. The failed release and state remain available for investigation.
If the lock was never activated, or both releases support this protocol, the
ordinary code rollback remains available after stopping the current owner.

An administrator may deliberately downgrade offline:

1. Keep every new/old writer and launch source stopped and drain pending work,
   as for upgrade. Confirm no live or unknown owner remains. Do not remove a lock
   to bypass an unexplained owner or failed stop.
2. Privately archive the entire directory and predecessor/new program versions,
   including all lock artifacts and their identity/permissions. Do not edit,
   restore or replace primary/backup, namespace revisions or legacy revocations
   merely to make the older program start.
3. Verify that the new guard protocol and its recovery artifacts are understood
   and cleanly closed. Unknown formats, unsafe identities, incomplete recovery or
   an unexplained journal require investigation while stopped. Never treat an
   unknown sidecar as disposable. Move only the verified new lock artifacts into
   the private archive outside the dedicated directory; never do this while any
   owner, opener, worker or automatic restart can still run.
4. Restore the known predecessor program and its runtime/configuration. Permit
   one owner to start, verify its normal state and next commit/restart, and only
   then restore the intended entry points. If it refuses state, stop and retain
   the evidence instead of clearing or reinitializing authorization data.

Downgrading reintroduces #2151's old stale-reclamation limitation. It is an explicit
temporary operational choice, not continued protection by the new protocol.
An upgrade or downgrade does not satisfy the separate release-security gate.

## In-process lock-file access

Never ordinarily open, read, hash or copy the fixed guard inode in a process that
may hold a SQLite connection to it, including from another worker or a failed
contender. POSIX closing such an ordinary descriptor can release that process's
SQLite file locks. Production checks use metadata and SQLite APIs instead.
Offline archival above is valid only after all owners have stopped; a future
maintenance tool operating under a live guard must use a separately reviewed
archive protocol and must not replace the fixed guard inode.

Management commands reject input/output within the dedicated controller directory,
the independent legacy state path, reserved guard/sidecar names and existing
aliases of managed files. Keep private operator inputs and outputs outside those
paths. Commands read inputs before opening their store and close it successfully
before publishing output or reporting success. A cleanup failure is reported as
failure even if the requested operation already committed; resolve it and use the
operation's existing replay contract rather than assuming it did not execute.

Local Windows concurrency tests use the existing injected filesystem-security
adapters and are separate from native DACL qualification. Linux and Windows CI
must exercise their applicable guard regressions; one platform's API experiment
does not establish the other platform or deployment qualification.

The rollout helper stops only its supplied service unit. Before every upgrade,
including upgrades from an already recognized lock protocol, the operator must
stop all other writers sharing the state directory and their restart sources.
The helper's archive is an offline archive only after that independent check.

On Windows, each guarded write rechecks the fixed guard's ACL. A normal persist
writes backup and primary, adding two guard-verification PowerShell launches to
the existing protection/verification work. This is not the total process count
of a CAS. Windows latency has not been measured here and depends on the host;
Linux timing cannot substitute for it. These checks retain the identity fence.

The lock-rollout tests resolve Bash from the active Git for Windows installation
instead of assuming a drive. For a custom installation, set `PINVOU_TEST_BASH`
to an existing absolute Bash executable before running those tests. This is a
test-only setting, not a deployment or service configuration option.
