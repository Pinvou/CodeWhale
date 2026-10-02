import {
  CONTROLLER_PROTOCOL_VERSION,
  validateControllerMessage,
  validateControllerRuntimeMessage,
} from "./protocol-controller.js";
import { ControllerLivenessHooks } from "./controller-liveness.js";
import { ControllerResumeHooks } from "./controller-resume.js";

const ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const HANDSHAKE_TYPES = new Set([
  "finished_receipt_commit", "encrypted_handshake_finished", "finished_receipt_ack",
]);
const LIVENESS_TYPES = new Set([
  "controller_liveness", "controller_liveness_challenge_a", "encrypted_controller_liveness_a",
  "controller_liveness_attestation_request_a",
]);
const RESUME_TYPES = new Set([
  "controller_resume_prepare", "encrypted_resume_confirm", "controller_resume_activation_ack",
  "controller_post_activation_liveness", "controller_resume_ready_ack",
]);
const BUSINESS_STAGE = Object.freeze({
  capability_snapshot_request: "task",
  capability_snapshot: "task",
  capability_snapshot_error: "task",
  task_submit: "task",
  task_cancel: "task",
  task_status: "task",
  remote_action_request: "task",
  remote_action_result: "task",
  temporary_model_package: "task",
  journal_event: "task",
  permission_list: "permission",
  permission_decide: "permission",
  attachment_begin: "attachment",
  attachment_chunk: "attachment",
  attachment_abort: "attachment",
  artifact_index: "artifact",
  artifact_preview: "artifact",
  artifact_download_begin: "artifact",
  artifact_download_chunk: "artifact",
  artifact_download_abort: "artifact",
});

function businessAuditEvent(payloadKind, accepted) {
  const stage = BUSINESS_STAGE[payloadKind];
  return stage ? `${stage}_${accepted ? "forwarded" : "rejected"}` : null;
}

export class ControllerRouterError extends Error {
  constructor(code, options) {
    super(code, options);
    this.name = "ControllerRouterError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new ControllerRouterError(code, cause ? { cause } : undefined);
}

function id(value, label) {
  if (typeof value !== "string" || !ID.test(value)) fail(`${label}_invalid`);
  return value;
}

function publicError(error) {
  const code = error?.code;
  if (["unsupported_capability", "unsupported_version", "version_incompatible"].includes(code)
    || ["unsupported_capability", "unsupported_version"].includes(error?.cause?.code)) {
    return "version_incompatible";
  }
  if (["pairing_code_invalid", "controller_busy", "endpoint_offline", "version_incompatible"].includes(code)) {
    return code;
  }
  return "invalid_message";
}

function errorMessage(code) {
  if (code === "pairing_code_invalid") return "连接码无效或已失效";
  if (code === "controller_busy") return "被控设备当前正被使用";
  if (code === "endpoint_offline") return "被控设备不在线";
  if (code === "version_incompatible") return "双方版本不兼容";
  return "消息无效";
}

export class ControllerRouter {
  #registration; #leases; #send; #peers = new Map(); #controlled = new Map(); #audit; #hooks; #livenessHooks;

  constructor({
    pairingRegistrationHandler,
    leaseManager,
    send,
    audit = () => {},
    livenessHooks = new ControllerLivenessHooks(),
    resumeHooks = new ControllerResumeHooks(),
    // DEV-06 审核修复：hook 槽必须能在不修改 server.js/router 的前提下扩展。
    // 后续任务（例如 liveness/resume 之外的 hook）通过该数组注入即可，
    // router 与 server 的构造代码保持不变。
    additionalHooks = [],
  } = {}) {
    if (!pairingRegistrationHandler || typeof pairingRegistrationHandler.register !== "function"
      || !leaseManager || typeof leaseManager.claim !== "function"
      || typeof leaseManager.acceptCommitment !== "function"
      || typeof leaseManager.forwardFinished !== "function"
      || typeof leaseManager.acceptReceipt !== "function"
      || typeof leaseManager.abortBySocket !== "function"
      || typeof leaseManager.abortHandshakesBySocket !== "function"
      || typeof leaseManager.lease !== "function"
      || typeof leaseManager.terminateLease !== "function"
      || typeof send !== "function" || typeof audit !== "function") {
      fail("controller_router_config_invalid");
    }
    if (!Array.isArray(additionalHooks)) fail("controller_router_config_invalid");
    const hooksList = [livenessHooks, resumeHooks, ...additionalHooks];
    for (const hooks of hooksList) {
      if (!hooks || typeof hooks.handles !== "function" || typeof hooks.handle !== "function"
        || typeof hooks.onLeaseCreated !== "function" || typeof hooks.onSocketClosed !== "function"
        || typeof hooks.onLeaseTerminated !== "function" || typeof hooks.expire !== "function") {
        fail("controller_router_hooks_invalid");
      }
    }
    this.#registration = pairingRegistrationHandler;
    this.#leases = leaseManager;
    this.#send = send;
    this.#audit = audit;
    this.#livenessHooks = livenessHooks;
    this.#hooks = Object.freeze(hooksList);
  }

