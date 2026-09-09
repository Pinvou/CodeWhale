#!/usr/bin/env node
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const require = createRequire(new URL('../account-platform/package.json', import.meta.url));
const YAML = require('yaml');
const expected = ['--encoding=UTF8', '--locale=C', '--auth-local=scram-sha-256', '--auth-host=scram-sha-256'];

function check(ok, code) { if (!ok) throw new Error(`production_postgres_${code}`); }

export function checkProductionPostgres(config) {
  const raw = config?.services?.postgres?.environment?.POSTGRES_INITDB_ARGS;
  check(typeof raw === 'string' && raw.trim() === raw && !/[\x00-\x20]/.test(raw.replaceAll(' ', '')), 'initdb_args_invalid');
  const args = raw.split(' ');
  check(args.length === expected.length && args.every((value, index) => value === expected[index]), 'initdb_args_invalid');
  return { decision: 'POSTGRES_CONFIG_VALID', encoding: 'UTF8', locale: 'C', environment_status: 'ENV_PENDING' };
}

async function main() {
  const argument = process.argv[2];
  check(process.argv.length <= 3, 'arguments_invalid');
  const source = argument === '-'
    ? await new Promise((resolve, reject) => {
      let data = ''; process.stdin.setEncoding('utf8'); process.stdin.on('data', chunk => { data += chunk; });
      process.stdin.on('end', () => resolve(data)); process.stdin.on('error', reject);
    })
    : await readFile(argument ? path.resolve(argument) : new URL('../account-platform/deploy/production/compose.dependencies.yaml', import.meta.url), 'utf8');
  check(source.length > 0 && source.length < 1024 * 1024, 'input_invalid');
  const config = YAML.parse(source);
  process.stdout.write(`${JSON.stringify(checkProductionPostgres(config))}\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(() => { process.stderr.write('PRODUCTION_POSTGRES_CONFIG_REJECTED\n'); process.exitCode = 1; });
}
