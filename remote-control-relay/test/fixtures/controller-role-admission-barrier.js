import { ControllerLeaseManager } from "../../controller-lease.js";
import { ControllerRouter } from "../../controller-router.js";
import { EnrollmentRegistry } from "../../enrollment-registry.js";

let release;
let matchingRegistrations = 0;
async function pause(type, id) {
  await new Promise((resolve) => {
    release = resolve;
    process.send({ type, endpointId: id });
  });
}

const authorize = EnrollmentRegistry.prototype.authorizeDesktopRegistration;
EnrollmentRegistry.prototype.authorizeDesktopRegistration = async function (input) {
  const result = await authorize.call(this, input);
  if (input.endpointId === process.env.PINVOU_TEST_PAUSE_ROLE_ENDPOINT
    && matchingRegistrations++ >= Number(process.env.PINVOU_TEST_PAUSE_ROLE_AFTER || 0)) {
    await pause("role-registration-ready", input.endpointId);
  }
  return result;
};

if (process.env.PINVOU_TEST_ROLE_RESUME_REQUEST) {
  const endpoints = new Map();
  const authenticate = ControllerRouter.prototype.authenticateControlled;
  ControllerRouter.prototype.authenticateControlled = function (input) {
    const result = authenticate.call(this, input);
    endpoints.set(input.socketIdentity, input.endpointId);
    return result;
  };
  const handle = ControllerRouter.prototype.handle;
  ControllerRouter.prototype.handle = function (input) {
    const requestId = input.message?.request_id;
    if (requestId === process.env.PINVOU_TEST_ROLE_RESUME_REQUEST) {
      process.send({ type: "role-resume-seen" });
    }
    const result = handle.call(this, input);
    if (input.message?.type === "controller_resume_prepare") {
      void result.then(
        (outcome) => process.send({ type: "role-resume-finished", endpointId: requestId, deliveries: outcome?.deliveries?.length ?? 0 }),
        (error) => process.send({ type: "role-resume-finished", endpointId: requestId, error: error?.code }),
      );
    }
    return result;
  };
  const remove = ControllerRouter.prototype.removeSocket;
  ControllerRouter.prototype.removeSocket = async function (socketIdentity) {
    const result = await remove.call(this, socketIdentity);
    const endpointId = endpoints.get(socketIdentity);
    endpoints.delete(socketIdentity);
    if (endpointId) process.send({ type: "role-socket-removed", endpointId });
    return result;
  };
}

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
