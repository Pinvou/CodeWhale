# Ordinary organization member units

Ordinary `member.unit_ids` are independent of the units attached to a
`UNIT_ADMIN` role. Creating or changing that role never assigns ordinary units.

The authenticated operation is `PUT
/v1/organizations/{organization_id}/members/{user_id}/units`; the platform
operation is `PUT
/admin/v1/organizations/{organization_id}/members/{user_id}/units`. Both require
an explicit `unit_ids` array, positive `expected_version` of the membership, and
an `Idempotency-Key` (8–120 characters; the JSON `idempotency_key` is also
accepted). UUIDs must identify active units in this organization. Explicit `[]`
removes ordinary attachments. Omission or null is invalid for replacement.

The organization operation uses existing `org.members.manage`. A scoped actor
must cover every old and new unit, and cannot acquire an unscoped target by
assigning it into the actor's subtree. The platform operation uses existing
`platform.organizations.members.manage`; the repository independently checks
the current actor and assignment. The owner role and role-binding scopes are
unchanged by ordinary attachment replacement.

Successful responses are HTTP 200 with `organization_id`, `member`, and
`replayed`. `member` is the current public member projection. New mutations
advance the membership's local version once and publish the existing
`account.membership.changed.v1` lifecycle event. The internal immutable command
receipt stores the affected scope; it is not exposed in the public response.
The same actor, organization, key, target, expected version and normalized units
can replay without another audit or event. Replays require current authority
over the original affected units; changed input conflicts. Later member
changes may appear in a successfully replayed current projection, including a
subsequent DISABLED or REVOKED state. The handler propagates that current status
and version to downstream lifecycle projections; it does not reactivate the
member on replay. A fresh mutation ACK must still be ACTIVE with exact advanced
version and units.

The HTTP handler also applies the existing Billing member-lifecycle projection.
A local commit with an unconfirmed projection returns HTTP 503
`ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE`; retry the same immutable request
and key. A client must not treat this response as a rollback or allocate a new
command key. Malformed input is 400, insufficient authority is 403, and stale
membership versions or changed same-key input are 409.

Admin member creation accepts optional `unit_ids` separately from
`unit_admin_unit_ids`. Omission preserves the previous unscoped behavior and
original request fingerprint. Explicit units initialize a new or reactivated
membership; they do not silently replace attachments on an already-active
member. Use the replacement operation for that change.

Invitations also accept optional `unit_ids`. Omission preserves the original
ordinary-member invitation behavior; explicit `[]` freezes an unscoped intent.
An explicit unit set is bound into the invitation request digest and cannot be
updated. Acceptance rechecks the current unit set and inviter's authority before
creating the member attachment. A scoped member manager can invite `MEMBER`
only. Any newly granted frozen non-`MEMBER` role additionally requires the
inviter's current organization-wide `org.roles.assign`; permission revocation
or expiry denies that new grant. An already-accepted replay grants nothing new.

Account migration 900003 adds nullable invitation scope and the closed
`UNITS_REPLACE` command type. Its normal public down operation fails closed to
preserve invitation and command history. Existing migrations remain unchanged.
Portal CSRF/session and Native bearer paths expose only these exact PUT routes;
ordinary attachments do not introduce gift, personal closure, or reclaim APIs.
