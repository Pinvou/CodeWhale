// Test-only preload: a transient failure leaves the live manager invalidating.
// No production timers, deadlines or state transitions are replaced.
import { PairingStore } from "../../pairing-store.js";

const terminate = PairingStore.prototype.terminate;
let attempted = false;
PairingStore.prototype.terminate = async function (input) {
  if (!attempted) {
    attempted = true;
    process.stdout.write("reservation-invalidation-probe: failed\n");
    throw new Error("injected transient invalidation failure");
  }
  const result = await terminate.call(this, input);
  process.stdout.write("reservation-invalidation-probe: retried\n");
  return result;
};
