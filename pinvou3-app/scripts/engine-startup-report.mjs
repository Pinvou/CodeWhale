#!/usr/bin/env node

import { readFile, readdir, stat } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const KINDS = ['cold', 'rebuilt', 'reused'];
const STAGES = [
  'runtime_lock_ms',
  'prepare_model_ms',
  'reclaim_ms',
  'finalize_bridge_ms',
  'tool_setup_ms',
  'materialize_skills_ms',
  'spawn_engine_ms',
  'load_session_ms',
  'sync_session_ms',
];

function percentile(values, quantile) {
  if (!values.length) return null;
  const sorted = [...values].sort((left, right) => left - right);
  const index = Math.max(0, Math.ceil(sorted.length * quantile) - 1);
  return sorted[index];
}

function distribution(values) {
  const finite = values.filter(Number.isFinite);
  return {
    count: finite.length,
    p50_ms: percentile(finite, 0.5),
    p95_ms: percentile(finite, 0.95),
    max_ms: finite.length ? Math.max(...finite) : null,
  };
}

function optionalNumber(value) {
  return value == null ? null : Number(value);
}

async function timingFiles(input) {
  const info = await stat(input);
  if (info.isFile()) return [input];
  const entries = await readdir(input, { withFileTypes: true });
  const direct = entries.find(entry => entry.isFile() && entry.name === 'timing_events.jsonl');
  if (direct) return [path.join(input, direct.name)];
  return entries
    .filter(entry => entry.isDirectory())
    .map(entry => path.join(input, entry.name, 'timing_events.jsonl'));
}

async function existingTimingFiles(input) {
  const candidates = await timingFiles(input);
  const checks = await Promise.all(candidates.map(async file => {
    try {
      return (await stat(file)).isFile() ? file : null;
    } catch {
      return null;
    }
  }));
  return checks.filter(Boolean);
}

function ingestFile(text, source, turns) {
  for (const line of text.split(/\r?\n/u)) {
    if (!line.trim()) continue;
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (!event?.turn_id || !event?.event) continue;
    const key = `${source}\0${event.turn_id}`;
    const turn = turns.get(key) || {};
    if (event.event === 'user_start') turn.userStart = Number(event.timestamp);
    if (event.event === 'assistant_done' && event.engine_startup?.engine_acquire) {
      turn.engineReady = optionalNumber(event.engine_startup.engine_ready_timestamp);
      turn.firstOutput = optionalNumber(event.engine_startup.first_output_timestamp);
      turn.acquire = event.engine_startup.engine_acquire;
    }
    turns.set(key, turn);
  }
}

export async function collectEngineStartupSamples(input) {
  const files = await existingTimingFiles(input);
  const turns = new Map();
  await Promise.all(files.map(async (file, index) => {
    ingestFile(await readFile(file, 'utf8'), index, turns);
  }));
  const samples = [];
  for (const turn of turns.values()) {
    const kind = turn.acquire?.kind;
    if (!KINDS.includes(kind) || !Number.isFinite(Number(turn.acquire.total_ms))) continue;
    samples.push({
      kind,
      total_ms: Number(turn.acquire.total_ms),
      send_to_ready_ms: Number.isFinite(turn.userStart) && Number.isFinite(turn.engineReady)
        ? Math.max(0, turn.engineReady - turn.userStart)
        : null,
      ready_to_first_output_ms: Number.isFinite(turn.engineReady) && Number.isFinite(turn.firstOutput)
        ? Math.max(0, turn.firstOutput - turn.engineReady)
        : null,
      send_to_first_output_ms: Number.isFinite(turn.userStart) && Number.isFinite(turn.firstOutput)
        ? Math.max(0, turn.firstOutput - turn.userStart)
        : null,
      stages: Object.fromEntries(STAGES.map(stage => [stage, Number(turn.acquire[stage]) || 0])),
    });
  }
  return samples;
}

export function summarizeEngineStartup(samples, { minSamples = 30, coldP95ThresholdMs = 300 } = {}) {
  const groups = {};
  for (const kind of KINDS) {
    const selected = samples.filter(sample => sample.kind === kind);
    groups[kind] = {
      acquisition: distribution(selected.map(sample => sample.total_ms)),
      send_to_ready: distribution(selected.map(sample => sample.send_to_ready_ms)),
      ready_to_first_output: distribution(selected.map(sample => sample.ready_to_first_output_ms)),
      send_to_first_output: distribution(selected.map(sample => sample.send_to_first_output_ms)),
      stages: Object.fromEntries(STAGES.map(stage => [stage, distribution(selected.map(sample => sample.stages[stage]))])),
    };
  }
  const enoughSamples = groups.cold.acquisition.count >= minSamples
    && groups.reused.acquisition.count >= minSamples;
  const coldP95 = groups.cold.acquisition.p95_ms;
  return {
    criteria: { min_samples_per_group: minSamples, cold_p95_threshold_ms: coldP95ThresholdMs },
    groups,
    verdict: !enoughSamples
      ? 'insufficient_samples'
      : coldP95 > coldP95ThresholdMs
        ? 'cold_start_issue_confirmed'
        : 'cold_start_within_threshold',
  };
}

function cell(value) {
  return value == null ? '-' : String(value);
}

export function formatEngineStartupReport(report) {
  const lines = [
    'Engine 启动观测（毫秒）',
    '类型\t样本\t获取 P50\t获取 P95\t发送→就绪 P95\t发送→首输出 P95',
  ];
  for (const kind of KINDS) {
    const group = report.groups[kind];
    lines.push([
      kind,
      group.acquisition.count,
      cell(group.acquisition.p50_ms),
      cell(group.acquisition.p95_ms),
      cell(group.send_to_ready.p95_ms),
      cell(group.send_to_first_output.p95_ms),
    ].join('\t'));
  }
  const verdicts = {
    insufficient_samples: `证据不足：cold 与 reused 各需要至少 ${report.criteria.min_samples_per_group} 个样本。`,
    cold_start_issue_confirmed: `问题成立：cold 获取 P95 超过 ${report.criteria.cold_p95_threshold_ms} ms。`,
    cold_start_within_threshold: `无需改动：cold 获取 P95 未超过 ${report.criteria.cold_p95_threshold_ms} ms。`,
  };
  lines.push(verdicts[report.verdict]);
  return lines.join('\n');
}

function parseArgs(argv) {
  const options = {
    input: path.join(process.env.PINVOU3_HOME || path.join(os.homedir(), '.pinvou3'), 'sessions'),
    minSamples: 30,
    coldP95ThresholdMs: 300,
    json: false,
  };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === '--input') options.input = argv[++index];
    else if (arg === '--min-samples') options.minSamples = Number(argv[++index]);
    else if (arg === '--cold-p95-ms') options.coldP95ThresholdMs = Number(argv[++index]);
    else if (arg === '--json') options.json = true;
    else throw new Error(`未知参数: ${arg}`);
  }
  return options;
}

async function main() {
  const options = parseArgs(process.argv.slice(2));
  const samples = await collectEngineStartupSamples(options.input);
  const report = summarizeEngineStartup(samples, options);
  process.stdout.write(`${options.json ? JSON.stringify(report, null, 2) : formatEngineStartupReport(report)}\n`);
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(error => {
    process.stderr.write(`Engine 启动报告生成失败: ${error.message}\n`);
    process.exitCode = 1;
  });
}
