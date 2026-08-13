import { AppError } from "./errors.js";

const ACTION_KINDS = new Set([
  "note",
  "handoff",
  "return",
  "final_deliver",
  "request_changes",
  "accept",
  "cancel",
]);

function denied(message) {
  throw new AppError(403, "action_not_allowed", message);
}

export function transition(thread, actorId, kind, recipientId, deliverySubtype = "") {
  if (!ACTION_KINDS.has(kind)) denied("The action kind is not available on this endpoint.");
  if (kind !== "handoff" && recipientId !== null && recipientId !== undefined) {
    denied("Only handoff accepts a recipient_id.");
  }
  if (thread.phase === "closed" || thread.phase === "cancelled") {
    denied("A finished thread cannot receive more actions.");
  }

  if (kind === "cancel") {
    if (thread.initiator_id !== actorId) denied("Only the initiator can cancel this work.");
    return {
      recipientId: null,
      actionReturnToId: null,
      currentActorId: null,
      threadReturnToId: null,
      phase: "cancelled",
      addParticipantId: null,
    };
  }

  if (kind === "note") {
    if (deliverySubtype === "progress"
        && (thread.phase !== "active" || thread.current_actor_id !== actorId)) {
      denied("Only the current actor can submit a progress update for active work.");
    }
    return {
      recipientId: null,
      actionReturnToId: null,
      currentActorId: thread.current_actor_id,
      threadReturnToId: thread.return_to_id,
      phase: thread.phase,
      addParticipantId: null,
    };
  }

  if (kind === "handoff") {
    if (thread.phase !== "active" || thread.current_actor_id !== actorId || thread.owner_id !== actorId) {
      denied("Only the current work owner can hand off an active thread.");
    }
    if (!recipientId || recipientId === actorId) denied("Handoff requires another team member.");
    return {
      recipientId,
      actionReturnToId: thread.owner_id,
      currentActorId: recipientId,
      threadReturnToId: thread.owner_id,
      phase: "active",
      addParticipantId: recipientId,
    };
  }

  if (kind === "return") {
    if (
      thread.phase !== "active"
      || thread.current_actor_id !== actorId
      || actorId === thread.owner_id
      || thread.return_to_id !== thread.owner_id
    ) {
      denied("Only the current collaborator can return work to its owner.");
    }
    return {
      recipientId: thread.owner_id,
      actionReturnToId: thread.final_recipient_id,
      currentActorId: thread.owner_id,
      threadReturnToId: thread.final_recipient_id,
      phase: "active",
      addParticipantId: null,
    };
  }

  if (kind === "final_deliver") {
    if (thread.phase !== "active" || thread.current_actor_id !== actorId || thread.owner_id !== actorId) {
      denied("Only the current work owner can make the final delivery.");
    }
    return {
      recipientId: thread.final_recipient_id,
      actionReturnToId: null,
      currentActorId: thread.final_recipient_id,
      threadReturnToId: null,
      phase: "awaiting_acceptance",
      addParticipantId: null,
    };
  }

  if (kind === "request_changes") {
    if (
      thread.phase !== "awaiting_acceptance"
      || thread.current_actor_id !== actorId
      || thread.final_recipient_id !== actorId
    ) {
      denied("Only the final recipient can request changes while accepting a delivery.");
    }
    return {
      recipientId: thread.owner_id,
      actionReturnToId: thread.final_recipient_id,
      currentActorId: thread.owner_id,
      threadReturnToId: thread.final_recipient_id,
      phase: "active",
      addParticipantId: null,
    };
  }

  if (
    thread.phase !== "awaiting_acceptance"
    || thread.current_actor_id !== actorId
    || thread.final_recipient_id !== actorId
  ) {
    denied("Only the final recipient can accept the final delivery.");
  }
  return {
    recipientId: null,
    actionReturnToId: null,
    currentActorId: null,
    threadReturnToId: null,
    phase: "closed",
    addParticipantId: null,
  };
}
