import { CONTROLLER_PROTOCOL_VERSION, validateControllerMessage } from "./protocol-controller.js";
import { ControllerLivenessHooks } from "./controller-liveness.js";
import { ControllerResumeHooks } from "./controller-resume.js";

const ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const HANDSHAKE_TYPES = new Set([
  "finished_receipt_commit", "encrypted_handshake_finished", "finished_receipt_ack",
]);

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
  #registration; #leases; #send; #peers = new Map(); #controlled = new Map(); #audit; #hooks;

  constructor({
    pairingRegistrationHandler,
    leaseManager,
    send,
    audit = () => {},
    livenessHooks = new ControllerLivenessHooks(),
    resumeHooks = new ControllerResumeHooks(),
  } = {}) {
    if (!pairingRegistrationHandler || typeof pairingRegistrationHandler.register !== "function"
      || !leaseManager || typeof leaseManager.claimForDelivery !== "function"
      || typeof leaseManager.acceptCommitment !== "function"
      || typeof leaseManager.forwardFinished !== "function"
      || typeof leaseManager.acceptReceipt !== "function"
      || typeof leaseManager.abortBySocket !== "function"
      || typeof leaseManager.abortHandshakesBySocket !== "function"
      || typeof leaseManager.isPendingHandshake !== "function"
      || typeof leaseManager.lease !== "function"
      || typeof leaseManager.terminateLease !== "function"
      || typeof send !== "function" || typeof audit !== "function") {
      fail("controller_router_config_invalid");
    }
    for (const hooks of [livenessHooks, resumeHooks]) {
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
    this.#hooks = Object.freeze([livenessHooks, resumeHooks]);
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
      pendingReservationId: null,
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
    let deliveryReservationId = null;
    let createdLease = null;
    try {
      if (message?.type === "desktop_pairing_register") {
        if (peer.role !== "B") fail("controller_role_invalid");
        const response = await this.#registration.register(message, peer.authenticatedContext);
        this.#deliver(peer, response);
        return response;
      }
      if (message?.type === "controller_pairing_claim") {
        if (peer.role !== "unknown" && peer.role !== "A") fail("controller_role_invalid");
        if (peer.controllerLeaseId && this.#leases.lease(peer.controllerLeaseId)) fail("controller_busy");
        const delivery = await this.#leases.claimForDelivery({
          message, socketIdentity, source: peer.source,
        });
        const reserved = delivery.message;
        deliveryReservationId = delivery.owns_initial_delivery ? reserved.reservation_id : null;
        const controlledIdentity = this.#controlled.get(reserved.transcript.endpoint_id);
        const controlled = controlledIdentity ? this.#peers.get(controlledIdentity) : null;
        if (this.#peers.get(socketIdentity) !== peer || !controlled || controlled.role !== "B"
          || !this.#leases.isPendingHandshake({ reservationId: reserved.reservation_id,
            socketIdentity, controlledSocketIdentity: controlledIdentity })) fail("endpoint_offline");
        if ((peer.pendingReservationId && peer.pendingReservationId !== reserved.reservation_id)
          || (controlled.pendingReservationId && controlled.pendingReservationId !== reserved.reservation_id)
          || (controlled.controllerLeaseId && this.#leases.lease(controlled.controllerLeaseId))) fail("controller_busy");
        peer.role = "A";
        peer.counterpart = controlledIdentity;
        controlled.counterpart = socketIdentity;
        peer.pendingReservationId = reserved.reservation_id;
        controlled.pendingReservationId = reserved.reservation_id;
        this.#deliver(peer, reserved);
        this.#deliver(controlled, reserved);
        this.#emit("controller_pairing_reserved");
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
          deliveryReservationId = message.reservation_id;
          this.#deliver(peer, routed.message);
          this.#deliver(this.#counterpart(peer), routed.message);
          return routed.message;
        }
        if (message.type === "encrypted_handshake_finished") {
          const routed = this.#leases.forwardFinished({
            message, role: peer.role, socketIdentity,
          });
          deliveryReservationId = message.reservation_id;
          this.#deliver(this.#counterpart(peer), routed.message);
          return routed.message;
        }
        const committed = await this.#leases.acceptReceipt({
          message, role: peer.role, socketIdentity,
        });
        if (!committed) return null;
        createdLease = committed.lease;
        const counterpart = this.#counterpart(peer);
        const peerMessage = committed.messages[peer.role];
        const counterpartMessage = committed.messages[counterpart.role];
        peer.controllerLeaseId = committed.lease.controller_lease_id;
        counterpart.controllerLeaseId = committed.lease.controller_lease_id;
        peer.pendingReservationId = null;
        counterpart.pendingReservationId = null;
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
        this.#emit("controller_disconnected");
        return disconnect;
      }
      const hooks = this.#hooks.find((candidate) => candidate.handles(message?.type));
      if (hooks) {
        const lease = typeof message?.controller_lease_id === "string"
          ? this.#leases.lease(message.controller_lease_id) : null;
        const result = await hooks.handle(Object.freeze({
          message, socket_identity: socketIdentity, role: peer.role, lease,
        }));
        for (const delivery of result?.deliveries || []) {
          const target = delivery.role === peer.role ? peer : this.#counterpart(peer);
          this.#deliver(target, delivery.message);
        }
        return result ?? null;
      }
      fail("controller_message_unsupported");
    } catch (error) {
      const code = publicError(error);
      try { this.#send(peer.socket, {
        v: CONTROLLER_PROTOCOL_VERSION,
        type: "error",
        request_id: requestId,
        code,
        message: errorMessage(code),
      }); } catch { /* Error delivery cannot broaden or skip precise cleanup. */ }
      if (createdLease) {
        await this.#leases.terminateLease({ controllerLeaseId: createdLease.controller_lease_id,
          reason: "handshake_failed" }).catch(() => {});
        this.#clearLeaseLinks(peer, createdLease.controller_lease_id);
      } else if (deliveryReservationId) {
        // Only a successfully accepted operation whose delivery/binding failed
        // owns cleanup. Rejected retries/replays have no cleanup authority.
        await this.#leases.abortHandshakesBySocket({ socketIdentity,
          reservationId: deliveryReservationId, reason: "handshake_failed" })
          .catch(() => {});
        this.#clearPendingLinks(peer, deliveryReservationId);
      }
      throw error;
    }
  }

  #counterpart(peer) {
    const counterpart = this.#peers.get(peer.counterpart);
    if (!counterpart || counterpart.counterpart !== peer.socketIdentity) fail("endpoint_offline");
    return counterpart;
  }

  #clearPendingLinks(peer, reservationId) {
    if (peer.pendingReservationId !== reservationId) return;
    const counterpart = this.#peers.get(peer.counterpart);
    if (counterpart?.pendingReservationId === reservationId && counterpart.counterpart === peer.socketIdentity) {
      counterpart.pendingReservationId = null;
      counterpart.counterpart = null;
    }
    peer.pendingReservationId = null;
    peer.counterpart = null;
  }

  #clearLeaseLinks(peer, leaseId) {
    const counterpart = this.#peers.get(peer.counterpart);
    if (counterpart?.controllerLeaseId === leaseId) {
      counterpart.controllerLeaseId = null;
      counterpart.counterpart = null;
    }
    if (peer.controllerLeaseId === leaseId) {
      peer.controllerLeaseId = null;
      peer.counterpart = null;
    }
  }

  async removeSocket(socketIdentity) {
    socketIdentity = id(socketIdentity, "socket_identity");
    const peer = this.#peers.get(socketIdentity);
    if (!peer) return Object.freeze({ handshakes: 0, leases: 0 });
    const lease = peer.controllerLeaseId ? this.#leases.lease(peer.controllerLeaseId) : null;
    const hookOutcomes = await Promise.allSettled(this.#hooks.map((hooks) => hooks.onSocketClosed(Object.freeze({
      socket_identity: socketIdentity, role: peer.role, lease,
    }))));
    const hookResults = hookOutcomes
      .filter((result) => result.status === "fulfilled")
      .map((result) => result.value);
    this.#peers.delete(socketIdentity);
    if (peer.endpointId && this.#controlled.get(peer.endpointId) === socketIdentity) {
      this.#controlled.delete(peer.endpointId);
    }
    const counterpart = peer.counterpart ? this.#peers.get(peer.counterpart) : null;
    if (counterpart?.counterpart === socketIdentity) {
      counterpart.counterpart = null;
      counterpart.pendingReservationId = null;
    }
    if (hookResults.some((result) => result?.retain_lease === true)) {
      const handshakes = await this.#leases.abortHandshakesBySocket({
        socketIdentity, reason: "owner_disconnected", endpointId: peer.role === "B" ? peer.endpointId : undefined,
      });
      return Object.freeze({ handshakes, leases: 0 });
    }
    // B's authenticated endpoint also identifies preparations suspended before
    // their first resolver returned a socket identity. Never take this from a frame.
    try {
      return await this.#leases.abortBySocket({ socketIdentity, reason: "owner_disconnected",
        endpointId: peer.role === "B" ? peer.endpointId : undefined });
    } finally {
      if (lease && !this.#leases.lease(lease.controller_lease_id)) {
        this.#clearLeaseLinks(peer, lease.controller_lease_id);
      }
    }
  }

  async expire() {
    const results = await Promise.all(this.#hooks.map((hooks) => hooks.expire()));
    for (const peer of this.#peers.values()) {
      if (peer.role !== "A" || !peer.pendingReservationId) continue;
      if (!this.#leases.isPendingHandshake({ reservationId: peer.pendingReservationId,
        socketIdentity: peer.socketIdentity, controlledSocketIdentity: peer.counterpart })) {
        this.#clearPendingLinks(peer, peer.pendingReservationId);
      }
    }
    return results.reduce((sum, value) => sum + Number(value || 0), 0);
  }
}
