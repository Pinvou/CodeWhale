import { loadConfig } from "./config.js";
import { createPool } from "./db.js";
import { createHubServer } from "./http.js";

const config = loadConfig();
const pool = createPool(config);
pool.on("error", (error) => {
  process.stderr.write(`Unexpected database pool error (${error.code || "unknown"}).\n`);
});
const server = createHubServer({ pool, config });

server.requestTimeout = 30_000;
server.headersTimeout = 35_000;
server.keepAliveTimeout = 5_000;

await new Promise((resolve, reject) => {
  server.once("error", reject);
  server.listen(config.port, config.host, resolve);
});

const address = server.address();
process.stdout.write(
  `Team Relay Hub listening on ${typeof address === "string" ? address : `${address.address}:${address.port}`}\n`,
);

async function shutdown(signal) {
  process.stdout.write(`Received ${signal}; shutting down.\n`);
  server.closeEventStreams();
  await new Promise((resolve) => server.close(resolve));
  await pool.end();
  process.exit(0);
}

process.once("SIGINT", () => void shutdown("SIGINT"));
process.once("SIGTERM", () => void shutdown("SIGTERM"));
