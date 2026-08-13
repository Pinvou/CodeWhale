import { AppError } from "./errors.js";

const ACTION_KINDS = new Set([
  "note",
  "handoff",
  "accept_transfer",
  "decline_transfer",
  "withdraw_transfer",
  "reroute_transfer",
  "return",
  "final_deliver",
  "request_changes",
  "accept",
  "cancel",
]);

function denied(message) {
  throw new AppError(403, "action_not_allowed", message);
}

function nextState(thread, patch = {}) {
  return {
    recipientId: null,
    actionReturnToId: null,
    ownerId: thread.owner_id,
    currentActorId: thread.current_actor_id,
    threadReturnToId: thread.return_to_id,
    phase: thread.phase,
    pendingTransferKind: thread.pending_transfer_kind || null,
    pendingSenderId: thread.pending_sender_id || null,
    pendingRecipientId: thread.pending_recipient_id || null,
    addParticipantId: null,
    removeParticipantId: null,
    grantMaterialAccessId: null,
    transferMode: null,
    ...patch,
  };
}

function clearPending(thread, patch = {}) {
  return nextState(thread, {
    pendingTransferKind: null,
    pendingSenderId: null,
    pendingRecipientId: null,
    ...patch,
  });
}

function transferFallback(thread, patch = {}) {
  if (thread.pending_transfer_kind === "assignment") {
    return clearPending(thread, {
      ownerId: thread.initiator_id,
      currentActorId: thread.initiator_id,
      threadReturnToId: thread.final_recipient_id,
      phase: "awaiting_reassignment",
      ...patch,
    });
  }
  return clearPending(thread, { phase: "active", ...patch });
}

