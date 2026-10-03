// Local IPC integration fixture. Real Router/LeaseManager/ReservationManager
// and liveness hooks; storage/enrollment and endpoint crypto are test adapters.
// No network service, installed credentials or production keys are used.
import crypto from "node:crypto";
import readline from "node:readline";
import { ControllerRouter } from "../../controller-router.js";
import { ControllerLeaseManager } from "../../controller-lease.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../../controller-reservation.js";
import { ControllerLivenessHooks } from "../../controller-liveness.js";
import { ControllerResumeHooks } from "../../controller-resume.js";
import { receiptCommitmentHex } from "../../protocol-controller.js";

// Bound a broken IPC request without depending on the parent's paused clock.
setTimeout(() => process.exit(3), 15_000).unref();

let clock = 0;
const capabilities = ["encrypted_frames_v1", "finished_receipt_v1", "pairing_v1", "resume_v1", "role_liveness_v1", "task_v1"];
const signing = crypto.generateKeyPairSync("ed25519");
const keyId = "relay-key-ipc-fixture";
const publicKey = () => crypto.generateKeyPairSync("x25519").publicKey.export({format: "der", type: "spki"}).subarray(-32).toString("base64");
const pairing = {pairing_id: "pairing-ipc", endpoint_id: "endpoint-ipc", remote_control_generation: "generation-ipc", handshake_public_key_b64: publicKey(), capabilities};
let consumed = false;
const pairings = {
  activePairing: id => !consumed && id === pairing.pairing_id ? pairing : null,
  findActiveByCode: code => !consumed && code === "2345678" ? pairing : null,
  consumeForControllerLease: async value => { consumed = true; return value; },
  terminate: async () => {},
};
const reservations = new ControllerReservationManager({pairingStore: pairings, interactionSlots: new EndpointInteractionSlots(), clock: () => clock});
let router;
const leases = new ControllerLeaseManager({pairingStore: pairings, reservationManager: reservations,
  relayEpoch: "relay-ipc", signingKeyId: keyId, signTranscript: preimage => crypto.sign(null, preimage, signing.privateKey),
  resolveControlledEndpoint: async id => router.resolveControlledEndpoint(id), clock: () => clock});
const sentA = [];
router = new ControllerRouter({pairingRegistrationHandler: {register: async () => {throw new Error("registration outside fixture");}},
  leaseManager: leases, livenessHooks: new ControllerLivenessHooks({clock: () => clock, signingKeyId: keyId, signingPrivateKey: signing.privateKey}),
  resumeHooks: new ControllerResumeHooks({clock: () => clock}), send: (socket, message) => {if(socket === "A") sentA.push(message); return true;}});
router.addSocket({socketIdentity: "socket-A", source: "127.0.0.1", socket: "A"});
router.addSocket({socketIdentity: "socket-B", source: "127.0.0.1", socket: "B"});
router.authenticateControlled({socketIdentity: "socket-B", endpointId: pairing.endpoint_id, authenticatedContext: {endpointId: pairing.endpoint_id, credential: "non-production-fixture"}});
const reserved = await router.handle({socketIdentity: "socket-A", message: {v: 1, type: "controller_pairing_claim", request_id: "request-ipc-claim", claim_request_id: "claim-ipc", pairing_code: "2345678", handshake_public_key_b64: publicKey(), key_algorithm: "x25519", capabilities}});
const common = {v: 1, pairing_id: reserved.pairing_id, reservation_id: reserved.reservation_id,
  remote_control_generation: reserved.remote_control_generation, relay_epoch: reserved.relay_epoch, transcript_hash: reserved.transcript_hash};
const secrets = {A: crypto.randomBytes(32), B: crypto.randomBytes(32)};
for(const role of ["A", "B"]){
  await router.handle({socketIdentity: `socket-${role}`, message: {...common, type: "finished_receipt_commit", request_id: `request-commit-${role}`, sender_role: role,
    commitment: receiptCommitmentHex({relay_epoch: reserved.relay_epoch, pairing_id: reserved.pairing_id, reservation_id: reserved.reservation_id, transcript_hash: reserved.transcript_hash, sender_role: role}, secrets[role])}});
}
for(const role of ["A", "B"]){
  await router.handle({socketIdentity: `socket-${role}`, message: {...common, type: "encrypted_handshake_finished", request_id: `request-finished-${role}`, sender_role: role, ciphertext_b64: "AQID", ciphertext_bytes: "3"}});
}
for(const role of ["A", "B"]){
  const receiver = role === "A" ? "B" : "A";
  await router.handle({socketIdentity: `socket-${receiver}`, message: {...common, type: "finished_receipt_ack", request_id: `request-receipt-${role}`, sender_role: role, receiver_role: receiver, receipt_secret_b64: secrets[role].toString("base64")}});
}
const joined = sentA.find(message => message.type === "controller_joined");
if(!joined || !consumed) throw new Error("production receipt/lease gate did not complete");
const lease = leases.lease(joined.controller_lease_id);
const context = Object.fromEntries(["connection_id", "controller_lease_id", "lease_epoch", "relay_epoch", "remote_control_generation"].map(field => [field, lease[field]]));
process.stdout.write(JSON.stringify({joined, reserved, public_key_b64: signing.publicKey.export({format: "der", type: "spki"}).subarray(-32).toString("base64"), key_id: keyId, capabilities}) + "\n");
let bSequence = 0;
for await (const line of readline.createInterface({input: process.stdin, crlfDelay: Infinity})) {
  const command = JSON.parse(line);
  if(!Number.isSafeInteger(command.clock_ms) || command.clock_ms < clock) throw new Error("invalid fixture clock");
  clock = command.clock_ms;
  sentA.length = 0;
  if(command.frame.type === "controller_liveness_attestation_request_b" && command.healthy_b){
    await router.handle({socketIdentity: "socket-B", message: {v: 1, type: "controller_liveness", ...context,
      sender_role: "B", liveness_epoch: lease.liveness_epoch_b, heartbeat_sequence: String(++bSequence)}});
  }
  // The real server catches Router rejection after it emits the public error
  // frame. Keep the authenticated socket open when B evidence has gone stale.
  try {
    await router.handle({socketIdentity: "socket-A", message: command.frame});
  } catch (error) {
    if (error.code !== "controller_liveness_attestation_stale") throw error;
  }
  process.stdout.write(JSON.stringify({deliveries: [...sentA]}) + "\n");
}
await router.removeSocket("socket-A");
await router.removeSocket("socket-B");
