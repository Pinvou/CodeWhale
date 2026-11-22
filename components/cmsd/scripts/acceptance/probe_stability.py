#!/usr/bin/env python3
"""Opt-in loopback inference endurance probe. Requires a real running model."""
import argparse
import json
import statistics
import subprocess
import time
import urllib.parse
import urllib.request
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', required=True)
    parser.add_argument('--model', required=True)
    parser.add_argument('--seconds', type=float, default=600)
    parser.add_argument('--interval', type=float, default=10)
    parser.add_argument('--container')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    parsed = urllib.parse.urlsplit(args.base_url)
    if parsed.scheme != 'http' or parsed.hostname not in ('127.0.0.1', 'localhost') or parsed.username:
        parser.error('use an explicit local loopback HTTP endpoint')
    if args.seconds <= 0 or args.interval < 1 or args.output.exists():
        parser.error('positive duration, interval >= 1, and a new output file are required')
    started = time.monotonic()
    samples = []
    report = {'started_at': time.time(), 'model': args.model, 'base_url': args.base_url,
              'samples': samples}
    while True:
        stream = len(samples) % 2 == 1
        sample = {'at': time.time(), 'stream': stream}
        before = time.monotonic()
        try:
            request = urllib.request.Request(args.base_url.rstrip('/')+'/chat/completions',
                data=json.dumps({'model': args.model, 'messages': [{'role': 'user',
                    'content': 'Write one short sentence about a computer.'}], 'max_tokens': 24,
                    'temperature': 0, 'stream': stream}).encode(), headers={'Content-Type': 'application/json'})
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = response.read().decode()
            if stream:
                events = [json.loads(line[6:]) for line in payload.splitlines()
                          if line.startswith('data: ') and line != 'data: [DONE]']
                content = ''.join(event['choices'][0].get('delta', {}).get('content', '') for event in events)
                assert 'data: [DONE]' in payload
            else:
                content = json.loads(payload)['choices'][0]['message']['content']
            assert content.strip(), 'empty model response'
            sample.update(ok=True, content=content)
        except Exception as error:
            sample.update(ok=False, error=str(error))
        sample['seconds'] = time.monotonic()-before
        if args.container:
            try:
                details = json.loads(subprocess.check_output(['docker', 'inspect', args.container], text=True))[0]
                sample['pid'] = details['State']['Pid']
                sample['restart_count'] = details['RestartCount']
                sample['container_running'] = details['State']['Running']
                status = Path('/proc')/str(sample['pid'])/'status'
                sample['rss_kib'] = int(next(line.split()[1] for line in status.read_text().splitlines()
                    if line.startswith('VmRSS:')))
            except Exception as error:
                sample['observation_error'] = str(error)
        samples.append(sample)
        report.update(duration_seconds=time.monotonic()-started,
                      successful=sum(row['ok'] for row in samples), failures=sum(not row['ok'] for row in samples))
        latencies = [row['seconds'] for row in samples if row['ok']]
        if latencies:
            report['latency_median_seconds'] = statistics.median(latencies)
            report['latency_max_seconds'] = max(latencies)
        temporary = args.output.with_suffix('.tmp')
        temporary.write_text(json.dumps(report, indent=2))
        temporary.replace(args.output)
        if report['duration_seconds'] >= args.seconds:
            break
        time.sleep(min(args.interval, args.seconds-report['duration_seconds']))
    print(json.dumps({key: value for key, value in report.items() if key != 'samples'}))
    return 1 if report['failures'] else 0


if __name__ == '__main__':
    raise SystemExit(main())
