import assert from "node:assert/strict";
import test from "node:test";
import { transition } from "../src/state-machine.js";

const ids = {
  initiator: "00000000-0000-4000-8000-000000000001",
  owner: "00000000-0000-4000-8000-000000000002",
  collaborator: "00000000-0000-4000-8000-000000000003",
};

function assignedThread() {
  return {
    phase: "active",
    initiator_id: ids.initiator,
    owner_id: ids.owner,
    current_actor_id: ids.owner,
    return_to_id: ids.initiator,
    final_recipient_id: ids.initiator,
  };
}

test("handoff keeps ownership and requires return to the owner", () => {
  const next = transition(assignedThread(), ids.owner, "handoff", ids.collaborator);
  assert.deepEqual(next, {
    recipientId: ids.collaborator,
    actionReturnToId: ids.owner,
    currentActorId: ids.collaborator,
    threadReturnToId: ids.owner,
    phase: "active",
    addParticipantId: ids.collaborator,
  });
});

test("a collaborator cannot hand off again or make the final delivery", () => {
  const thread = {
    ...assignedThread(),
    current_actor_id: ids.collaborator,
    return_to_id: ids.owner,
  };
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
  assert.deepEqual(transition(thread, ids.collaborator, "note", null, "progress"), {
    recipientId: null,
    actionReturnToId: null,
    currentActorId: ids.collaborator,
    threadReturnToId: ids.owner,
    phase: "active",
    addParticipantId: null,
  });
  assert.deepEqual(transition(thread, ids.initiator, "note", null), {
    recipientId: null,
    actionReturnToId: null,
    currentActorId: ids.collaborator,
    threadReturnToId: ids.owner,
    phase: "active",
    addParticipantId: null,
  });
  assert.throws(
    () => transition(thread, ids.initiator, "note", null, "progress"),
    (error) => error.code === "action_not_allowed",
  );
  assert.throws(
    () => transition({ ...thread, phase: "awaiting_acceptance" }, ids.collaborator, "note", null, "progress"),
    (error) => error.code === "action_not_allowed",
  );
});

test("accept closes the thread only for the final recipient", () => {
  const thread = {
    ...assignedThread(),
    phase: "awaiting_acceptance",
    current_actor_id: ids.initiator,
    return_to_id: null,
  };
  assert.throws(
    () => transition(thread, ids.owner, "accept", null),
    (error) => error.code === "action_not_allowed",
  );
  assert.deepEqual(transition(thread, ids.initiator, "accept", null), {
    recipientId: null,
    actionReturnToId: null,
    currentActorId: null,
    threadReturnToId: null,
    phase: "closed",
    addParticipantId: null,
  });
});

test("only the initiator can cancel unfinished work", () => {
  const thread = assignedThread();
  assert.throws(
    () => transition(thread, ids.owner, "cancel", null),
    (error) => error.code === "action_not_allowed",
  );
  assert.deepEqual(transition(thread, ids.initiator, "cancel", null), {
    recipientId: null,
    actionReturnToId: null,
    currentActorId: null,
    threadReturnToId: null,
    phase: "cancelled",
    addParticipantId: null,
  });
  assert.throws(
    () => transition({ ...thread, phase: "cancelled", current_actor_id: null, return_to_id: null }, ids.initiator, "note", null),
    (error) => error.code === "action_not_allowed",
  );
});
