import { isDirectCliEntry } from "../cli-entrypoint.js";
import WebSocket from "ws";
import { CONTROLLER_SUBPROTOCOL } from "../protocol-controller.js";

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

export function validateControllerRouteSmokeResponse(message) {
  if (message?.v !== 1 || message?.type !== "error" || message?.code !== "invalid_message") {
    fail("controller_route_smoke_response_invalid");
  }
}

export async function smokeControllerRoute(url) {
  await new Promise((resolveSmoke, rejectSmoke) => {
    const socket = new WebSocket(url, CONTROLLER_SUBPROTOCOL);
    let settled = false;
    let timer;
    const finish = (error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        if (socket.readyState === WebSocket.OPEN) socket.close();
        else if (socket.readyState !== WebSocket.CLOSED) socket.terminate();
      } catch {
        // Cleanup cannot change the already determined smoke-test result.
      }
      if (error) rejectSmoke(error);
      else resolveSmoke();
    };
    timer = setTimeout(() => finish(new Error("controller_route_smoke_timeout")), 5000);
    socket.once("open", () => {
      if (socket.protocol !== CONTROLLER_SUBPROTOCOL) {
        finish(new Error("controller_route_subprotocol_missing"));
        return;
      }
      // This deliberately malformed registration is non-mutating. A live controller-v1
      // router returns its bounded v1 invalid_message; a disabled/legacy lane returns v2.
      socket.send(JSON.stringify({
        v: 1,
        type: "desktop_pairing_register",
        request_id: "request-deploy-controller-smoke",
      }));
    });
    socket.once("message", (raw) => {
      try {
        const response = JSON.parse(String(raw));
        validateControllerRouteSmokeResponse(response);
        finish();
      } catch (error) {
        finish(error);
      }
    });
    socket.once("error", (error) => finish(error));
    socket.once("close", () => {
      if (!settled) finish(new Error("controller_route_smoke_closed"));
    });
  });
}

if (isDirectCliEntry(import.meta.url)) {
  smokeControllerRoute(process.argv[2] || "ws://127.0.0.1:8787/pinvou3/remote/ws")
    .catch(() => {
      // WebSocket errors can include the supplied URL, credentials or local paths.
      console.error("controller_route_smoke_failed");
      process.exitCode = 1;
    });
}
