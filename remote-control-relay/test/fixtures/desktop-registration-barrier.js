// Test-only preload: retain genuine enrollment authorization and hold its return.
// IPC barriers control ordering without replacing socket state or server behavior.
import { WebSocketServer } from "ws";
import { EnrollmentRegistry } from "../../enrollment-registry.js";

const pending = [];
const authorize = EnrollmentRegistry.prototype.authorizeDesktopRegistration;
EnrollmentRegistry.prototype.authorizeDesktopRegistration = async function (input) {
  const result = await authorize.call(this, input);
  await new Promise((resolve) => {
    pending.push(resolve);
    process.send({ type: "authorization-ready", endpointId: input.endpointId });
  });
  setImmediate(() => process.send({ type: "authorization-finished", endpointId: input.endpointId }));
  return result;
};
process.on("message", (message) => {
  if (message?.type === "release-registration") {
    for (const release of pending.splice(0)) release();
  }
});

const emit = WebSocketServer.prototype.emit;
WebSocketServer.prototype.emit = function (event, ...args) {
  const result = emit.call(this, event, ...args);
  if (event === "connection") {
    args[0].once("close", () => {
      setImmediate(() => process.send({ type: "socket-closed" }));
    });
  }
  return result;
};
