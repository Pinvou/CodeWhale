# Live npm audit in CI

The Hub and update console use `python3 scripts/ci-npm-audit.py --prefix <project>`
on Linux. The helper queries the public npm registry with the existing `high`
severity threshold and validates a version-2 audit report. It never substitutes
a cached advisory result, disables audit or permits high/critical findings.

When an HTTPS/HTTP proxy is configured, only the audit child receives a loopback
`NO_PROXY` list. This keeps public advisory traffic on the configured egress
instead of inheriting a public-registry bypass intended for another environment.
The parent environment, package installation and local cache remain unchanged;
hosted runners without a configured proxy use their ordinary direct route.

npm's fetch layer does not replay audit POST requests automatically. The helper
rebuilds the entire read-only audit request only for explicitly classified
transient transport errors, rate limiting or transient server errors. It allows
at most three attempts within 60 seconds, with a 15-second fetch timeout and a
20-second process cap per attempt. A timeout retires that attempt's Linux process
group. This bounds recovery inside the existing job budget rather than extending
the workflow timeout.

High/critical findings, authentication errors, HTTP 400, malformed/missing audit
data and exhausted network recovery all fail the gate. Valid reports with a
nonzero npm exit also remain failures. Logs contain attempt counts, timings,
bounded error codes and vulnerability counts; network stderr/headers are not
forwarded. Actual vulnerability reports remain visible for remediation.

A successful retry confirms a completed live audit, not the cause of the earlier
network outage. Preserve runner/network evidence when investigating an incident.
