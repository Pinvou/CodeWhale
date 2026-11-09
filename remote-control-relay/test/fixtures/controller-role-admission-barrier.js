import { ControllerLeaseManager } from "../../controller-lease.js";
import { EnrollmentRegistry } from "../../enrollment-registry.js";

let release;
async function pause(type, id) {
  await new Promise((resolve) => {
    release = resolve;
    process.send({ type, endpointId: id });
  });
}

const authorize = EnrollmentRegistry.prototype.authorizeDesktopRegistration;
EnrollmentRegistry.prototype.authorizeDesktopRegistration = async function (input) {
  const result = await authorize.call(this, input);
  if (input.endpointId === process.env.PINVOU_TEST_PAUSE_ROLE_ENDPOINT) {
    await pause("role-registration-ready", input.endpointId);
  }
  return result;
};

const claim = ControllerLeaseManager.prototype.claimForDelivery;
ControllerLeaseManager.prototype.claimForDelivery = async function (input) {
  const result = await claim.call(this, input);
  if (result.owns_initial_delivery && input.message.claim_request_id === process.env.PINVOU_TEST_PAUSE_ROLE_CLAIM) {
    await pause("role-claim-ready", input.message.claim_request_id);
  }
  return result;
};

process.on("message", (message) => {
  if (message?.type !== "release-role-admission") return;
  const pending = release;
  release = null;
  pending?.();
});
