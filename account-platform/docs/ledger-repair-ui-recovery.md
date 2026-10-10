# Ledger repair draft recovery

The repair page reads the owner's frozen projection and creates an approval
**DRAFT**. Creating or recovering a draft never submits approval or executes a
financial repair. The existing approval detail remains the next explicit step.

Before the first create request, the page saves the actor, operation, complete
projection, reason and original operator key in session storage and verifies the
stored bytes. The local key is trimmed once and may contain up to 111 UTF-8
bytes. The `approval:` prefix adds nine bytes; the existing public approval key
limit remains 120 bytes. Valid Unicode keys remain supported.

An uncertain create result freezes these facts. Explicit retry uses the original
body and key after refresh or recent authentication, including a new client and
CSRF token. A subsequent 401, 403 or 409 does not discard an earlier uncertain
invocation. Another actor cannot replay it. An unavailable or damaged recovery
record blocks a new write. A first definite rejection with no earlier uncertain
write can release its intent.

Only a validated create response and successful comparison against the complete
original stored record clear the recovery intent. A current list or detail read
is not a create receipt. Comparing only the key is insufficient: a late response
must not clear a newer invocation using the same operator key with different
facts. A comparison or storage-clear failure retains evidence and blocks another
write. Responses from an unmounted view or a replaced client cannot navigate the
new actor's view.

The shared operation hook adds optional intent-construction and complete-record
clearing adapters. Existing account-closure and entitlement-correction callers
retain their original defaults. Browser verification uses built Portal assets
and controlled HTTP responses; it does not establish live identity-provider or
production financial execution coverage. Recovery cannot survive deletion of
the browser's session storage, and this storage does not grant permission.
