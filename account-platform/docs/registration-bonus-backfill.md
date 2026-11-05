# Permanent registration eligibility backfill

Account prepares the eligibility set before accepting registration traffic. The
1000-row pages share one frozen historical time cutoff. This is an upper bound,
not a database snapshot spanning page transactions; startup preparation still
precedes registration traffic. Each committed PENDING page
stores the fingerprint key ID and a keyed verifier in the same transaction as its
facts and cursor. Resume requires both the same ID and the same key material;
changing the key under an unchanged ID is also rejected. Registration writers
check this binding while the backfill is still PENDING. Award eligibility and
PENDING/READY/BLOCKED registration modes retain their existing meaning.

The snapshot bounds both recoverable and unrecoverable historical counts. Rows
created or committed after it do not change those historical counts. Finalization
also rejects facts with another key ID. Published migration 000087 is unchanged;
the new, unmerged 900014 migration changes the checkpoint invariant and refuses a
destructive rollback after a key binding or permanent facts exist.

Unbound legacy progress or facts cannot safely be attributed to a key from its
ID alone. Preparation fails closed instead of blessing that evidence with the
current key. Do not erase the immutable facts or checkpoints to bypass the check;
restore consistent database/key evidence or use a separately reviewed forward
repair. There is no automatic key rotation or repair of mixed-key history.
