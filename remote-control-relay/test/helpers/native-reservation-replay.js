// Bounded stdio adapter for the real native handshake regression. Pairing
// persistence/authentication are controlled seams; reservation, signing, lease
// creation and routing use the production implementations without substitutions.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import readline from "node:readline";
import { ControllerLeaseManager } from "../../controller-lease.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../../controller-reservation.js";
import { ControllerRouter } from "../../controller-router.js";

const signing = crypto.generateKeyPairSync("ed25519");
const keyId = "native-replay-key";
let router, pairing, firstClaim;
let consumed = 0, retries = 0;
const deliveries = [];
const hooks = () => ({ handles: () => false, handle: async () => {},
  onLeaseCreated: async () => {}, onLeaseTerminated: async () => {},
  onSocketClosed: async () => ({ retain_lease: false }), expire: async () => 0 });

async function command(input) {
  if (input.init) {
    assert.equal(router, undefined);
    pairing = Object.freeze({ pairing_id: "native-replay-pairing", endpoint_id: "native-replay-endpoint",
      remote_control_generation: input.generation, handshake_public_key_b64: input.publicKey,
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"] });
    const pairings = {
      activePairing: (id) => id === pairing.pairing_id && consumed === 0 ? pairing : null,
      findActiveByCode: (code) => code === "2345678" && consumed === 0 ? pairing : null,
      consumeForControllerLease: async () => { consumed += 1; assert.equal(consumed, 1); },
      terminate: async () => {},
    };
    const reservations = new ControllerReservationManager({ pairingStore: pairings,
      interactionSlots: new EndpointInteractionSlots() });
    const manager = new ControllerLeaseManager({ pairingStore: pairings, reservationManager: reservations,
      relayEpoch: "native-replay-relay", signingKeyId: keyId,
      signTranscript: (bytes) => crypto.sign(null, bytes, signing.privateKey),
      resolveControlledEndpoint: async (id) => router.resolveControlledEndpoint(id) });
    router = new ControllerRouter({ pairingRegistrationHandler: { register: async () => { throw Error("unexpected register"); } },
      leaseManager: manager, livenessHooks: hooks(), resumeHooks: hooks(),
      send: (role, message) => { deliveries.push({ role, message }); return true; } });
    for (const role of ["A", "B"]) router.addSocket({ socketIdentity: `socket-${role}`, source: `source-${role}`, socket: role });
    router.authenticateControlled({ socketIdentity: "socket-B", endpointId: pairing.endpoint_id, authenticatedContext: {} });
  } else {
    const { role, message } = input;
    assert.ok(role === "A" || role === "B");
    if (role === "A") {
      if (message.type === "controller_pairing_claim") firstClaim = structuredClone(message);
      // Retry before every A one-shot frame, while the reservation is pending.
      // Only the outer request_id changes; real Router must notify BOTH ends.
      if (firstClaim && message.type !== "controller_pairing_claim") {
        for (let i = 0; i < 2; i += 1) {
          retries += 1;
          const start = deliveries.length;
          await router.handle({ socketIdentity: "socket-A", message: {
            ...firstClaim, request_id: `request-native-retry-${retries}` } });
          assert.deepEqual(deliveries.slice(start).map((item) => item.role), ["A", "B"]);
          assert.ok(deliveries.slice(start).every((item) => item.message.type === "controller_pairing_reserved"));
        }
      }
    }
    await router.handle({ socketIdentity: `socket-${role}`, message });
  }
  return { keyId, publicKey: signing.publicKey.export({ type: "spki", format: "der" }).subarray(-32).toString("base64"),
    deliveries: deliveries.splice(0), consumed, retries };
}

const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
try {
  for await (const line of lines) {
    assert.ok(line.length <= 128 * 1024);
    process.stdout.write(JSON.stringify(await command(JSON.parse(line))) + "\n");
  }
} finally {
  lines.close();
}
