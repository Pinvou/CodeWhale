import assert from "node:assert/strict";
import test from "node:test";
import { transition } from "../src/state-machine.js";

const ids = {
  initiator: "00000000-0000-4000-8000-000000000001",
  owner: "00000000-0000-4000-8000-000000000002",
  collaborator: "00000000-0000-4000-8000-000000000003",
  replacement: "00000000-0000-4000-8000-000000000004",
};

function assignedThread(patch = {}) {
  return {
    phase: "active",
    initiator_id: ids.initiator,
    owner_id: ids.owner,
    current_actor_id: ids.owner,
    return_to_id: ids.initiator,
    final_recipient_id: ids.initiator,
    pending_transfer_kind: null,
    pending_sender_id: null,
    pending_recipient_id: null,
    ...patch,
  };
}

function withoutAudit(next) {
  const { addParticipantId, removeParticipantId, grantMaterialAccessId, transferMode, ...state } = next;
  return state;
}

test("a new assignment remains with its sender until the recipient accepts", () => {
  const pending = assignedThread({
    phase: "pending_transfer",
    owner_id: ids.initiator,
    current_actor_id: ids.initiator,
    pending_transfer_kind: "assignment",
    pending_sender_id: ids.initiator,
    pending_recipient_id: ids.owner,
  });
  const accepted = transition(pending, ids.owner, "accept_transfer", null);
  assert.deepEqual(withoutAudit(accepted), {
    recipientId: ids.initiator,
    actionReturnToId: null,
    ownerId: ids.owner,
    currentActorId: ids.owner,
    threadReturnToId: ids.initiator,
    phase: "active",
    pendingTransferKind: null,
    pendingSenderId: null,
    pendingRecipientId: null,
  });
  assert.equal(accepted.grantMaterialAccessId, ids.owner);
});

test("declining an assignment leaves it ready for rerouting", () => {
  const pending = assignedThread({
    phase: "pending_transfer",
    owner_id: ids.initiator,
    current_actor_id: ids.initiator,
    pending_transfer_kind: "assignment",
    pending_sender_id: ids.initiator,
    pending_recipient_id: ids.owner,
  });
  const declined = transition(pending, ids.owner, "decline_transfer", null);
  assert.equal(declined.phase, "awaiting_reassignment");
  assert.equal(declined.ownerId, ids.initiator);
  assert.equal(declined.recipientId, ids.initiator);
  assert.equal(declined.removeParticipantId, ids.owner);
  const rerouted = transition({
    ...pending,
    phase: "awaiting_reassignment",
    pending_transfer_kind: null,
    pending_sender_id: null,
    pending_recipient_id: null,
  }, ids.initiator, "reroute_transfer", ids.replacement);
  assert.equal(rerouted.pendingTransferKind, "assignment");
  assert.equal(rerouted.pendingRecipientId, ids.replacement);
});

test("handoff is an invitation and only moves the relay after acceptance", () => {
  const invited = transition(assignedThread(), ids.owner, "handoff", ids.collaborator);
  assert.equal(invited.phase, "pending_transfer");
  assert.equal(invited.currentActorId, ids.owner);
  assert.equal(invited.ownerId, ids.owner);
  assert.equal(invited.pendingRecipientId, ids.collaborator);
  const accepted = transition({
    ...assignedThread(),
    phase: invited.phase,
    pending_transfer_kind: invited.pendingTransferKind,
    pending_sender_id: invited.pendingSenderId,
    pending_recipient_id: invited.pendingRecipientId,
  }, ids.collaborator, "accept_transfer", null);
  assert.equal(accepted.phase, "active");
  assert.equal(accepted.currentActorId, ids.collaborator);
  assert.equal(accepted.ownerId, ids.owner);
  assert.equal(accepted.threadReturnToId, ids.owner);
});

test("protocol 0.1 keeps direct handoff behavior during rolling upgrades", () => {
  const handedOff = transition(assignedThread(), ids.owner, "handoff", ids.collaborator, false);
  assert.equal(handedOff.phase, "active");
  assert.equal(handedOff.currentActorId, ids.collaborator);
  assert.equal(handedOff.pendingRecipientId, null);
  assert.equal(handedOff.grantMaterialAccessId, ids.collaborator);
  assert.equal(handedOff.transferMode, "direct_legacy");
});

