import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { loadConfig } from "./config.js";
import { createPool } from "./db.js";

const config = loadConfig();
const pool = createPool(config);

try {
  const migrationFiles = (await readdir(config.migrationDir))
    .filter((name) => /^\d{4}_[a-z0-9_]+\.sql$/.test(name))
    .sort();
  if (migrationFiles.length === 0) {
    throw new Error(`No migrations found in ${config.migrationDir}`);
  }
  const migrationTable = await pool.query(
    `SELECT EXISTS (
       SELECT 1
         FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'team_relay_schema_migrations'
     ) AS table_exists`,
  );
  const appliedVersions = new Set();
  if (migrationTable.rows[0].table_exists) {
    const applied = await pool.query("SELECT version FROM team_relay_schema_migrations");
    for (const row of applied.rows) appliedVersions.add(row.version);
  }
  for (const name of migrationFiles) {
    const version = name.replace(/\.sql$/, "");
    if (appliedVersions.has(version)) {
      process.stdout.write(`Migration ${version} already applied.\n`);
      continue;
    }
    const sql = await readFile(join(config.migrationDir, name), "utf8");
    await pool.query(sql);
    process.stdout.write(`Applied ${version}.\n`);
  }
} finally {
  await pool.end();
}