export function transition(thread, actorId, kind, recipientId, confirmedTransfers = true) {
  if (!ACTION_KINDS.has(kind)) denied("The action kind is not available on this endpoint.");
  if (!new Set(["handoff", "reroute_transfer"]).has(kind) && recipientId !== null && recipientId !== undefined) {
    denied("Only handoff and reroute_transfer accept a recipient_id.");
  }
  if (thread.phase === "closed" || thread.phase === "cancelled") {
    denied("A finished thread cannot receive more actions.");
  }

  if (kind === "cancel") {
    if (thread.initiator_id !== actorId) denied("Only the initiator can cancel this work.");
    return clearPending(thread, {
      ownerId: thread.owner_id,
      currentActorId: null,
      threadReturnToId: null,
      phase: "cancelled",
    });
  }

  if (kind === "note") return nextState(thread);

  if (kind === "reroute_transfer") {
    const canReroutePending = thread.phase === "pending_transfer" && thread.pending_sender_id === actorId;
    const canRerouteAssignment = thread.phase === "awaiting_reassignment" && thread.initiator_id === actorId;
    if (!canReroutePending && !canRerouteAssignment) {
      denied("Only the current transfer sender can choose another recipient.");
    }
    if (!recipientId || recipientId === actorId || recipientId === thread.pending_recipient_id) {
      denied("Rerouting requires a different team member.");
    }
    const transferKind = canRerouteAssignment ? "assignment" : thread.pending_transfer_kind;
    if (!confirmedTransfers) {
      const directState = transferKind === "assignment"
        ? {
            ownerId: recipientId,
            currentActorId: recipientId,
            threadReturnToId: thread.final_recipient_id,
          }
        : {
            currentActorId: recipientId,
            threadReturnToId: thread.owner_id,
            actionReturnToId: thread.owner_id,
          };
      return clearPending(thread, {
        ...directState,
        recipientId,
        phase: "active",
        addParticipantId: recipientId,
        removeParticipantId: canReroutePending ? thread.pending_recipient_id : null,
        grantMaterialAccessId: recipientId,
        transferMode: "direct_legacy",
      });
    }
    return nextState(thread, {
      recipientId,
      phase: "pending_transfer",
      pendingTransferKind: transferKind,
      pendingSenderId: actorId,
      pendingRecipientId: recipientId,
      addParticipantId: recipientId,
      removeParticipantId: canReroutePending ? thread.pending_recipient_id : null,
      transferMode: "confirmed",
    });
  }

  if (new Set(["accept_transfer", "decline_transfer"]).has(kind)) {
    if (thread.phase !== "pending_transfer" || thread.pending_recipient_id !== actorId) {
      denied("Only the proposed recipient can answer this transfer.");
    }
    if (kind === "decline_transfer") {
      return transferFallback(thread, {
        recipientId: thread.pending_sender_id,
        removeParticipantId: actorId,
      });
    }
    if (thread.pending_transfer_kind === "assignment") {
      return clearPending(thread, {
        recipientId: thread.pending_sender_id,
        ownerId: actorId,
        currentActorId: actorId,
        threadReturnToId: thread.final_recipient_id,
        phase: "active",
        grantMaterialAccessId: actorId,
      });
    }
    return clearPending(thread, {
      recipientId: thread.pending_sender_id,
      currentActorId: actorId,
      threadReturnToId: thread.owner_id,
      phase: "active",
      grantMaterialAccessId: actorId,
    });
  }

  if (kind === "withdraw_transfer") {
    if (thread.phase !== "pending_transfer" || thread.pending_sender_id !== actorId) {
      denied("Only the transfer sender can withdraw a pending transfer.");
    }
    return transferFallback(thread, {
      recipientId: thread.pending_recipient_id,
      removeParticipantId: thread.pending_recipient_id,
    });
  }

  if (kind === "handoff") {
    if (thread.phase !== "active" || thread.current_actor_id !== actorId || thread.owner_id !== actorId) {
      denied("Only the current work owner can invite another member to take the relay.");
    }
    if (!recipientId || recipientId === actorId) denied("Handoff requires another team member.");
    if (!confirmedTransfers) {
      return nextState(thread, {
        recipientId,
        actionReturnToId: thread.owner_id,
        currentActorId: recipientId,
        threadReturnToId: thread.owner_id,
        addParticipantId: recipientId,
        grantMaterialAccessId: recipientId,
        transferMode: "direct_legacy",
      });
    }
    return nextState(thread, {
      recipientId,
      actionReturnToId: thread.owner_id,
      phase: "pending_transfer",
      pendingTransferKind: "handoff",
      pendingSenderId: actorId,
      pendingRecipientId: recipientId,
      addParticipantId: recipientId,
      transferMode: "confirmed",
    });
  }

  if (thread.phase !== "active" && !new Set(["request_changes", "accept"]).has(kind)) {
    denied("This action requires active work.");
  }

  if (kind === "return") {
    if (
      thread.current_actor_id !== actorId
      || actorId === thread.owner_id
      || thread.return_to_id !== thread.owner_id
    ) {
      denied("Only the current collaborator can return work to its owner.");
    }
    return nextState(thread, {
      recipientId: thread.owner_id,
      actionReturnToId: thread.final_recipient_id,
      currentActorId: thread.owner_id,
      threadReturnToId: thread.final_recipient_id,
    });
  }

  if (kind === "final_deliver") {
    if (thread.current_actor_id !== actorId || thread.owner_id !== actorId) {
      denied("Only the current work owner can make the final delivery.");
    }
    return nextState(thread, {
      recipientId: thread.final_recipient_id,
      currentActorId: thread.final_recipient_id,
      threadReturnToId: null,
      phase: "awaiting_acceptance",
    });
  }

  if (kind === "request_changes") {
    if (
      thread.phase !== "awaiting_acceptance"
      || thread.current_actor_id !== actorId
      || thread.final_recipient_id !== actorId
    ) {
      denied("Only the final recipient can request changes while accepting a delivery.");
    }
    return nextState(thread, {
      recipientId: thread.owner_id,
      actionReturnToId: thread.final_recipient_id,
      currentActorId: thread.owner_id,
      threadReturnToId: thread.final_recipient_id,
      phase: "active",
    });
  }

  if (
    kind !== "accept"
    || thread.phase !== "awaiting_acceptance"
    || thread.current_actor_id !== actorId
    || thread.final_recipient_id !== actorId
  ) {
    denied("Only the final recipient can accept the final delivery.");
  }
  return nextState(thread, {
    currentActorId: null,
    threadReturnToId: null,
    phase: "closed",
  });
}
