---
name: webapp-testing
description: Start/reuse a local app, wait for readiness, and verify its HTTP surface with evidence. The built-in web tool (`web.run`) refuses loopback/private targets, so local-app flows are checked via bash+curl, not page driving.
invocation: model+user
---

# Webapp Testing

## When to use
Use to prove a local web app's flow works end to end, with evidence.

## Non-goals
- Do not claim success from unit tests alone.
- Do not infer success from status codes alone — record bodies and headers.

## Workflow
1. Start or reuse the local app.
2. Wait for readiness.
3. Verify over HTTP first: the built-in web tool (`web.run`) refuses loopback
   and private-network targets (including
   `localhost`/`127.0.0.1`) with no override, so local-app flows are exercised
   with `bash` + `curl` (on a sandbox network denial, retry once with
   `sandbox_permissions` escalation — the approval prompt asks the user;
   check status codes, response bodies, headers) — not by driving the page.
4. Record evidence of pass/fail: endpoint, request, response excerpt.
