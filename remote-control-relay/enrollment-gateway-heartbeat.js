import { WebSocket } from "ws";

export const DEFAULT_GATEWAY_HEARTBEAT_INTERVAL_MS = 15_000;

export function validateGatewayHeartbeatInterval(intervalMs) {
  if (!Number.isSafeInteger(intervalMs) || intervalMs < 100 || intervalMs > 60_000) {
    const error = new Error("gateway_heartbeat_interval_invalid");
    error.code = "gateway_heartbeat_interval_invalid";
    throw error;
  }
  return intervalMs;
}

// Start only after enrollment authorization and the upstream upgrade succeed.
// A transport pong never refreshes credentials, task state or business liveness.
export function startGatewayHeartbeat(client, intervalMs) {
  validateGatewayHeartbeatInterval(intervalMs);
  if (client.readyState !== WebSocket.OPEN) return () => {};
  let alive = true;
  let stopped = false;
  const pong = () => { alive = true; };
  const stop = () => {
    if (stopped) return;
    stopped = true;
    clearInterval(timer);
    client.off("pong", pong);
    client.off("close", stop);
  };
  const timer = setInterval(() => {
    if (client.readyState !== WebSocket.OPEN) { stop(); return; }
    if (!alive) { stop(); client.terminate(); return; }
    alive = false;
    try { client.ping(); }
    catch { stop(); client.terminate(); }
  }, intervalMs);
  timer.unref?.();
  client.on("pong", pong);
  client.once("close", stop);
  return stop;
}