test("rerouting to a legacy recipient applies the transfer directly", () => {
  const pending = assignedThread({
    phase: "pending_transfer",
    owner_id: ids.initiator,
    current_actor_id: ids.initiator,
    pending_transfer_kind: "assignment",
    pending_sender_id: ids.initiator,
    pending_recipient_id: ids.owner,
  });
  const rerouted = transition(pending, ids.initiator, "reroute_transfer", ids.replacement, false);
  assert.equal(rerouted.phase, "active");
  assert.equal(rerouted.ownerId, ids.replacement);
  assert.equal(rerouted.currentActorId, ids.replacement);
  assert.equal(rerouted.pendingRecipientId, null);
  assert.equal(rerouted.removeParticipantId, ids.owner);
  assert.equal(rerouted.grantMaterialAccessId, ids.replacement);
  assert.equal(rerouted.transferMode, "direct_legacy");
});

test("a handoff sender can withdraw or reroute before acceptance", () => {
  const pending = assignedThread({
    phase: "pending_transfer",
    pending_transfer_kind: "handoff",
    pending_sender_id: ids.owner,
    pending_recipient_id: ids.collaborator,
  });
  const withdrawn = transition(pending, ids.owner, "withdraw_transfer", null);
  assert.equal(withdrawn.phase, "active");
  assert.equal(withdrawn.currentActorId, ids.owner);
  assert.equal(withdrawn.removeParticipantId, ids.collaborator);
  const rerouted = transition(pending, ids.owner, "reroute_transfer", ids.replacement);
  assert.equal(rerouted.pendingRecipientId, ids.replacement);
  assert.equal(rerouted.currentActorId, ids.owner);
  assert.equal(rerouted.removeParticipantId, ids.collaborator);
});

test("a collaborator cannot hand off again or make the final delivery", () => {
  const thread = assignedThread({ current_actor_id: ids.collaborator, return_to_id: ids.owner });
  assert.throws(
    () => transition(thread, ids.collaborator, "handoff", ids.initiator),
    (error) => error.code === "action_not_allowed",
  );
  assert.throws(
    () => transition(thread, ids.collaborator, "final_deliver", null),
    (error) => error.code === "action_not_allowed",
  );
});

test("a progress note keeps the active relay with its current actor", () => {
  const thread = {
    ...assignedThread(),
    current_actor_id: ids.collaborator,
    return_to_id: ids.owner,
  };
  const progress = transition(thread, ids.collaborator, "note", null, true, "progress");
  assert.equal(progress.currentActorId, ids.collaborator);
  assert.equal(progress.threadReturnToId, ids.owner);
  assert.equal(progress.phase, "active");
  const genericNote = transition(thread, ids.initiator, "note", null);
  assert.equal(genericNote.currentActorId, ids.collaborator);
  assert.equal(genericNote.phase, "active");
  assert.throws(
    () => transition(thread, ids.initiator, "note", null, true, "progress"),
    (error) => error.code === "action_not_allowed",
  );
  assert.throws(
    () => transition({ ...thread, phase: "awaiting_acceptance" }, ids.collaborator, "note", null, true, "progress"),
    (error) => error.code === "action_not_allowed",
  );
  const pendingHandoff = {
    ...assignedThread(),
    phase: "pending_transfer",
    pending_transfer_kind: "handoff",
    pending_sender_id: ids.owner,
    pending_recipient_id: ids.collaborator,
  };
  const pendingProgress = transition(pendingHandoff, ids.owner, "note", null, true, "progress");
  assert.equal(pendingProgress.phase, "pending_transfer");
  assert.equal(pendingProgress.pendingRecipientId, ids.collaborator);
  assert.throws(
    () => transition({ ...pendingHandoff, pending_transfer_kind: "assignment" }, ids.owner, "note", null, true, "progress"),
    (error) => error.code === "action_not_allowed",
  );
});

test("accept closes the thread only for the final recipient", () => {
  const thread = assignedThread({
    phase: "awaiting_acceptance",
    current_actor_id: ids.initiator,
    return_to_id: null,
  });
  assert.throws(
    () => transition(thread, ids.owner, "accept", null),
    (error) => error.code === "action_not_allowed",
  );
  assert.equal(transition(thread, ids.initiator, "accept", null).phase, "closed");
});

test("only the initiator can cancel unfinished work", () => {
  const thread = assignedThread();
  assert.throws(
    () => transition(thread, ids.owner, "cancel", null),
    (error) => error.code === "action_not_allowed",
  );
  assert.equal(transition(thread, ids.initiator, "cancel", null).phase, "cancelled");
});
