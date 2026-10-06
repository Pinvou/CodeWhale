---
name: webapp-testing
description: Start/reuse a local app, wait for readiness, and verify its HTTP surface with evidence. Browser automation refuses loopback/private targets, so local-app flows are checked via bash+curl, not page driving.
invocation: model+user
---

# Webapp Testing

## When to use
Use to prove a web app flow works in a real browser/runtime context.

## Non-goals
- Do not claim success from unit tests alone.
- Do not hardcode fragile selectors without observation.

## Workflow
1. Start or reuse the local app.
2. Wait for readiness.
3. Verify over HTTP first: the browser automation tool refuses loopback and
   private-network targets (including `localhost`/`127.0.0.1`) with no
   override, so local-app flows are exercised with `bash` + `curl` (status
   codes, response bodies, headers) — not by driving the page.
4. Record evidence of pass/fail: endpoint, request, response excerpt.