  #emit(event) {
    try { this.#audit(event); } catch { /* audit cannot alter routing */ }
  }

  #deliver(peer, message) {
    if (!this.#send(peer.socket, message)) fail("endpoint_offline");
  }

  addSocket({ socketIdentity, source, socket } = {}) {
    socketIdentity = id(socketIdentity, "socket_identity");
    if (typeof source !== "string" || source.length < 1 || source.length > 128 || !socket) {
      fail("controller_socket_invalid");
    }
    if (this.#peers.has(socketIdentity)) fail("controller_socket_conflict");
    this.#peers.set(socketIdentity, {
      socketIdentity, source, socket, role: "unknown", endpointId: null,
      authenticatedContext: null, counterpart: null, controllerLeaseId: null,
    });
  }

  authenticateControlled({ socketIdentity, endpointId, authenticatedContext } = {}) {
    socketIdentity = id(socketIdentity, "socket_identity");
    endpointId = id(endpointId, "endpoint_id");
    const peer = this.#peers.get(socketIdentity);
    if (!peer || (peer.role !== "unknown" && peer.role !== "B")) fail("controller_socket_invalid");
    const previous = this.#controlled.get(endpointId);
    if (previous && previous !== socketIdentity) {
      const stale = this.#peers.get(previous);
      if (stale) {
        stale.role = "unknown";
        stale.endpointId = null;
        stale.authenticatedContext = null;
      }
    }
    peer.role = "B";
    peer.endpointId = endpointId;
    peer.authenticatedContext = authenticatedContext;
    this.#controlled.set(endpointId, socketIdentity);
  }

  resolveControlledEndpoint(endpointId) {
    endpointId = id(endpointId, "endpoint_id");
    const socketIdentity = this.#controlled.get(endpointId);
    const peer = socketIdentity ? this.#peers.get(socketIdentity) : null;
    return peer?.role === "B" ? Object.freeze({ socketIdentity }) : null;
  }

