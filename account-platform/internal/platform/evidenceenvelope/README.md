# Evidence envelope boundary

`Seal` and `Open` implement only bounded per-file AES-256-GCM envelope encryption.
Each file gets a fresh 32-byte DEK and 12-byte nonce. The Context-aware existing
key wrapper receives only the DEK and authenticated metadata. It must authenticate
AAD, return owned buffers, honor cancellation, and support explicit historical
key IDs. No fallback to a current key or automatic rotation is provided.

The AAD schema `pinvou.evidence-envelope.v1` commits to purpose, owner UUID,
document UUID, immutable version, media type, plaintext size, SHA-256, and key ID.
Purposes are 1–128 ASCII letters/digits with `.`, `-`, `_` allowed after the first
character; key IDs use the same rule without `.`. Changing this encoding requires
an explicit compatibility design.

The caller must authorize the object and independently obtain the expected
`Binding` from its trusted owner record. Passing `envelope.Binding` directly to
`Open` does not establish authorization. Owner/document/version relationships and
the purpose are server-owned facts, never authority supplied by an upload client.
Even a valid envelope cannot detect replacement by an older valid version if the
caller also accepts that older binding. The owner must select the intended version.

This package accepts only PNG/JPEG/PDF media labels and 1 byte through 10 MiB of
plaintext. It does not decode or validate file content; the existing format
validator must run before storage. Persist every envelope field together. Bound
transport/JSON/base64 input before decoding; the in-memory checks cannot prevent
an upstream decoder from allocating an oversized body. Wrapped DEKs are limited
to 64 KiB, nonces to 12 bytes, and ciphertext to plaintext size plus 16 bytes.

Errors are the fixed `ErrInvalid`; wrapper errors are not exposed. DEK byte
buffers and failed decryption output are explicitly cleared; Go cipher internals
and garbage collector copies cannot be guaranteed erased. Callers must protect
and erase successfully returned plaintext according to their own lifetime policy.
Arguments and providers must not be concurrently mutated, and providers must be
configured non-nil objects implementing the wrapper contract.

Tests use a local authenticated wrapper to exercise encryption contracts and
failure controls. They do not establish live KMS connectivity or production IAM,
key retention, provider AAD enforcement, or historical-key availability; those
require separate integration verification. No HTTP, SQL, retention, UI, forced
refund workflow, or production key provisioning is included here.
