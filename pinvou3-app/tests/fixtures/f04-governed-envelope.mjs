// Controlled cross-runtime fixture: call the actual Portal envelope producer.
import { governedTokenEnvelope } from '../../src/features/account-platform/admin-high-risk-action-state.js';
let input = '';
for await (const chunk of process.stdin) input += chunk;
process.stdout.write(JSON.stringify(await governedTokenEnvelope(JSON.parse(input))));