  async handle({ socketIdentity, message } = {}) {
    socketIdentity = id(socketIdentity, "socket_identity");
    const peer = this.#peers.get(socketIdentity);
    if (!peer) fail("controller_socket_invalid");
    const requestId = typeof message?.request_id === "string" && ID.test(message.request_id)
      ? message.request_id : "request-invalid";
    try {
      if (message?.type === "desktop_pairing_register") {
        if (peer.role !== "B") fail("controller_role_invalid");
        const response = await this.#registration.register(message, peer.authenticatedContext);
        this.#deliver(peer, response);
        this.#emit("desktop_pairing_registered");
        return response;
      }
      if (message?.type === "controller_pairing_claim") {
        if (peer.role !== "unknown" && peer.role !== "A") fail("controller_role_invalid");
        const reserved = await this.#leases.claim({
          message, socketIdentity, source: peer.source,
        });
        const controlledIdentity = this.#controlled.get(reserved.transcript.endpoint_id);
        const controlled = controlledIdentity ? this.#peers.get(controlledIdentity) : null;
        if (!controlled || controlled.role !== "B") fail("endpoint_offline");
        peer.role = "A";
        peer.counterpart = controlledIdentity;
        controlled.counterpart = socketIdentity;
        this.#deliver(peer, reserved);
        this.#deliver(controlled, reserved);
        return reserved;
      }
      if (HANDSHAKE_TYPES.has(message?.type)) {
        if (!new Set(["A", "B"]).has(peer.role) || !peer.counterpart) {
          fail("controller_role_invalid");
        }
        if (message.type === "finished_receipt_commit") {
          const routed = this.#leases.acceptCommitment({
            message, role: peer.role, socketIdentity,
          });
          this.#deliver(peer, routed.message);
          this.#deliver(this.#counterpart(peer), routed.message);
          return routed.message;
        }
        if (message.type === "encrypted_handshake_finished") {
          const routed = this.#leases.forwardFinished({
            message, role: peer.role, socketIdentity,
          });
          this.#deliver(this.#counterpart(peer), routed.message);
          return routed.message;
        }
        const committed = await this.#leases.acceptReceipt({
          message, role: peer.role, socketIdentity,
        });
        if (!committed) return null;
        const counterpart = this.#counterpart(peer);
        const peerMessage = committed.messages[peer.role];
        const counterpartMessage = committed.messages[counterpart.role];
        peer.controllerLeaseId = committed.lease.controller_lease_id;
        counterpart.controllerLeaseId = committed.lease.controller_lease_id;
        await Promise.all(this.#hooks.map((hooks) => hooks.onLeaseCreated(committed.lease)));
        this.#deliver(peer, peerMessage);
        this.#deliver(counterpart, counterpartMessage);
        this.#emit("controller_joined");
        return committed;
      }
      if (message?.type === "controller_disconnect") {
        if (!new Set(["A", "B"]).has(peer.role) || !peer.controllerLeaseId) {
          fail("controller_role_invalid");
        }
        const lease = this.#leases.lease(peer.controllerLeaseId);
        if (!lease || lease.sockets[peer.role] !== socketIdentity) fail("controller_lease_not_found");
        let disconnect;
        try { disconnect = validateControllerMessage(message, peer.role, lease); } catch (error) {
          fail("controller_disconnect_invalid", error);
        }
        const counterpart = this.#counterpart(peer);
        this.#deliver(counterpart, disconnect);
        await this.#leases.terminateLease({
          controllerLeaseId: lease.controller_lease_id,
          reason: disconnect.reason,
        });
        peer.controllerLeaseId = null;
        counterpart.controllerLeaseId = null;
        peer.counterpart = null;
        counterpart.counterpart = null;
        return disconnect;
      }
      if (message?.type === "encrypted_frame") {
        if (!new Set(["A", "B"]).has(peer.role) || !peer.controllerLeaseId) {
          fail("controller_role_invalid");
        }
        const lease = this.#leases.lease(peer.controllerLeaseId);
        if (!lease || lease.sockets?.[peer.role] !== socketIdentity) {
          fail("controller_lease_not_found");
        }
        const resume = this.#hooks.find(
          (candidate) => typeof candidate.isBusinessAllowed === "function",
        );
        if (resume && !resume.isBusinessAllowed(lease.controller_lease_id)) {
          fail("controller_business_suspended");
        }
        let validated;
        try {
          validated = validateControllerRuntimeMessage(message, peer.role, {
            lease: {
              connection_id: lease.connection_id,
              controller_lease_id: lease.controller_lease_id,
              lease_epoch: lease.lease_epoch,
              relay_epoch: lease.relay_epoch,
              remote_control_generation: lease.remote_control_generation,
            },
            negotiated_capabilities: lease.negotiated_capabilities,
          });
        } catch (error) {
          fail("controller_business_invalid", error);
        }
        const counterpart = this.#counterpart(peer);
        const counterpartRole = peer.role === "A" ? "B" : "A";
        if (lease.sockets?.[counterpartRole] !== counterpart.socketIdentity
          || counterpart.controllerLeaseId !== lease.controller_lease_id) {
          fail("controller_lease_not_found");
        }
        this.#deliver(counterpart, validated);
        const auditEvent = businessAuditEvent(validated.payload_kind, true);
        if (auditEvent) this.#emit(auditEvent);
        return validated;
      }
      const hooks = this.#hooks.find((candidate) => candidate.handles(message?.type));
      if (hooks) {
        const lease = typeof message?.controller_lease_id === "string"
          ? this.#leases.lease(message.controller_lease_id) : null;
        const result = await hooks.handle(Object.freeze({
          message, socket_identity: socketIdentity, role: peer.role, lease,
        }));
        let committed = null;
        try {
          this.#applyPeerUpdates(result?.peer_updates, lease);
          for (const delivery of result?.deliveries || []) {
            const target = delivery.role === peer.role ? peer : this.#counterpart(peer);
            this.#deliver(target, delivery.message);
          }
          if (result?.commit) committed = await result.commit();
          if (committed?.lease) {
            await Promise.all(this.#hooks.map((candidate) => candidate.onLeaseCreated(committed.lease)));
          }
          if (LIVENESS_TYPES.has(message?.type)) this.#emit("liveness_forwarded");
          if (RESUME_TYPES.has(message?.type)) this.#emit("resume_forwarded");
          return result ?? null;
        } catch (error) {
          try {
            await result?.abort?.();
            await Promise.all(this.#hooks.map((candidate) => candidate.onLeaseTerminated({
              controller_lease_id: lease.controller_lease_id,
            })));
          } catch { /* cleanup failure cannot resurrect an uncertain resume */ }
          throw error;
        }
      }
      fail("controller_message_unsupported");
    } catch (error) {
      if (message?.type === "desktop_pairing_register") {
        this.#emit("desktop_pairing_register_rejected");
      } else if (HANDSHAKE_TYPES.has(message?.type)) {
        this.#emit("handshake_rejected");
      } else if (LIVENESS_TYPES.has(message?.type)) {
        this.#emit("liveness_rejected");
      } else if (RESUME_TYPES.has(message?.type)) {
        this.#emit("resume_rejected");
      } else if (message?.type === "encrypted_frame") {
        const auditEvent = businessAuditEvent(message?.payload_kind, false);
        if (auditEvent) this.#emit(auditEvent);
      }
      const code = publicError(error);
      this.#send(peer.socket, {
        v: CONTROLLER_PROTOCOL_VERSION,
        type: "error",
        request_id: requestId,
        code,
        message: errorMessage(code),
      });
      if (message?.type === "controller_pairing_claim" || HANDSHAKE_TYPES.has(message?.type)) {
        await this.#leases.abortBySocket({ socketIdentity, reason: "handshake_failed" })
          .catch(() => {});
      }
      throw error;
    }
  }

  #counterpart(peer) {
    const counterpart = this.#peers.get(peer.counterpart);
    if (!counterpart || counterpart.counterpart !== peer.socketIdentity) fail("endpoint_offline");
    return counterpart;
  }

  #applyPeerUpdates(updates, lease) {
    if (!updates) return;
    if (!Array.isArray(updates)) fail("controller_peer_updates_invalid");
    for (const update of updates) {
      if (!update || !new Set(["A", "B"]).has(update.role)
        || update.controller_lease_id !== lease.controller_lease_id
        || typeof update.socket_identity !== "string" || !ID.test(update.socket_identity)) {
        fail("controller_peer_updates_invalid");
      }
      const peer = this.#peers.get(update.socket_identity);
      if (!peer) fail("endpoint_offline");
      if (peer.role !== "unknown" && peer.role !== update.role) fail("controller_role_invalid");
      peer.role = update.role;
      peer.controllerLeaseId = lease.controller_lease_id;
      peer.counterpart = update.counterpart ?? null;
    }
    for (const update of updates) {
      if (!update.counterpart) continue;
      const peer = this.#peers.get(update.socket_identity);
      const counterpart = this.#peers.get(update.counterpart);
      if (!peer || !counterpart || counterpart.counterpart !== peer.socketIdentity) {
        fail("controller_peer_updates_invalid");
      }
    }
  }

  async removeSocket(socketIdentity) {
    socketIdentity = id(socketIdentity, "socket_identity");
    const peer = this.#peers.get(socketIdentity);
    if (!peer) return Object.freeze({ handshakes: 0, leases: 0 });
    const lease = peer.controllerLeaseId ? this.#leases.lease(peer.controllerLeaseId) : null;
    // Both hooks run asynchronously and liveness removes its state on close.
    // Capture authenticated evidence once, before dispatching either hook.
    let evidenceDeadline = null;
    try { evidenceDeadline = this.#livenessHooks.resumeDeadline?.(lease) ?? null; }
    catch { /* Missing/invalid evidence must not prevent disconnect cleanup. */ }
    const hookOutcomes = await Promise.allSettled(this.#hooks.map((hooks) => hooks.onSocketClosed(Object.freeze({
      socket_identity: socketIdentity, role: peer.role, lease, evidence_deadline_ms: evidenceDeadline,
    }))));
    const hookResults = hookOutcomes
      .filter((result) => result.status === "fulfilled")
      .map((result) => result.value);
    this.#peers.delete(socketIdentity);
    if (peer.endpointId && this.#controlled.get(peer.endpointId) === socketIdentity) {
      this.#controlled.delete(peer.endpointId);
    }
    const counterpart = peer.counterpart ? this.#peers.get(peer.counterpart) : null;
    if (counterpart?.counterpart === socketIdentity) counterpart.counterpart = null;
    if (hookResults.some((result) => result?.retain_lease === true)) {
      const handshakes = await this.#leases.abortHandshakesBySocket({
        socketIdentity, reason: "owner_disconnected",
      });
      return Object.freeze({ handshakes, leases: 0 });
    }
    return this.#leases.abortBySocket({ socketIdentity, reason: "owner_disconnected" });
  }

  async expire() {
    const results = await Promise.all(this.#hooks.map(async (hooks) => ({
      hooks,
      count: Number(await hooks.expire()) || 0,
    })));
    for (const { hooks, count } of results) {
      const event = hooks instanceof ControllerLivenessHooks
        ? "liveness_expired"
        : (hooks instanceof ControllerResumeHooks ? "resume_expired" : null);
      for (let index = 0; event && index < count; index += 1) this.#emit(event);
    }
    return results.reduce((sum, result) => sum + result.count, 0);
  }
}
