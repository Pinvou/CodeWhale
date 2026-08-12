import { cleanupAttachments } from "./attachment-cleanup.js";
import { loadConfig } from "./config.js";
import { createPool } from "./db.js";

const config = loadConfig();
const pool = createPool(config);

try {
  const result = await cleanupAttachments(pool, config);
  process.stdout.write(`${JSON.stringify(result)}\n`);
} finally {
  await pool.end();
}
