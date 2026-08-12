import { loadConfig } from "./config.js";
import { createPool } from "./db.js";
import { createTeamWithOwnerInvite } from "./identity-service.js";

function argument(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

const name = argument("--name");
if (!name) {
  process.stderr.write("Usage: npm run team:create -- --name <team-name> [--expires-hours 24]\n");
  process.exit(2);
}

const expiresInHours = Number(argument("--expires-hours") || 24);
const config = loadConfig();
const pool = createPool(config);

try {
  const result = await createTeamWithOwnerInvite(pool, { name, expiresInHours });
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
} finally {
  await pool.end();
}
