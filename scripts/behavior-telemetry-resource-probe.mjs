#!/usr/bin/env node

import { execFileSync } from 'node:child_process';
import { cpus } from 'node:os';
import { chmodSync, readFileSync, writeFileSync } from 'node:fs';

const args = new Map();
for (let index = 2; index < process.argv.length; index += 2) {
  args.set(process.argv[index], process.argv[index + 1]);
}

const processId = Number(args.get('--pid') || process.pid);
const durationSeconds = Number(args.get('--duration') || 300);
const intervalMs = Number(args.get('--interval') || 5_000);
const outputPath = args.get('--output');
const baselinePath = args.get('--baseline');
if (!Number.isSafeInteger(processId) || processId <= 0 || !outputPath
  || !Number.isFinite(durationSeconds) || durationSeconds <= 0
  || !Number.isFinite(intervalMs) || intervalMs < 100) {
  console.error('usage: node scripts/behavior-telemetry-resource-probe.mjs [--pid <pid>] --output <json> [--duration 300] [--interval 5000] [--baseline <json>]');
  process.exit(2);
}

function parsePsCpuTime(value) {
  const [dayPart, clockPart] = value.trim().includes('-')
    ? value.trim().split('-')
    : ['0', value.trim()];
  const units = clockPart.split(':').map(Number);
  const seconds = units.pop() || 0;
  const minutes = units.pop() || 0;
  const hours = units.pop() || 0;
  return Number(dayPart) * 86_400 + hours * 3_600 + minutes * 60 + seconds;
}

function sample(pid) {
  if (process.platform === 'win32') {
    const command = `$p=Get-Process -Id ${pid} -ErrorAction Stop; @{cpu=$p.TotalProcessorTime.TotalSeconds;rss=$p.WorkingSet64}|ConvertTo-Json -Compress`;
    return JSON.parse(execFileSync('powershell.exe', [
      '-NoProfile', '-NonInteractive', '-Command', command,
    ], { encoding: 'utf8', windowsHide: true }));
  }
  const output = execFileSync('ps', ['-o', 'time=,rss=', '-p', String(pid)], { encoding: 'utf8' }).trim();
  const match = output.match(/^(\S+)\s+(\d+)$/);
  if (!match) throw new Error(`cannot sample process ${pid}`);
  return { cpu: parsePsCpuTime(match[1]), rss: Number(match[2]) * 1024 };
}

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const startedAt = Date.now();
const first = sample(processId);
const memorySamples = [first.rss];
while (Date.now() - startedAt < durationSeconds * 1_000) {
  await sleep(Math.min(intervalMs, Math.max(100, durationSeconds * 1_000 - (Date.now() - startedAt))));
  memorySamples.push(sample(processId).rss);
}
const last = sample(processId);
memorySamples.push(last.rss);
const elapsedSeconds = Math.max(0.001, (Date.now() - startedAt) / 1_000);
const averageCpuPercent = Math.max(0, last.cpu - first.cpu) / elapsedSeconds / Math.max(1, cpus().length) * 100;
const averageWorkingSetMiB = memorySamples.reduce((sum, value) => sum + value, 0)
  / memorySamples.length / 1024 / 1024;
const result = {
  schema_version: 1,
  process_id: processId,
  platform: process.platform,
  measured_at: Date.now(),
  duration_seconds: elapsedSeconds,
  sample_count: memorySamples.length,
  average_cpu_percent: averageCpuPercent,
  average_working_set_mib: averageWorkingSetMiB,
  peak_working_set_mib: Math.max(...memorySamples) / 1024 / 1024,
};

if (baselinePath) {
  const baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
  result.baseline = baselinePath;
  result.cpu_increment_percent = averageCpuPercent - Number(baseline.average_cpu_percent);
  result.memory_increment_mib = averageWorkingSetMiB - Number(baseline.average_working_set_mib);
  result.budget = { cpu_increment_percent: 0.5, memory_increment_mib: 16 };
  result.passed = result.cpu_increment_percent <= result.budget.cpu_increment_percent
    && result.memory_increment_mib <= result.budget.memory_increment_mib;
}

writeFileSync(outputPath, `${JSON.stringify(result, null, 2)}\n`, { mode: 0o600 });
chmodSync(outputPath, 0o600);
console.log(JSON.stringify(result, null, 2));
if (result.passed === false) process.exitCode = 1;
